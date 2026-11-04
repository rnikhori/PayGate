//go:build integration

// Package ledger's integration test for PostCapture (see capture.go)
// exercises an end-to-end payment capture against a real, migrated
// Postgres database, asserting the resulting account balances -- this
// issue's acceptance criterion "Integration test covers a capture
// end-to-end and asserts resulting balances".
//
// Not run by `go test ./...` (and so not part of `make test-backend` / CI):
// see balance_integration_test.go's package comment for how to run these
// against a throwaway database.
package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPostCaptureEndToEndAgainstRealPostgres posts one capture through
// PostCapture against a live database, then asserts, from GetBalance
// (ledger's own read side) and a direct read of journal_entries, that:
// customer_suspense moved down by the full gross amount, the merchant's
// payable account now holds the gross amount net of the fee, and
// gateway_fee_revenue moved up by exactly that fee -- i.e. the 3 entries
// this issue's acceptance criteria describe, under one transaction_ref,
// balanced.
func TestPostCaptureEndToEndAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_capture_it_%d", time.Now().UnixNano())
	ref := fmt.Sprintf("txn_capture_it_%d", time.Now().UnixNano())

	const (
		grossAmount = int64(100000) // ₹1000.00
		feeBps      = int64(250)    // 2.50%
		wantFee     = int64(2500)
		wantNet     = grossAmount - wantFee
	)

	suspenseBefore, err := GetBalance(ctx, conn, mustAccountID(t, ctx, conn, CustomerSuspenseAccountCode))
	if err != nil {
		t.Fatalf("GetBalance(customer_suspense) before capture: %v", err)
	}
	feeRevenueBefore, err := GetBalance(ctx, conn, mustAccountID(t, ctx, conn, GatewayFeeRevenueAccountCode))
	if err != nil {
		t.Fatalf("GetBalance(gateway_fee_revenue) before capture: %v", err)
	}

	if err := PostCapture(ctx, conn, ref, merchantID, grossAmount, "INR", FeeSchedule{BasisPoints: feeBps}); err != nil {
		t.Fatalf("PostCapture: %v", err)
	}

	suspenseAfter, err := GetBalance(ctx, conn, mustAccountID(t, ctx, conn, CustomerSuspenseAccountCode))
	if err != nil {
		t.Fatalf("GetBalance(customer_suspense) after capture: %v", err)
	}
	if got, want := suspenseAfter-suspenseBefore, -grossAmount; got != want {
		t.Errorf("customer_suspense balance moved by %d, want %d", got, want)
	}

	feeRevenueAfter, err := GetBalance(ctx, conn, mustAccountID(t, ctx, conn, GatewayFeeRevenueAccountCode))
	if err != nil {
		t.Fatalf("GetBalance(gateway_fee_revenue) after capture: %v", err)
	}
	if got, want := feeRevenueAfter-feeRevenueBefore, wantFee; got != want {
		t.Errorf("gateway_fee_revenue balance moved by %d, want %d", got, want)
	}

	merchantAccount, err := GetAccountByCode(ctx, conn, "merchant_payable:"+merchantID)
	if err != nil {
		t.Fatalf("GetAccountByCode(merchant_payable:%s): %v", merchantID, err)
	}
	merchantBalance, err := GetBalance(ctx, conn, merchantAccount.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable): %v", err)
	}
	// The merchant's payable account was created fresh by this capture
	// (unique merchantID per run), so its balance is this capture's net
	// credit alone, not a delta against some prior balance.
	if merchantBalance != wantNet {
		t.Errorf("merchant_payable balance = %d, want %d (gross %d minus fee %d)", merchantBalance, wantNet, grossAmount, wantFee)
	}

	// Directly verify exactly one transaction_ref with exactly 3 rows that
	// sum to zero -- this issue's "exactly one transaction_ref with 3
	// balanced entries" acceptance criterion, checked against the real
	// table rather than only through GetBalance's aggregation.
	rows, err := db.QueryContext(ctx, `SELECT account_id, amount FROM journal_entries WHERE transaction_ref = $1`, ref)
	if err != nil {
		t.Fatalf("query journal_entries for %q: %v", ref, err)
	}
	defer rows.Close()

	var (
		count int64
		sum   int64
	)
	for rows.Next() {
		var accountID, amount int64
		if err := rows.Scan(&accountID, &amount); err != nil {
			t.Fatalf("scan journal_entries row: %v", err)
		}
		count++
		sum += amount
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate journal_entries rows: %v", err)
	}
	if count != 3 {
		t.Errorf("transaction_ref %q has %d row(s), want exactly 3", ref, count)
	}
	if sum != 0 {
		t.Errorf("transaction_ref %q entries sum to %d, want 0", ref, sum)
	}
}

// mustAccountID resolves a fixed system account's id by its code, failing
// the test if it isn't found -- these accounts are seeded once by
// backend/migrations/*_create_ledger_accounts, so a lookup miss here means
// the test database isn't migrated, not a real "account doesn't exist yet"
// case.
func mustAccountID(t *testing.T, ctx context.Context, db DB, code string) int64 {
	t.Helper()
	a, err := GetAccountByCode(ctx, db, code)
	if err != nil {
		t.Fatalf("GetAccountByCode(%q): %v (is the test database migrated? see balance_integration_test.go's testDB comment)", code, err)
	}
	return a.ID
}
