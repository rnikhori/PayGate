//go:build integration

// Package ledger's integration test for PostSettlementClearing (see
// settlement.go) exercises settlement batches against a real, migrated
// Postgres database -- this issue's acceptance criteria: one
// transaction_ref per merchant per batch, only captured payments older
// than the cutoff included, idempotency on retry, and a batch spanning
// multiple payments for one merchant.
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

// TestPostSettlementClearingBatchSpanningMultipleCapturesForOneMerchantAgainstRealPostgres
// captures 3 separate payments for one merchant, then settles them all in
// one batch, asserting: exactly one transaction_ref with exactly 2 rows
// that sum to zero, the amount cleared equals the sum of all 3 captures'
// net merchant_payable credit, and the merchant's payable balance lands at
// exactly 0 (everything captured is now in transit).
func TestPostSettlementClearingBatchSpanningMultipleCapturesForOneMerchantAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_settle_it_%d", time.Now().UnixNano())
	const feeBps = int64(250)

	var wantNet int64
	for i, gross := range []int64{100000, 25000, 7500} {
		ref := fmt.Sprintf("txn_capture_settle_it_%d_%d", time.Now().UnixNano(), i)
		if err := PostCapture(ctx, conn, ref, merchantID, gross, "INR", FeeSchedule{BasisPoints: feeBps}); err != nil {
			t.Fatalf("seed PostCapture %d: %v", i, err)
		}
		wantNet += gross - (FeeSchedule{BasisPoints: feeBps}).Fee(gross)
	}

	batchID := fmt.Sprintf("batch_it_%d", time.Now().UnixNano())
	cutoff := time.Now().Add(time.Hour) // comfortably after all 3 captures above

	cleared, err := PostSettlementClearing(ctx, conn, batchID, merchantID, cutoff, "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing: %v", err)
	}
	if cleared != wantNet {
		t.Errorf("PostSettlementClearing() cleared = %d, want %d (sum of all 3 captures' net merchant_payable credit)", cleared, wantNet)
	}

	ref, err := SettlementTransactionRef(batchID, merchantID)
	if err != nil {
		t.Fatalf("SettlementTransactionRef: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT account_id, amount FROM journal_entries WHERE transaction_ref = $1`, ref)
	if err != nil {
		t.Fatalf("query journal_entries for %q: %v", ref, err)
	}
	defer rows.Close()

	var count, sum int64
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
	if count != 2 {
		t.Errorf("transaction_ref %q has %d row(s), want exactly 2 (merchant_payable debit, settlement_clearing credit)", ref, count)
	}
	if sum != 0 {
		t.Errorf("transaction_ref %q entries sum to %d, want 0", ref, sum)
	}

	merchantAccount, err := GetAccountByCode(ctx, conn, "merchant_payable:"+merchantID)
	if err != nil {
		t.Fatalf("GetAccountByCode(merchant_payable:%s): %v", merchantID, err)
	}
	merchantBalance, err := GetBalance(ctx, conn, merchantAccount.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable): %v", err)
	}
	if merchantBalance != 0 {
		t.Errorf("merchant_payable balance after settling everything captured = %d, want 0", merchantBalance)
	}
}

// TestPostSettlementClearingExcludesCapturesAfterCutoffAgainstRealPostgres
// captures one payment before a cutoff and one after it, settles against
// that cutoff, and asserts only the earlier capture's net amount clears --
// the later one is left in merchant_payable, uncleared.
func TestPostSettlementClearingExcludesCapturesAfterCutoffAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_settle_cutoff_it_%d", time.Now().UnixNano())
	const feeBps = int64(250)
	schedule := FeeSchedule{BasisPoints: feeBps}

	const grossBefore = int64(100000)
	refBefore := fmt.Sprintf("txn_capture_before_it_%d", time.Now().UnixNano())
	if err := PostCapture(ctx, conn, refBefore, merchantID, grossBefore, "INR", schedule); err != nil {
		t.Fatalf("seed PostCapture (before cutoff): %v", err)
	}

	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond) // ensure the next capture's created_at is strictly after cutoff

	const grossAfter = int64(50000)
	refAfter := fmt.Sprintf("txn_capture_after_it_%d", time.Now().UnixNano())
	if err := PostCapture(ctx, conn, refAfter, merchantID, grossAfter, "INR", schedule); err != nil {
		t.Fatalf("seed PostCapture (after cutoff): %v", err)
	}

	batchID := fmt.Sprintf("batch_cutoff_it_%d", time.Now().UnixNano())
	cleared, err := PostSettlementClearing(ctx, conn, batchID, merchantID, cutoff, "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing: %v", err)
	}

	wantCleared := grossBefore - schedule.Fee(grossBefore)
	if cleared != wantCleared {
		t.Errorf("PostSettlementClearing() cleared = %d, want %d (only the capture before cutoff)", cleared, wantCleared)
	}

	merchantAccount, err := GetAccountByCode(ctx, conn, "merchant_payable:"+merchantID)
	if err != nil {
		t.Fatalf("GetAccountByCode(merchant_payable:%s): %v", merchantID, err)
	}
	merchantBalance, err := GetBalance(ctx, conn, merchantAccount.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable): %v", err)
	}
	wantRemaining := grossAfter - schedule.Fee(grossAfter)
	if merchantBalance != wantRemaining {
		t.Errorf("merchant_payable balance after settling only the pre-cutoff capture = %d, want %d (the post-cutoff capture, still uncleared)", merchantBalance, wantRemaining)
	}
}

// TestPostSettlementClearingRetryAfterPartialFailureIsIdempotentAgainstRealPostgres
// settles a merchant's batch once, then calls PostSettlementClearing again
// for the exact same batchID and merchantID (simulating a settlement job
// retried after a partial failure elsewhere in the same batch) and asserts
// the second call neither posts additional entries nor double-counts the
// amount cleared.
func TestPostSettlementClearingRetryAfterPartialFailureIsIdempotentAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_settle_retry_it_%d", time.Now().UnixNano())
	const gross = int64(100000)
	schedule := FeeSchedule{BasisPoints: 250}

	ref := fmt.Sprintf("txn_capture_retry_it_%d", time.Now().UnixNano())
	if err := PostCapture(ctx, conn, ref, merchantID, gross, "INR", schedule); err != nil {
		t.Fatalf("seed PostCapture: %v", err)
	}

	batchID := fmt.Sprintf("batch_retry_it_%d", time.Now().UnixNano())
	cutoff := time.Now().Add(time.Hour)

	firstCleared, err := PostSettlementClearing(ctx, conn, batchID, merchantID, cutoff, "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing (first call): %v", err)
	}

	secondCleared, err := PostSettlementClearing(ctx, conn, batchID, merchantID, cutoff, "INR")
	if err != nil {
		t.Fatalf("PostSettlementClearing (retry): %v", err)
	}
	if secondCleared != firstCleared {
		t.Errorf("PostSettlementClearing() retry = %d, want %d (the same amount, not posted again)", secondCleared, firstCleared)
	}

	settlementTransactionRef, err := SettlementTransactionRef(batchID, merchantID)
	if err != nil {
		t.Fatalf("SettlementTransactionRef: %v", err)
	}
	var count int64
	row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE transaction_ref = $1`, settlementTransactionRef)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count journal_entries for %q: %v", settlementTransactionRef, err)
	}
	if count != 2 {
		t.Errorf("transaction_ref %q has %d row(s) after a retry, want exactly 2 (not doubled)", settlementTransactionRef, count)
	}

	merchantAccount, err := GetAccountByCode(ctx, conn, "merchant_payable:"+merchantID)
	if err != nil {
		t.Fatalf("GetAccountByCode(merchant_payable:%s): %v", merchantID, err)
	}
	merchantBalance, err := GetBalance(ctx, conn, merchantAccount.ID)
	if err != nil {
		t.Fatalf("GetBalance(merchant_payable): %v", err)
	}
	if merchantBalance != 0 {
		t.Errorf("merchant_payable balance after settling once and retrying = %d, want 0 (retry must not debit a second time)", merchantBalance)
	}
}

// TestPostSettlementClearingUsesOneTransactionRefPerMerchantPerBatchAgainstRealPostgres
// settles 2 different merchants within the same batchID and asserts each
// gets its own, distinct transaction_ref -- never one shared, unscoped
// entry mixing both merchants' money together.
func TestPostSettlementClearingUsesOneTransactionRefPerMerchantPerBatchAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	schedule := FeeSchedule{BasisPoints: 250}
	batchID := fmt.Sprintf("batch_multi_merchant_it_%d", time.Now().UnixNano())
	cutoff := time.Now().Add(time.Hour)

	merchantIDs := []string{
		fmt.Sprintf("mch_settle_multi_a_it_%d", time.Now().UnixNano()),
		fmt.Sprintf("mch_settle_multi_b_it_%d", time.Now().UnixNano()),
	}
	for i, merchantID := range merchantIDs {
		ref := fmt.Sprintf("txn_capture_multi_it_%d_%d", time.Now().UnixNano(), i)
		if err := PostCapture(ctx, conn, ref, merchantID, 100000, "INR", schedule); err != nil {
			t.Fatalf("seed PostCapture for %s: %v", merchantID, err)
		}
		if _, err := PostSettlementClearing(ctx, conn, batchID, merchantID, cutoff, "INR"); err != nil {
			t.Fatalf("PostSettlementClearing for %s: %v", merchantID, err)
		}
	}

	refs := make([]string, len(merchantIDs))
	for i, merchantID := range merchantIDs {
		ref, err := SettlementTransactionRef(batchID, merchantID)
		if err != nil {
			t.Fatalf("SettlementTransactionRef: %v", err)
		}
		refs[i] = ref
	}
	if refs[0] == refs[1] {
		t.Fatalf("both merchants resolved to the same transaction_ref %q, want distinct refs", refs[0])
	}

	for _, ref := range refs {
		var count int64
		row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE transaction_ref = $1`, ref)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("count journal_entries for %q: %v", ref, err)
		}
		if count != 2 {
			t.Errorf("transaction_ref %q has %d row(s), want exactly 2", ref, count)
		}
	}
}
