package ledger

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// fakeSettlementConn is a Conn that resolves the 2 settlement accounts
// exactly like fakeCaptureConn resolves capture accounts, plus answers the
// 3 kinds of QueryRowContext call PostSettlementClearing issues against
// journal_entries directly (not through an account-code lookup):
//
//   - the idempotency check (sumEntries, exact transaction_ref match,
//     against the settlement_clearing account) -- args[1] is a string.
//   - ResolveSettlementAmount's "captured before cutoff" sum (against the
//     merchant_payable account) -- args[1] is a time.Time.
//   - ResolveSettlementAmount's "already cleared" sum (sumEntries, LIKE
//     match, against the merchant_payable account) -- args[1] is a string
//     and the query text contains "LIKE".
type fakeSettlementConn struct {
	accounts map[string]Account

	alreadyPostedForRef map[int64]int64 // keyed by settlement_clearing account id
	capturedTotal       map[int64]int64 // keyed by merchant_payable account id
	alreadyClearedTotal map[int64]int64 // keyed by merchant_payable account id (positive)

	tx         *fakeTx
	beginErr   error
	beginCalls int
}

func (f *fakeSettlementConn) QueryRowContext(_ context.Context, query string, args ...any) Row {
	if code, ok := args[0].(string); ok {
		if a, found := f.accounts[code]; found {
			return fakeRow{scan: func(dest ...any) error {
				*dest[0].(*int64) = a.ID
				*dest[1].(*string) = a.Code
				*dest[2].(*string) = string(a.Type)
				*dest[3].(*string) = a.Name
				*dest[4].(*time.Time) = a.CreatedAt
				return nil
			}}
		}
	}

	accountID, _ := args[0].(int64)
	var sum int64
	switch args[1].(type) {
	case time.Time:
		sum = f.capturedTotal[accountID]
	default:
		if containsLike(query) {
			sum = f.alreadyClearedTotal[accountID]
		} else {
			sum = f.alreadyPostedForRef[accountID]
		}
	}
	return fakeRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = sum
		return nil
	}}
}

func (f *fakeSettlementConn) BeginTx(context.Context, *sql.TxOptions) (Tx, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.tx, nil
}

// settlementTestConn returns a fakeSettlementConn pre-seeded with the
// merchant_payable and settlement_clearing accounts merchantID needs,
// plus capturedTotal/alreadyCleared as ResolveSettlementAmount's inputs
// (alreadyCleared is the plain, positive "amount already cleared"; it is
// stored negated, matching what a real settlement clearing debit leg --
// sumEntries over negative amounts -- would sum to) and alreadyPosted as
// what the idempotency check should find for this merchant's
// settlement_clearing leg.
func settlementTestConn(merchantID string, capturedTotal, alreadyCleared, alreadyPosted int64) *fakeSettlementConn {
	const (
		payableID  = int64(3)
		clearingID = int64(4)
	)
	return &fakeSettlementConn{
		tx: &fakeTx{},
		accounts: map[string]Account{
			SettlementClearingAccountCode: {ID: clearingID, Code: SettlementClearingAccountCode, Type: AssetAccount},
			"merchant_payable:" + merchantID: {
				ID: payableID, Code: "merchant_payable:" + merchantID, Type: LiabilityAccount,
			},
		},
		capturedTotal:       map[int64]int64{payableID: capturedTotal},
		alreadyClearedTotal: map[int64]int64{payableID: -alreadyCleared},
		alreadyPostedForRef: map[int64]int64{clearingID: alreadyPosted},
	}
}

func TestSettlementTransactionRefFormat(t *testing.T) {
	ref, err := SettlementTransactionRef("batch_1", "mch_1")
	if err != nil {
		t.Fatalf("SettlementTransactionRef() returned error: %v", err)
	}
	if want := "settlement:batch_1:mch_1"; ref != want {
		t.Errorf("SettlementTransactionRef() = %q, want %q", ref, want)
	}
}

func TestSettlementTransactionRefRejectsEmptyArgs(t *testing.T) {
	if _, err := SettlementTransactionRef("", "mch_1"); err == nil {
		t.Error("SettlementTransactionRef(\"\", ...) = nil error, want an error")
	}
	if _, err := SettlementTransactionRef("batch_1", ""); err == nil {
		t.Error("SettlementTransactionRef(..., \"\") = nil error, want an error")
	}
}

func TestSettlementTransactionRefIsStableAcrossCalls(t *testing.T) {
	// PostSettlementClearing's idempotency check relies on this being a
	// pure function of its inputs: calling it twice for the same batch and
	// merchant must produce the exact same string every time.
	a, err := SettlementTransactionRef("batch_1", "mch_1")
	if err != nil {
		t.Fatalf("SettlementTransactionRef() returned error: %v", err)
	}
	b, err := SettlementTransactionRef("batch_1", "mch_1")
	if err != nil {
		t.Fatalf("SettlementTransactionRef() returned error: %v", err)
	}
	if a != b {
		t.Errorf("SettlementTransactionRef() = %q then %q, want the same string both times", a, b)
	}
}

func TestBuildSettlementClearingEntriesProducesTwoBalancedLegs(t *testing.T) {
	accounts := SettlementClearingAccounts{
		MerchantPayable:    Account{ID: 1},
		SettlementClearing: Account{ID: 2},
	}

	entries, err := BuildSettlementClearingEntries(accounts, 50000, "INR")
	if err != nil {
		t.Fatalf("BuildSettlementClearingEntries() returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("BuildSettlementClearingEntries() returned %d entries, want 2", len(entries))
	}

	byAccount := map[int64]int64{}
	var sum int64
	for _, e := range entries {
		if e.Currency != "INR" {
			t.Errorf("entry for account %d has currency %q, want INR", e.AccountID, e.Currency)
		}
		byAccount[e.AccountID] = e.Amount
		sum += e.Amount
	}
	if sum != 0 {
		t.Errorf("entries sum to %d, want 0 (balanced)", sum)
	}
	if got := byAccount[1]; got != -50000 {
		t.Errorf("merchant_payable leg = %d, want -50000", got)
	}
	if got := byAccount[2]; got != 50000 {
		t.Errorf("settlement_clearing leg = %d, want 50000", got)
	}
}

func TestBuildSettlementClearingEntriesRejectsNonPositiveAmount(t *testing.T) {
	accounts := SettlementClearingAccounts{MerchantPayable: Account{ID: 1}, SettlementClearing: Account{ID: 2}}
	for _, amount := range []int64{0, -1, -50000} {
		if _, err := BuildSettlementClearingEntries(accounts, amount, "INR"); err == nil {
			t.Errorf("BuildSettlementClearingEntries(amount=%d) = nil error, want an error", amount)
		}
	}
}

func TestBuildSettlementClearingEntriesRejectsInvalidCurrency(t *testing.T) {
	accounts := SettlementClearingAccounts{MerchantPayable: Account{ID: 1}, SettlementClearing: Account{ID: 2}}
	if _, err := BuildSettlementClearingEntries(accounts, 50000, "inr"); err == nil {
		t.Error("BuildSettlementClearingEntries(currency=\"inr\") = nil error, want an error")
	}
}

func TestResolveSettlementAmountSubtractsAlreadyCleared(t *testing.T) {
	conn := settlementTestConn("mch_1", 100000, 60000, 0)
	got, err := ResolveSettlementAmount(context.Background(), conn, 3, "mch_1", time.Now())
	if err != nil {
		t.Fatalf("ResolveSettlementAmount() returned error: %v", err)
	}
	if want := int64(40000); got != want {
		t.Errorf("ResolveSettlementAmount() = %d, want %d (100000 captured minus 60000 already cleared)", got, want)
	}
}

func TestResolveSettlementAmountNeverNegative(t *testing.T) {
	// Defensive: already-cleared somehow exceeding captured (should not
	// happen in practice) must not surface as a negative amount to settle.
	conn := settlementTestConn("mch_1", 50000, 70000, 0)
	got, err := ResolveSettlementAmount(context.Background(), conn, 3, "mch_1", time.Now())
	if err != nil {
		t.Fatalf("ResolveSettlementAmount() returned error: %v", err)
	}
	if got != 0 {
		t.Errorf("ResolveSettlementAmount() = %d, want 0", got)
	}
}

func TestPostSettlementClearingPostsEligibleAmountAndCommits(t *testing.T) {
	conn := settlementTestConn("mch_1", 100000, 0, 0)

	cleared, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing() returned error: %v", err)
	}
	if cleared != 100000 {
		t.Errorf("PostSettlementClearing() = %d, want 100000", cleared)
	}
	if conn.beginCalls != 1 {
		t.Errorf("BeginTx called %d times, want 1", conn.beginCalls)
	}
	if !conn.tx.committed {
		t.Error("PostSettlementClearing did not commit")
	}
	if len(conn.tx.execCalls) != 2 {
		t.Fatalf("ExecContext called %d times, want 2 (merchant_payable debit, settlement_clearing credit)", len(conn.tx.execCalls))
	}

	wantRef := "settlement:batch_1:mch_1"
	for _, call := range conn.tx.execCalls {
		if call.args[0] != wantRef {
			t.Errorf("entry transaction_ref = %v, want %q", call.args[0], wantRef)
		}
	}
}

func TestPostSettlementClearingIsIdempotentWhenAlreadyPosted(t *testing.T) {
	// alreadyPostedForRef is non-zero -- as if a prior call for this exact
	// batch and merchant already posted and committed. The captured/cleared
	// totals are set up so that, if PostSettlementClearing recomputed and
	// reposted instead of short-circuiting, it would double-post.
	conn := settlementTestConn("mch_1", 100000, 0, 100000)

	cleared, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing() returned error: %v", err)
	}
	if cleared != 100000 {
		t.Errorf("PostSettlementClearing() = %d, want 100000 (the amount already posted)", cleared)
	}
	if conn.beginCalls != 0 {
		t.Errorf("BeginTx called %d times, want 0 (already-posted batch must not write again)", conn.beginCalls)
	}
}

func TestPostSettlementClearingSkipsPostingWhenNothingEligible(t *testing.T) {
	// Everything captured has already been cleared by a previous batch.
	conn := settlementTestConn("mch_1", 100000, 100000, 0)

	cleared, err := PostSettlementClearing(context.Background(), conn, "batch_2", "mch_1", time.Now(), "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing() returned error: %v", err)
	}
	if cleared != 0 {
		t.Errorf("PostSettlementClearing() = %d, want 0", cleared)
	}
	if conn.beginCalls != 0 {
		t.Errorf("BeginTx called %d times, want 0 (nothing eligible, must not post a zero-amount transaction)", conn.beginCalls)
	}
}

func TestPostSettlementClearingRejectsEmptyBatchIDWithoutWriting(t *testing.T) {
	conn := settlementTestConn("mch_1", 100000, 0, 0)
	if _, err := PostSettlementClearing(context.Background(), conn, "", "mch_1", time.Now(), "INR"); err == nil {
		t.Error("PostSettlementClearing(batchID=\"\") = nil error, want an error")
	}
	if conn.beginCalls != 0 {
		t.Error("PostSettlementClearing called BeginTx for an empty batch id")
	}
}
