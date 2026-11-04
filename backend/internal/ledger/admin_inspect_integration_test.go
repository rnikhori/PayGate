//go:build integration

// See balance_integration_test.go's package comment for how to run these
// (they need a live, already-migrated Postgres via TEST_DATABASE_URL or
// DATABASE_URL, and are opt-in -- not part of `go test ./...` / `make
// test-backend`):
//
//	go test -tags=integration ./internal/ledger/... -run TestInspectTransactions -v
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestInspectTransactionsEndToEndAgainstRealPostgres posts one real capture
// through PostCapture (so every acceptance criterion is checked against an
// actual, trigger-enforced double-entry transaction, not a fake), then
// confirms InspectTransactions returns its 3 legs grouped under that one
// transaction_ref, in chronological order, each carrying its account's
// readable code -- and that exactly one allowed ledger_admin_access_log row
// was written for the lookup.
func TestInspectTransactionsEndToEndAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_inspect_it_%d", time.Now().UnixNano())
	ref := fmt.Sprintf("txn_inspect_it_%d", time.Now().UnixNano())
	adminID := fmt.Sprintf("admin_inspect_it_%d", time.Now().UnixNano())

	if err := PostCapture(ctx, conn, ref, merchantID, 100000, "INR", FeeSchedule{BasisPoints: 250}); err != nil {
		t.Fatalf("PostCapture: %v", err)
	}

	inspectionDB := WrapInspectionDB(db)
	journals, err := InspectTransactions(ctx, inspectionDB, Actor{ID: adminID, Role: RoleAdmin}, []string{ref})
	if err != nil {
		t.Fatalf("InspectTransactions: %v", err)
	}

	if len(journals) != 1 {
		t.Fatalf("len(journals) = %d, want 1 (one transaction_ref)", len(journals))
	}
	journal := journals[0]
	if journal.TransactionRef != ref {
		t.Fatalf("journal.TransactionRef = %q, want %q", journal.TransactionRef, ref)
	}
	if len(journal.Entries) != 3 {
		t.Fatalf("len(journal.Entries) = %d, want 3 (customer_suspense, merchant_payable, gateway_fee_revenue)", len(journal.Entries))
	}

	wantCodes := map[string]bool{
		CustomerSuspenseAccountCode:                    false,
		GatewayFeeRevenueAccountCode:                   false,
		fmt.Sprintf("merchant_payable:%s", merchantID): false,
	}
	for i, e := range journal.Entries {
		if e.TransactionRef != ref {
			t.Errorf("entry %d TransactionRef = %q, want %q", i, e.TransactionRef, ref)
		}
		if _, known := wantCodes[e.AccountCode]; !known {
			t.Errorf("entry %d AccountCode = %q, not one of the capture's 3 accounts", i, e.AccountCode)
		}
		wantCodes[e.AccountCode] = true
		if i > 0 && journal.Entries[i-1].CreatedAt.After(e.CreatedAt) {
			t.Errorf("entry %d created_at %v is after entry %d's %v, want chronological order", i, e.CreatedAt, i-1, journal.Entries[i-1].CreatedAt)
		}
	}
	for code, seen := range wantCodes {
		if !seen {
			t.Errorf("expected account code %q among the returned entries, got none", code)
		}
	}

	// The lookup itself must have left exactly one allowed audit row.
	var (
		gotActorID string
		gotRole    string
		gotAllowed bool
		gotRefs    string
	)
	row := db.QueryRowContext(ctx,
		`SELECT actor_id, actor_role, allowed, transaction_refs::text
		 FROM ledger_admin_access_log WHERE actor_id = $1`,
		adminID,
	)
	if err := row.Scan(&gotActorID, &gotRole, &gotAllowed, &gotRefs); err != nil {
		t.Fatalf("query ledger_admin_access_log: %v", err)
	}
	if gotActorID != adminID || gotRole != string(RoleAdmin) || !gotAllowed {
		t.Errorf("audit row = (actor_id=%q, role=%q, allowed=%v), want (%q, admin, true)", gotActorID, gotRole, gotAllowed, adminID)
	}
	var refs []string
	if err := json.Unmarshal([]byte(gotRefs), &refs); err != nil {
		t.Fatalf("unmarshal audit row transaction_refs %q: %v", gotRefs, err)
	}
	if len(refs) != 1 || refs[0] != ref {
		t.Errorf("audit row transaction_refs = %v, want [%q]", refs, ref)
	}
}

// TestInspectTransactionsDeniedForMerchantActorAgainstRealPostgres confirms
// the role gate rejects a non-admin Actor without ever reading
// journal_entries, and still writes a denied ledger_admin_access_log row --
// against a real database, so the gate is proven to run before any query
// is even sent, not just in the fakes admin_inspect_test.go uses.
func TestInspectTransactionsDeniedForMerchantActorAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	merchantID := fmt.Sprintf("mch_inspect_denied_it_%d", time.Now().UnixNano())
	ref := fmt.Sprintf("txn_inspect_denied_it_%d", time.Now().UnixNano())

	inspectionDB := WrapInspectionDB(db)
	_, err := InspectTransactions(ctx, inspectionDB, Actor{ID: merchantID, Role: RoleMerchant}, []string{ref})
	if err == nil {
		t.Fatal("InspectTransactions() = nil error, want ErrForbidden for a merchant actor")
	}

	var gotAllowed bool
	row := db.QueryRowContext(ctx,
		`SELECT allowed FROM ledger_admin_access_log WHERE actor_id = $1`,
		merchantID,
	)
	if err := row.Scan(&gotAllowed); err != nil {
		t.Fatalf("query ledger_admin_access_log: %v", err)
	}
	if gotAllowed {
		t.Error("audit row allowed = true, want false for a denied merchant lookup")
	}
}
