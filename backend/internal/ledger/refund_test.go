package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// fakeRefundConn is a Conn that resolves the 3 capture accounts exactly
// like fakeCaptureConn (see capture_test.go), but additionally answers the
// sumEntries queries ResolveRefundContext issues — keyed by account id and
// whether the query is an exact transaction_ref match or a LIKE-pattern
// match — so refund tests can exercise PostRefund end to end without a
// live database.
type fakeRefundConn struct {
	accounts map[string]Account

	// sums[accountID] is the exact-match sum (the original capture's leg
	// for that account); likeSums[accountID] is the LIKE-match sum (prior
	// refunds' leg for that account).
	sums     map[int64]int64
	likeSums map[int64]int64

	tx         *fakeTx
	beginErr   error
	beginCalls int
}

func (f *fakeRefundConn) QueryRowContext(_ context.Context, query string, args ...any) Row {
	// Account lookups (GetAccountByCode / EnsureMerchantPayableAccount)
	// pass the code as their first arg, same as fakeCaptureConn.
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

	// Otherwise this is a sumEntries call: args[0] is always the account
	// id; whether it's a LIKE match is decided by which query text ran.
	accountID, _ := args[0].(int64)
	var sum int64
	if containsLike(query) {
		sum = f.likeSums[accountID]
	} else {
		sum = f.sums[accountID]
	}
	return fakeRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = sum
		return nil
	}}
}

func containsLike(query string) bool {
	for i := 0; i+4 <= len(query); i++ {
		if query[i:i+4] == "LIKE" {
			return true
		}
	}
	return false
}

func (f *fakeRefundConn) BeginTx(context.Context, *sql.TxOptions) (Tx, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.tx, nil
}

// refundTestConn returns a fakeRefundConn pre-seeded with the 3 accounts a
// refund for merchantID needs, plus captureGross/captureFee as the
// original capture's recorded legs and alreadyRefunded as the sum of any
// prior refunds against it.
func refundTestConn(merchantID string, captureGross, captureFee, alreadyRefunded int64) *fakeRefundConn {
	const (
		suspenseID = int64(1)
		feeID      = int64(2)
		payableID  = int64(3)
	)
	return &fakeRefundConn{
		tx: &fakeTx{},
		accounts: map[string]Account{
			CustomerSuspenseAccountCode:  {ID: suspenseID, Code: CustomerSuspenseAccountCode, Type: LiabilityAccount},
			GatewayFeeRevenueAccountCode: {ID: feeID, Code: GatewayFeeRevenueAccountCode, Type: RevenueAccount},
			"merchant_payable:" + merchantID: {
				ID: payableID, Code: "merchant_payable:" + merchantID, Type: LiabilityAccount,
			},
		},
		sums: map[int64]int64{
			suspenseID: -captureGross, // capture debited suspense
			feeID:      captureFee,    // capture credited fee revenue
		},
		likeSums: map[int64]int64{
			suspenseID: alreadyRefunded, // prior refunds credited suspense
		},
	}
}

func rcFor(merchantID string, captureGross, captureFee, alreadyRefunded int64) RefundContext {
	return RefundContext{
		Accounts: CaptureAccounts{
			CustomerSuspense:  Account{ID: 1},
			MerchantPayable:   Account{ID: 3},
			GatewayFeeRevenue: Account{ID: 2},
		},
		GrossAmount:     captureGross,
		Fee:             captureFee,
		AlreadyRefunded: alreadyRefunded,
	}
}

func TestRefundTransactionRefFormat(t *testing.T) {
	got, err := RefundTransactionRef("txn_capture_1", "refund_1")
	if err != nil {
		t.Fatalf("RefundTransactionRef() returned error: %v", err)
	}
	if want := "refund:txn_capture_1:refund_1"; got != want {
		t.Errorf("RefundTransactionRef() = %q, want %q", got, want)
	}
}

func TestRefundTransactionRefRejectsEmptyArgs(t *testing.T) {
	if _, err := RefundTransactionRef("", "refund_1"); err == nil {
		t.Error("RefundTransactionRef(\"\", ...) = nil error, want an error")
	}
	if _, err := RefundTransactionRef("txn_capture_1", ""); err == nil {
		t.Error("RefundTransactionRef(..., \"\") = nil error, want an error")
	}
}

func TestBuildRefundEntriesFullRefundReversesExactly(t *testing.T) {
	// Gross 100000, fee 2500 (2.5%), net 97500 -- a full refund must
	// reverse the fee and net exactly, down to the last paisa.
	rc := rcFor("mch_1", 100000, 2500, 0)

	entries, err := BuildRefundEntries(rc, 100000, "INR")
	if err != nil {
		t.Fatalf("BuildRefundEntries() returned error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("BuildRefundEntries() returned %d entries, want 3", len(entries))
	}

	byAccount := map[int64]int64{}
	var sum int64
	for _, e := range entries {
		byAccount[e.AccountID] = e.Amount
		sum += e.Amount
	}
	if sum != 0 {
		t.Errorf("entries sum to %d, want 0 (balanced)", sum)
	}
	if got := byAccount[1]; got != 100000 {
		t.Errorf("customer_suspense leg = %d, want 100000", got)
	}
	if got := byAccount[3]; got != -97500 {
		t.Errorf("merchant_payable leg = %d, want -97500", got)
	}
	if got := byAccount[2]; got != -2500 {
		t.Errorf("gateway_fee_revenue leg = %d, want -2500", got)
	}
}

func TestBuildRefundEntriesPartialRefundIsProportional(t *testing.T) {
	// Gross 100000, fee 2500. Refunding 40% should reverse 40% of the fee.
	rc := rcFor("mch_1", 100000, 2500, 0)

	entries, err := BuildRefundEntries(rc, 40000, "INR")
	if err != nil {
		t.Fatalf("BuildRefundEntries() returned error: %v", err)
	}

	byAccount := map[int64]int64{}
	for _, e := range entries {
		byAccount[e.AccountID] = e.Amount
	}
	if got := byAccount[1]; got != 40000 {
		t.Errorf("customer_suspense leg = %d, want 40000 (not the full gross)", got)
	}
	if got := byAccount[2]; got != -1000 {
		t.Errorf("gateway_fee_revenue leg = %d, want -1000 (2500 * 40000 / 100000)", got)
	}
	if got := byAccount[3]; got != -39000 {
		t.Errorf("merchant_payable leg = %d, want -39000 (40000 - 1000)", got)
	}
}

func TestBuildRefundEntriesTwoSequentialPartialRefundsSumToFullCapture(t *testing.T) {
	gross, fee := int64(100000), int64(2500)

	rc1 := rcFor("mch_1", gross, fee, 0)
	first, err := BuildRefundEntries(rc1, 40000, "INR")
	if err != nil {
		t.Fatalf("first BuildRefundEntries() returned error: %v", err)
	}

	rc2 := rcFor("mch_1", gross, fee, 40000) // first refund already posted
	second, err := BuildRefundEntries(rc2, 60000, "INR")
	if err != nil {
		t.Fatalf("second BuildRefundEntries() returned error: %v", err)
	}

	sumByAccount := map[int64]int64{}
	for _, e := range append(first, second...) {
		sumByAccount[e.AccountID] += e.Amount
	}
	if got := sumByAccount[1]; got != gross {
		t.Errorf("total customer_suspense reversal = %d, want %d (the full gross)", got, gross)
	}
	if got := sumByAccount[2]; got != -fee {
		t.Errorf("total gateway_fee_revenue reversal = %d, want %d (the full fee)", got, -fee)
	}
	if got := sumByAccount[3]; got != -(gross - fee) {
		t.Errorf("total merchant_payable reversal = %d, want %d (the full net)", got, -(gross - fee))
	}
}

func TestBuildRefundEntriesRejectsOverRefund(t *testing.T) {
	rc := rcFor("mch_1", 100000, 2500, 70000) // only 30000 left available

	if _, err := BuildRefundEntries(rc, 40000, "INR"); err == nil {
		t.Error("BuildRefundEntries() = nil error, want an error for an over-refund")
	} else if !errors.Is(err, ErrRefundExceedsAvailableBalance) {
		t.Errorf("BuildRefundEntries() error = %v, want it to wrap ErrRefundExceedsAvailableBalance", err)
	}
}

func TestBuildRefundEntriesRejectsOverRefundAtExactBoundaryPlusOne(t *testing.T) {
	rc := rcFor("mch_1", 100000, 2500, 0)

	// Exactly the available amount must succeed...
	if _, err := BuildRefundEntries(rc, 100000, "INR"); err != nil {
		t.Errorf("BuildRefundEntries(refundAmount=available) returned error: %v", err)
	}
	// ...one paisa more must not.
	if _, err := BuildRefundEntries(rc, 100001, "INR"); err == nil {
		t.Error("BuildRefundEntries(refundAmount=available+1) = nil error, want an error")
	}
}

func TestBuildRefundEntriesRejectsNonPositiveAmount(t *testing.T) {
	rc := rcFor("mch_1", 100000, 2500, 0)
	for _, amount := range []int64{0, -1, -100000} {
		if _, err := BuildRefundEntries(rc, amount, "INR"); err == nil {
			t.Errorf("BuildRefundEntries(amount=%d) = nil error, want an error", amount)
		}
	}
}

func TestBuildRefundEntriesRejectsInvalidCurrency(t *testing.T) {
	rc := rcFor("mch_1", 100000, 2500, 0)
	if _, err := BuildRefundEntries(rc, 1000, "inr"); err == nil {
		t.Error("BuildRefundEntries(currency=\"inr\") = nil error, want an error")
	}
}

func TestBuildRefundEntriesRejectsUnknownCapture(t *testing.T) {
	rc := rcFor("mch_1", 0, 0, 0) // no customer_suspense leg ever recorded
	if _, err := BuildRefundEntries(rc, 1000, "INR"); err == nil {
		t.Error("BuildRefundEntries() = nil error, want an error when the original capture has no gross amount")
	}
}

func TestBuildRefundEntriesOmitsZeroFeeLeg(t *testing.T) {
	// A tiny refund of a tiny fee can truncate the proportional fee
	// reversal to 0; only 2 legs should be posted, never a zero-amount
	// 3rd leg (ValidateBalancedTransaction would reject that anyway).
	rc := rcFor("mch_1", 100000, 1, 0) // fee=1, any refund < 100000 truncates to 0

	entries, err := BuildRefundEntries(rc, 1000, "INR")
	if err != nil {
		t.Fatalf("BuildRefundEntries() returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("BuildRefundEntries() returned %d entries, want 2 (no zero-amount fee leg)", len(entries))
	}
	for _, e := range entries {
		if e.AccountID == 2 {
			t.Error("BuildRefundEntries() posted a fee_revenue leg despite a 0 proportional reversal")
		}
	}
}

func TestResolveRefundContextReadsOriginalCaptureAndPriorRefunds(t *testing.T) {
	conn := refundTestConn("mch_1", 100000, 2500, 40000)

	rc, err := ResolveRefundContext(context.Background(), conn, "mch_1", "txn_capture_1")
	if err != nil {
		t.Fatalf("ResolveRefundContext() returned error: %v", err)
	}
	if rc.GrossAmount != 100000 {
		t.Errorf("GrossAmount = %d, want 100000", rc.GrossAmount)
	}
	if rc.Fee != 2500 {
		t.Errorf("Fee = %d, want 2500", rc.Fee)
	}
	if rc.AlreadyRefunded != 40000 {
		t.Errorf("AlreadyRefunded = %d, want 40000", rc.AlreadyRefunded)
	}
}

func TestResolveRefundContextRejectsEmptyCaptureRef(t *testing.T) {
	conn := refundTestConn("mch_1", 100000, 2500, 0)
	if _, err := ResolveRefundContext(context.Background(), conn, "mch_1", ""); err == nil {
		t.Error("ResolveRefundContext(..., \"\") = nil error, want an error")
	}
}

func TestPostRefundWritesEntriesUnderTheExpectedTransactionRefAndCommits(t *testing.T) {
	conn := refundTestConn("mch_1", 100000, 2500, 0)

	err := PostRefund(context.Background(), conn, "txn_capture_1", "refund_1", "mch_1", 100000, "INR")
	if err != nil {
		t.Fatalf("PostRefund() returned error: %v", err)
	}

	if !conn.tx.committed {
		t.Error("PostRefund did not commit")
	}
	if len(conn.tx.execCalls) != 3 {
		t.Fatalf("ExecContext called %d times, want 3", len(conn.tx.execCalls))
	}
	wantRef := "refund:txn_capture_1:refund_1"
	for _, call := range conn.tx.execCalls {
		if call.args[0] != wantRef {
			t.Errorf("entry transaction_ref = %v, want %q", call.args[0], wantRef)
		}
	}
}

func TestPostRefundRejectsOverRefundWithoutWriting(t *testing.T) {
	conn := refundTestConn("mch_1", 100000, 2500, 70000)

	err := PostRefund(context.Background(), conn, "txn_capture_1", "refund_2", "mch_1", 40000, "INR")
	if err == nil {
		t.Fatal("PostRefund() = nil error, want an error for an over-refund")
	}
	if !errors.Is(err, ErrRefundExceedsAvailableBalance) {
		t.Errorf("PostRefund() error = %v, want it to wrap ErrRefundExceedsAvailableBalance", err)
	}
	if conn.beginCalls != 0 {
		t.Error("PostRefund called BeginTx for a rejected over-refund")
	}
}

func TestPostRefundTxDoesNotCommitOrRollback(t *testing.T) {
	tx := &fakeTx{}
	rc := rcFor("mch_1", 100000, 2500, 0)

	err := PostRefundTx(context.Background(), tx, "txn_capture_1", "refund_1", rc, 50000, "INR")
	if err != nil {
		t.Fatalf("PostRefundTx() returned error: %v", err)
	}
	if len(tx.execCalls) == 0 {
		t.Fatal("PostRefundTx did not write any entries")
	}
	if tx.committed {
		t.Error("PostRefundTx committed tx; it must leave that to the caller")
	}
	if tx.rolledBack {
		t.Error("PostRefundTx rolled back tx; it must leave that to the caller")
	}
}
