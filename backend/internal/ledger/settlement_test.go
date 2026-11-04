package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// An account-code lookup/upsert (GetAccountByCode, EnsureMerchantPayableAccount)
	// always passes exactly the code as its 1st arg -- a miss here must
	// report sql.ErrNoRows directly, the same way fakeCaptureConn and
	// fakeRefundConn do, rather than falling through to the sumEntries-
	// style handling below, which assumes a 2nd (accountID-keyed) arg that
	// a bare code lookup never has.
	if code, ok := args[0].(string); ok {
		a, found := f.accounts[code]
		if !found {
			return fakeRow{scan: func(...any) error { return sql.ErrNoRows }}
		}
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = a.ID
			*dest[1].(*string) = a.Code
			*dest[2].(*string) = string(a.Type)
			*dest[3].(*string) = a.Name
			*dest[4].(*time.Time) = a.CreatedAt
			return nil
		}}
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

// TestResolveSettlementClearingAccountsPropagatesLookupErrors mirrors
// TestResolveCaptureAccountsPropagatesGatewayFeeRevenueLookupError /
// ...MerchantPayableLookupError for settlement's own 2-account resolver:
// either the fixed settlement_clearing lookup or the merchant payable
// upsert failing must surface, not be swallowed.
func TestResolveSettlementClearingAccountsPropagatesLookupErrors(t *testing.T) {
	t.Run("settlement_clearing lookup fails", func(t *testing.T) {
		conn := &fakeSettlementConn{tx: &fakeTx{}, accounts: map[string]Account{}}
		if _, err := ResolveSettlementClearingAccounts(context.Background(), conn, "mch_1"); err == nil {
			t.Error("ResolveSettlementClearingAccounts() = nil error, want an error when settlement_clearing can't be resolved")
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("ResolveSettlementClearingAccounts() error = %v, want it to wrap sql.ErrNoRows", err)
		}
	})

	t.Run("merchant payable upsert fails", func(t *testing.T) {
		conn := &fakeSettlementConn{tx: &fakeTx{}, accounts: map[string]Account{
			SettlementClearingAccountCode: {ID: 4, Code: SettlementClearingAccountCode, Type: AssetAccount},
		}}
		if _, err := ResolveSettlementClearingAccounts(context.Background(), conn, "mch_1"); err == nil {
			t.Error("ResolveSettlementClearingAccounts() = nil error, want an error when the merchant payable account can't be ensured")
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("ResolveSettlementClearingAccounts() error = %v, want it to wrap sql.ErrNoRows", err)
		}
	})
}

// TestResolveSettlementAmountPropagatesQueryErrors exercises
// ResolveSettlementAmount's own 2 QueryRowContext calls -- the "captured
// before cutoff" sum (1st) and the "already cleared" sum (2nd) -- each
// failing in turn.
func TestResolveSettlementAmountPropagatesQueryErrors(t *testing.T) {
	cases := []struct {
		name string
		call int
	}{
		{"captured-before-cutoff sum", 1},
		{"already-cleared sum", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantErr := fmt.Errorf("boom reading %s", c.name)
			conn := &errOnCallConn{inner: settlementTestConn("mch_1", 100000, 0, 0), n: c.call, err: wantErr}

			_, err := ResolveSettlementAmount(context.Background(), conn, 3, "mch_1", time.Now())
			if err == nil {
				t.Fatal("ResolveSettlementAmount() = nil error, want an error")
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("ResolveSettlementAmount() error = %v, want it to wrap %v", err, wantErr)
			}
		})
	}
}

// TestPostSettlementClearingPropagatesAccountResolutionError asserts
// PostSettlementClearing itself surfaces a ResolveSettlementClearingAccounts
// failure (here: settlement_clearing missing).
func TestPostSettlementClearingPropagatesAccountResolutionError(t *testing.T) {
	conn := &fakeSettlementConn{tx: &fakeTx{}, accounts: map[string]Account{}}

	_, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err == nil {
		t.Fatal("PostSettlementClearing() = nil error, want an error when settlement accounts can't be resolved")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("PostSettlementClearing() error = %v, want it to wrap sql.ErrNoRows", err)
	}
	if conn.beginCalls != 0 {
		t.Error("PostSettlementClearing called BeginTx despite account resolution failing")
	}
}

// TestPostSettlementClearingPropagatesIdempotencyCheckError asserts a
// failure reading back whether this batch/merchant was already posted
// (the 3rd QueryRowContext call: 2 account lookups, then the idempotency
// sumEntries) is surfaced rather than silently treated as "not yet posted".
func TestPostSettlementClearingPropagatesIdempotencyCheckError(t *testing.T) {
	wantErr := errors.New("boom checking idempotency")
	conn := &errOnCallConn{inner: settlementTestConn("mch_1", 100000, 0, 0), n: 3, err: wantErr}

	_, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err == nil {
		t.Fatal("PostSettlementClearing() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostSettlementClearing() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestPostSettlementClearingPropagatesResolveSettlementAmountError asserts
// a failure inside ResolveSettlementAmount (the 4th QueryRowContext call --
// after the 2 account lookups and the idempotency check succeed) is
// surfaced by PostSettlementClearing.
func TestPostSettlementClearingPropagatesResolveSettlementAmountError(t *testing.T) {
	wantErr := errors.New("boom resolving settlement amount")
	conn := &errOnCallConn{inner: settlementTestConn("mch_1", 100000, 0, 0), n: 4, err: wantErr}

	_, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err == nil {
		t.Fatal("PostSettlementClearing() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostSettlementClearing() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestPostSettlementClearingPropagatesBuildEntriesErrorWithoutWriting
// asserts an invalid currency (BuildSettlementClearingEntries' own check)
// is rejected without ever calling BeginTx -- there is a positive amount
// eligible to clear, so this is the one error BuildSettlementClearingEntries
// can still produce once PostSettlementClearing has already checked amount
// > 0 itself.
func TestPostSettlementClearingPropagatesBuildEntriesErrorWithoutWriting(t *testing.T) {
	conn := settlementTestConn("mch_1", 100000, 0, 0)

	_, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "inr")
	if err == nil {
		t.Fatal("PostSettlementClearing() = nil error, want an error for an invalid currency")
	}
	if conn.beginCalls != 0 {
		t.Error("PostSettlementClearing called BeginTx despite BuildSettlementClearingEntries failing")
	}
}

// TestPostSettlementClearingPropagatesPostJournalEntryError asserts a
// failure in the final PostJournalEntry call (here: the entries insert
// itself failing) is surfaced, rather than the cleared amount being
// returned as if it had actually posted.
func TestPostSettlementClearingPropagatesPostJournalEntryError(t *testing.T) {
	wantErr := errors.New("boom inserting journal entries")
	conn := settlementTestConn("mch_1", 100000, 0, 0)
	conn.tx.execErr = wantErr

	_, err := PostSettlementClearing(context.Background(), conn, "batch_1", "mch_1", time.Now(), "INR")
	if err == nil {
		t.Fatal("PostSettlementClearing() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostSettlementClearing() error = %v, want it to wrap %v", err, wantErr)
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
