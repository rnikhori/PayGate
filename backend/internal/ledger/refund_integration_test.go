//go:build integration

// Package ledger's integration test for PostRefund (see refund.go)
// exercises a capture followed by one or more refunds end to end against a
// real, migrated Postgres database, asserting the resulting account
// balances and transaction_ref traceability -- this issue's acceptance
// criteria: a proportional partial refund, two sequential partial refunds
// reconciling to the full capture, and over-refund rejection.
//
// Not run by `go test ./...` (and so not part of `make test-backend` / CI):
// see balance_integration_test.go's package comment for how to run these
// against a throwaway database.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedCapture posts a capture for a fresh merchant via PostCapture and
// returns its transaction_ref, merchant id, and the accounts a refund
// against it will need.
func seedCapture(t *testing.T, ctx context.Context, conn Conn, grossAmount, feeBps int64) (captureRef, merchantID string, accounts CaptureAccounts) {
	t.Helper()

	merchantID = fmt.Sprintf("mch_refund_it_%d", time.Now().UnixNano())
	captureRef = fmt.Sprintf("txn_capture_it_%d", time.Now().UnixNano())

	if err := PostCapture(ctx, conn, captureRef, merchantID, grossAmount, "INR", FeeSchedule{BasisPoints: feeBps}); err != nil {
		t.Fatalf("seed PostCapture: %v", err)
	}

	accounts, err := ResolveCaptureAccounts(ctx, conn, merchantID)
	if err != nil {
		t.Fatalf("ResolveCaptureAccounts: %v", err)
	}
	return captureRef, merchantID, accounts
}

// TestPostRefundFullRefundEndToEndAgainstRealPostgres posts a capture, then
// refunds it in full, and asserts customer_suspense, gateway_fee_revenue,
// and the merchant's payable account all move back to exactly where they
// started.
func TestPostRefundFullRefundEndToEndAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	const (
		grossAmount = int64(100000)
		feeBps      = int64(250)
		wantFee     = int64(2500)
	)

	captureRef, merchantID, accounts := seedCapture(t, ctx, conn, grossAmount, feeBps)

	suspenseAfterCapture, err := GetBalance(ctx, conn, accounts.CustomerSuspense.ID)
	if err != nil {
		t.Fatalf("GetBalance(customer_suspense) after capture: %v", err)
	}
	feeRevenueAfterCapture, err := GetBalance(ctx, conn, accounts.GatewayFeeRevenue.ID)
	if err != nil {
		t.Fatalf("GetBalance(gateway_fee_revenue) after capture: %v", err)
	}
	merchantAfterCapture, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) after capture: %v", err)
	}

	if err := PostRefund(ctx, conn, captureRef, "refund_full", merchantID, grossAmount, "INR"); err != nil {
		t.Fatalf("PostRefund (full): %v", err)
	}

	suspenseAfterRefund, err := GetBalance(ctx, conn, accounts.CustomerSuspense.ID)
	if err != nil {
		t.Fatalf("GetBalance(customer_suspense) after refund: %v", err)
	}
	if got, want := suspenseAfterRefund-suspenseAfterCapture, grossAmount; got != want {
		t.Errorf("customer_suspense moved by %d on refund, want %d", got, want)
	}

	feeRevenueAfterRefund, err := GetBalance(ctx, conn, accounts.GatewayFeeRevenue.ID)
	if err != nil {
		t.Fatalf("GetBalance(gateway_fee_revenue) after refund: %v", err)
	}
	if got, want := feeRevenueAfterCapture-feeRevenueAfterRefund, wantFee; got != want {
		t.Errorf("gateway_fee_revenue reversed by %d, want %d", got, want)
	}

	merchantAfterRefund, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) after refund: %v", err)
	}
	if got, want := merchantAfterCapture-merchantAfterRefund, grossAmount-wantFee; got != want {
		t.Errorf("merchant_payable reversed by %d, want %d", got, want)
	}

	// The merchant's net position for this payment is back to exactly 0
	// (capture's net credit, fully reversed by the refund's net debit).
	if merchantAfterRefund != 0 {
		t.Errorf("merchant_payable balance after full refund = %d, want 0", merchantAfterRefund)
	}

	// Traceability: the refund's transaction_ref embeds the capture's.
	wantRef := "refund:" + captureRef + ":refund_full"
	rows, err := db.QueryContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE transaction_ref = $1`, wantRef)
	if err != nil {
		t.Fatalf("query journal_entries for %q: %v", wantRef, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row counting journal_entries for %q", wantRef)
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if count != 3 {
		t.Errorf("refund transaction_ref %q has %d row(s), want 3 (suspense, merchant, fee)", wantRef, count)
	}
}

// TestPostRefundTwoSequentialPartialRefundsAgainstRealPostgres posts a
// capture, refunds 40% of it, then refunds the remaining 60%, and asserts
// the two partial refunds together reconcile exactly to a full refund.
func TestPostRefundTwoSequentialPartialRefundsAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	const (
		grossAmount = int64(100000)
		feeBps      = int64(250)
	)

	captureRef, merchantID, accounts := seedCapture(t, ctx, conn, grossAmount, feeBps)

	merchantBefore, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) before refunds: %v", err)
	}

	if err := PostRefund(ctx, conn, captureRef, "refund_partial_1", merchantID, 40000, "INR"); err != nil {
		t.Fatalf("PostRefund (first partial): %v", err)
	}
	if err := PostRefund(ctx, conn, captureRef, "refund_partial_2", merchantID, 60000, "INR"); err != nil {
		t.Fatalf("PostRefund (second partial): %v", err)
	}

	merchantAfter, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) after refunds: %v", err)
	}
	// Two partials summing to the full gross must reverse the merchant's
	// net credit exactly -- same end state as one full refund.
	if merchantAfter != 0 {
		t.Errorf("merchant_payable balance after two sequential partial refunds totalling the gross = %d, want 0 (before this payment: %d)", merchantAfter, merchantBefore)
	}
}

// TestPostRefundRejectsOverRefundAgainstRealPostgres posts a capture,
// refunds most of it, then asserts a further refund that would exceed the
// capture's gross amount is rejected -- and that the rejected call wrote
// nothing (the merchant's balance is unchanged).
func TestPostRefundRejectsOverRefundAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	const (
		grossAmount = int64(100000)
		feeBps      = int64(250)
	)

	captureRef, merchantID, accounts := seedCapture(t, ctx, conn, grossAmount, feeBps)

	if err := PostRefund(ctx, conn, captureRef, "refund_most", merchantID, 70000, "INR"); err != nil {
		t.Fatalf("PostRefund (70%%): %v", err)
	}

	merchantBeforeRejected, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) before rejected over-refund: %v", err)
	}

	err = PostRefund(ctx, conn, captureRef, "refund_over", merchantID, 40000, "INR")
	if err == nil {
		t.Fatal("PostRefund(remaining 30000 + extra = 40000) = nil error, want an error (over-refund)")
	}
	if !errors.Is(err, ErrRefundExceedsAvailableBalance) {
		t.Errorf("PostRefund() error = %v, want it to wrap ErrRefundExceedsAvailableBalance", err)
	}

	merchantAfterRejected, err := GetBalance(ctx, conn, accounts.MerchantPayable.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable) after rejected over-refund: %v", err)
	}
	if merchantAfterRejected != merchantBeforeRejected {
		t.Errorf("merchant_payable balance changed from %d to %d after a rejected over-refund, want no write", merchantBeforeRejected, merchantAfterRejected)
	}
}
