//go:build integration

// See balance_integration_test.go's package comment for how to run these:
// a live, already-migrated Postgres is required (not part of `go test
// ./...` / CI).
package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestCheckIntegrityAgainstRealPostgresFindsNoUnbalancedTransactions posts a
// handful of real, balanced transactions via PostJournalEntry (the same
// write path every other package uses) against a live database, then runs
// CheckIntegrity for real -- this is the only way to actually prove
// unbalancedTransactionsQuery's array_agg/JOIN and accountCodeArray's
// parsing of what Postgres actually sends back are correct together,
// as opposed to proving CheckIntegrity plumbs a fake's result through
// unchanged (integrity_test.go already covers that).
//
// It does not try to manufacture an actual unbalanced row: the
// journal_entries_balanced_transaction trigger (see
// backend/migrations/*_create_journal_entries) already rejects any write
// that would create one, through this package's own write path or any
// other -- which is exactly the invariant CheckIntegrity exists to audit
// from the outside, not a scenario this test can trigger without disabling
// that trigger first.
func TestCheckIntegrityAgainstRealPostgresFindsNoUnbalancedTransactions(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acctA := mustCreateTestAccount(t, db, "integrity_a")
	acctB := mustCreateTestAccount(t, db, "integrity_b")

	for i := 0; i < 5; i++ {
		ref := fmt.Sprintf("txn_integrity_%d_%d", time.Now().UnixNano(), i)
		entries := []Entry{
			{AccountID: acctA, Amount: 1000, Currency: "INR"},
			{AccountID: acctB, Amount: -1000, Currency: "INR"},
		}
		if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
			t.Fatalf("PostJournalEntry(%d): %v", i, err)
		}
	}

	report, err := CheckIntegrity(ctx, WrapStatementDB(db))
	if err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
	if report.GlobalSum != 0 {
		t.Errorf("report.GlobalSum = %d, want 0 across the real journal_entries table", report.GlobalSum)
	}
	if len(report.UnbalancedTransactions) != 0 {
		t.Errorf("report.UnbalancedTransactions = %+v, want none against a real, trigger-protected database", report.UnbalancedTransactions)
	}
	if !report.Passed() {
		t.Error("report.Passed() = false, want true")
	}
}

// TestRecordIntegrityCheckAgainstRealPostgresRoundTrips persists a report
// via RecordIntegrityCheck against a live database and reads the row back
// with a plain query, asserting every column -- including details, a real
// JSONB column -- round-trips the way integrity_test.go's fakes assume.
func TestRecordIntegrityCheckAgainstRealPostgresRoundTrips(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	report := IntegrityReport{
		CheckedAt: time.Now().UTC().Truncate(time.Microsecond),
		GlobalSum: 0,
	}

	id, err := RecordIntegrityCheck(ctx, Wrap(db), ManualIntegrityCheck, report)
	if err != nil {
		t.Fatalf("RecordIntegrityCheck: %v", err)
	}
	if id == 0 {
		t.Fatal("RecordIntegrityCheck returned id 0, want a real row id")
	}

	var (
		gotTrigger string
		gotPassed  bool
		gotSum     int64
		gotCount   int
		gotDetails string
	)
	row := db.QueryRowContext(ctx,
		`SELECT trigger, passed, global_sum, unbalanced_count, details::text FROM ledger_integrity_checks WHERE id = $1`,
		id,
	)
	if err := row.Scan(&gotTrigger, &gotPassed, &gotSum, &gotCount, &gotDetails); err != nil {
		t.Fatalf("read back ledger_integrity_checks row %d: %v", id, err)
	}

	if gotTrigger != string(ManualIntegrityCheck) {
		t.Errorf("trigger = %q, want %q", gotTrigger, ManualIntegrityCheck)
	}
	if !gotPassed {
		t.Error("passed = false, want true for a zero-sum, no-unbalanced-transactions report")
	}
	if gotSum != 0 {
		t.Errorf("global_sum = %d, want 0", gotSum)
	}
	if gotCount != 0 {
		t.Errorf("unbalanced_count = %d, want 0", gotCount)
	}
	if gotDetails != "{}" {
		t.Errorf("details = %q, want \"{}\" for a report with no unbalanced transactions", gotDetails)
	}
}
