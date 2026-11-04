package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// fakeCaptureConn is a Conn whose QueryRowContext resolves a canned Account
// by the code every capture-accounts lookup passes as its first query
// argument (see GetAccountByCode and EnsureMerchantPayableAccount), and
// whose BeginTx hands back a fixed Tx — enough to exercise PostCapture end
// to end without a live database.
type fakeCaptureConn struct {
	accounts   map[string]Account
	tx         *fakeTx
	beginErr   error
	beginCalls int
}

func (f *fakeCaptureConn) QueryRowContext(_ context.Context, _ string, args ...any) Row {
	code, _ := args[0].(string)
	a, ok := f.accounts[code]
	if !ok {
		return fakeRow{scan: func(dest ...any) error { return sql.ErrNoRows }}
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

func (f *fakeCaptureConn) BeginTx(context.Context, *sql.TxOptions) (Tx, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.tx, nil
}

// captureTestAccounts returns a fakeCaptureConn pre-seeded with the 3
// accounts a capture for merchantID needs, as if the fixed system accounts
// already existed and EnsureMerchantPayableAccount's upsert had just
// returned merchantID's row.
func captureTestConn(merchantID string) *fakeCaptureConn {
	return &fakeCaptureConn{
		tx: &fakeTx{},
		accounts: map[string]Account{
			CustomerSuspenseAccountCode:  {ID: 1, Code: CustomerSuspenseAccountCode, Type: LiabilityAccount},
			GatewayFeeRevenueAccountCode: {ID: 2, Code: GatewayFeeRevenueAccountCode, Type: RevenueAccount},
			"merchant_payable:" + merchantID: {
				ID: 3, Code: "merchant_payable:" + merchantID, Type: LiabilityAccount,
			},
		},
	}
}

func TestFeeScheduleFeeTruncatesDown(t *testing.T) {
	cases := []struct {
		bps     int64
		gross   int64
		wantFee int64
	}{
		{bps: 200, gross: 10000, wantFee: 200},   // 2.00% of 10000 = 200
		{bps: 250, gross: 100000, wantFee: 2500}, // 2.50% of 100000 = 2500
		{bps: 199, gross: 3, wantFee: 0},         // truncates toward zero, not up
		{bps: 0, gross: 10000, wantFee: 0},
	}
	for _, c := range cases {
		s := FeeSchedule{BasisPoints: c.bps}
		if got := s.Fee(c.gross); got != c.wantFee {
			t.Errorf("FeeSchedule{%d}.Fee(%d) = %d, want %d", c.bps, c.gross, got, c.wantFee)
		}
	}
}

func TestFeeScheduleValidateRejectsNegativeAndTooLarge(t *testing.T) {
	for _, bps := range []int64{-1, -200, basisPointsDenominator, basisPointsDenominator + 1} {
		if err := (FeeSchedule{BasisPoints: bps}).Validate(); err == nil {
			t.Errorf("FeeSchedule{%d}.Validate() = nil, want an error", bps)
		}
	}
}

func TestFeeScheduleValidateAcceptsLegalRates(t *testing.T) {
	for _, bps := range []int64{0, 1, 200, basisPointsDenominator - 1} {
		if err := (FeeSchedule{BasisPoints: bps}).Validate(); err != nil {
			t.Errorf("FeeSchedule{%d}.Validate() = %v, want nil", bps, err)
		}
	}
}

func TestBuildCaptureEntriesProducesThreeBalancedLegs(t *testing.T) {
	accounts := CaptureAccounts{
		CustomerSuspense:  Account{ID: 1},
		MerchantPayable:   Account{ID: 2},
		GatewayFeeRevenue: Account{ID: 3},
	}

	entries, err := BuildCaptureEntries(accounts, 100000, "INR", FeeSchedule{BasisPoints: 250})
	if err != nil {
		t.Fatalf("BuildCaptureEntries() returned error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("BuildCaptureEntries() returned %d entries, want 3", len(entries))
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

	if got := byAccount[1]; got != -100000 {
		t.Errorf("customer_suspense leg = %d, want -100000", got)
	}
	if got := byAccount[2]; got != 97500 {
		t.Errorf("merchant_payable leg = %d, want 97500 (100000 - 2.5%% fee)", got)
	}
	if got := byAccount[3]; got != 2500 {
		t.Errorf("gateway_fee_revenue leg = %d, want 2500", got)
	}
}

func TestBuildCaptureEntriesRejectsNonPositiveGrossAmount(t *testing.T) {
	accounts := CaptureAccounts{CustomerSuspense: Account{ID: 1}, MerchantPayable: Account{ID: 2}, GatewayFeeRevenue: Account{ID: 3}}
	for _, gross := range []int64{0, -1, -100000} {
		if _, err := BuildCaptureEntries(accounts, gross, "INR", FeeSchedule{BasisPoints: 200}); err == nil {
			t.Errorf("BuildCaptureEntries(gross=%d) = nil error, want an error", gross)
		}
	}
}

func TestBuildCaptureEntriesRejectsZeroFee(t *testing.T) {
	accounts := CaptureAccounts{CustomerSuspense: Account{ID: 1}, MerchantPayable: Account{ID: 2}, GatewayFeeRevenue: Account{ID: 3}}
	// A 0bps schedule, or a gross amount small enough to truncate to a
	// zero fee, would only produce 2 legs -- this issue's acceptance
	// criteria call for exactly 3 on every capture.
	if _, err := BuildCaptureEntries(accounts, 100000, "INR", FeeSchedule{BasisPoints: 0}); err == nil {
		t.Error("BuildCaptureEntries(... FeeSchedule{0}) = nil error, want an error (no fee leg to post)")
	}
	if _, err := BuildCaptureEntries(accounts, 1, "INR", FeeSchedule{BasisPoints: 1}); err == nil {
		t.Error("BuildCaptureEntries(gross=1, bps=1) = nil error, want an error (fee truncates to 0)")
	}
}

func TestBuildCaptureEntriesRejectsInvalidCurrency(t *testing.T) {
	accounts := CaptureAccounts{CustomerSuspense: Account{ID: 1}, MerchantPayable: Account{ID: 2}, GatewayFeeRevenue: Account{ID: 3}}
	if _, err := BuildCaptureEntries(accounts, 100000, "inr", FeeSchedule{BasisPoints: 200}); err == nil {
		t.Error("BuildCaptureEntries(currency=\"inr\") = nil error, want an error")
	}
}

func TestPostCaptureWritesExactlyThreeEntriesAndCommits(t *testing.T) {
	conn := captureTestConn("mch_1")

	err := PostCapture(context.Background(), conn, "txn_capture_1", "mch_1", 100000, "INR", FeeSchedule{BasisPoints: 250})
	if err != nil {
		t.Fatalf("PostCapture() returned error: %v", err)
	}

	if conn.beginCalls != 1 {
		t.Errorf("BeginTx called %d times, want 1", conn.beginCalls)
	}
	if !conn.tx.committed {
		t.Error("PostCapture did not commit")
	}
	if len(conn.tx.execCalls) != 3 {
		t.Fatalf("ExecContext called %d times, want 3 (suspense debit, merchant credit, fee credit)", len(conn.tx.execCalls))
	}

	for _, call := range conn.tx.execCalls {
		if call.args[0] != "txn_capture_1" {
			t.Errorf("entry transaction_ref = %v, want %q", call.args[0], "txn_capture_1")
		}
	}
}

func TestPostCaptureRejectsEmptyMerchantID(t *testing.T) {
	conn := captureTestConn("mch_1")

	if err := PostCapture(context.Background(), conn, "txn_capture_2", "", 100000, "INR", FeeSchedule{BasisPoints: 200}); err == nil {
		t.Error("PostCapture(merchantID=\"\") = nil error, want an error")
	}
	if conn.beginCalls != 0 {
		t.Error("PostCapture called BeginTx for an empty merchant id")
	}
}

func TestPostCaptureRejectsNonPositiveGrossAmountWithoutWriting(t *testing.T) {
	conn := captureTestConn("mch_1")

	if err := PostCapture(context.Background(), conn, "txn_capture_3", "mch_1", 0, "INR", FeeSchedule{BasisPoints: 200}); err == nil {
		t.Error("PostCapture(grossAmount=0) = nil error, want an error")
	}
	if conn.beginCalls != 0 {
		t.Error("PostCapture called BeginTx for a non-positive gross amount")
	}
}

func TestPostCapturePropagatesMissingAccountError(t *testing.T) {
	// A conn that knows nothing about customer_suspense simulates a
	// misconfigured/unmigrated database.
	conn := &fakeCaptureConn{tx: &fakeTx{}, accounts: map[string]Account{}}

	err := PostCapture(context.Background(), conn, "txn_capture_4", "mch_1", 100000, "INR", FeeSchedule{BasisPoints: 200})
	if err == nil {
		t.Fatal("PostCapture() = nil error, want an error when customer_suspense can't be resolved")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("PostCapture() error = %v, want it to wrap sql.ErrNoRows", err)
	}
	if conn.beginCalls != 0 {
		t.Error("PostCapture called BeginTx after failing to resolve accounts")
	}
}

func TestPostCaptureTxDoesNotCommitOrRollback(t *testing.T) {
	tx := &fakeTx{}
	accounts := CaptureAccounts{
		CustomerSuspense:  Account{ID: 1},
		MerchantPayable:   Account{ID: 2},
		GatewayFeeRevenue: Account{ID: 3},
	}

	err := PostCaptureTx(context.Background(), tx, "txn_capture_5", accounts, 100000, "INR", FeeSchedule{BasisPoints: 200})
	if err != nil {
		t.Fatalf("PostCaptureTx() returned error: %v", err)
	}
	if len(tx.execCalls) != 3 {
		t.Fatalf("ExecContext called %d times, want 3", len(tx.execCalls))
	}
	if tx.committed {
		t.Error("PostCaptureTx committed tx; it must leave that to the caller")
	}
	if tx.rolledBack {
		t.Error("PostCaptureTx rolled back tx; it must leave that to the caller")
	}
}
