//go:build integration

// Package ledger's trigger-level integration tests exercise the DB-level
// defense-in-depth guard added by backend/migrations/*_create_journal_entries
// (the journal_entries_balanced_transaction deferred constraint trigger)
// directly against a real, migrated Postgres database.
//
// They prove two things no amount of testing the Go-level
// ValidateBalancedTransaction check (see entry.go) can prove on its own:
// that the database itself rejects an unbalanced transaction_ref no matter
// which code path inserts the rows (not just calls that go through
// PostJournalEntry, see post.go), and that doing so costs an acceptable
// amount of write latency. See docs/ARCHITECTURE.md's "Ledger model"
// section for why that guard exists at all: a bug in some future caller
// that bypasses PostJournalEntry must not be able to corrupt the ledger.
//
// Not run by `go test ./...` (and so not part of `make test-backend` / CI)
// -- see balance_integration_test.go's package comment for how to run
// these against a throwaway database.
package ledger

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// TestRawInsertBypassingPostJournalEntryFailsAtCommit writes straight to
// journal_entries with database/sql -- never through PostJournalEntry or
// ValidateBalancedTransaction -- so the only thing that can catch an
// invalid "transaction" below is the database's own
// journal_entries_balanced_transaction deferred constraint trigger. This is
// this issue's acceptance criterion "Directly inserting an unbalanced set
// of rows via raw SQL fails the transaction".
func TestRawInsertBypassingPostJournalEntryFailsAtCommit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	acctA := mustCreateTestAccount(t, db, "raw_unbalanced_a")
	acctB := mustCreateTestAccount(t, db, "raw_unbalanced_b")

	const insert = `INSERT INTO journal_entries (transaction_ref, account_id, amount, currency) VALUES ($1, $2, $3, $4)`

	t.Run("legs do not sum to zero", func(t *testing.T) {
		ref := fmt.Sprintf("txn_raw_unbalanced_%d", time.Now().UnixNano())
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, ref, acctA, 500, "INR"); err != nil {
			t.Fatalf("insert leg 1: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, ref, acctB, -200, "INR"); err != nil {
			t.Fatalf("insert leg 2: %v", err)
		}
		// Both inserts succeed on their own: the deferred constraint
		// trigger doesn't run its check until COMMIT (see the migration's
		// DEFERRABLE INITIALLY DEFERRED), by which point every row this
		// transaction means to insert together is visible to it.
		if err := tx.Commit(); err == nil {
			t.Fatal("Commit() = nil, want an error: these two legs sum to 300, not 0")
		} else {
			t.Logf("commit correctly rejected: %v", err)
		}
	})

	t.Run("only one leg for the transaction_ref", func(t *testing.T) {
		ref := fmt.Sprintf("txn_raw_single_leg_%d", time.Now().UnixNano())
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, ref, acctA, 500, "INR"); err != nil {
			t.Fatalf("insert leg: %v", err)
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("Commit() = nil, want an error: a single leg is never a valid double-entry transaction")
		} else {
			t.Logf("commit correctly rejected: %v", err)
		}
	})

	t.Run("legs balance overall but not per currency", func(t *testing.T) {
		// Net amount across both rows is zero, but each individual
		// currency's legs are not -- this proves the trigger groups by
		// currency rather than summing every row regardless of currency
		// (see the migration's GROUP BY currency).
		ref := fmt.Sprintf("txn_raw_cross_currency_%d", time.Now().UnixNano())
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, ref, acctA, 500, "INR"); err != nil {
			t.Fatalf("insert leg 1: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, ref, acctB, -500, "USD"); err != nil {
			t.Fatalf("insert leg 2: %v", err)
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("Commit() = nil, want an error: INR and USD legs don't balance each other")
		} else {
			t.Logf("commit correctly rejected: %v", err)
		}
	})
}

// TestJournalEntryTriggerOverhead covers this issue's acceptance criterion
// "Trigger overhead is measured and acceptable (<10% write latency increase
// in benchmark)": it posts the smallest real transaction PostJournalEntry
// ever writes -- one balanced pair -- many times with the
// journal_entries_balanced_transaction trigger disabled, then again with it
// enabled, and compares median wall-clock latency between the two.
//
// The percentage this measures is sensitive to the database connection's
// own round-trip latency, which the trigger's few extra server-side index
// lookups are only a fraction of: over a Unix-socket DSN (near-zero
// baseline latency) the trigger's fixed per-leg cost reads as a much larger
// percentage than it does over the TCP connection PayGate actually uses in
// every real deployment (see infra/docker-compose.yml and
// backend/internal/config -- DATABASE_URL is always a "postgres://host:port/
// ..." address, never a socket path). Point TEST_DATABASE_URL at a TCP
// address, not a Unix-socket one, for a representative number; see this
// package's doc comment on testDB in balance_integration_test.go for how
// TEST_DATABASE_URL is resolved.
func TestJournalEntryTriggerOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency benchmark in -short mode")
	}

	db := testDB(t)
	ctx := context.Background()
	conn := Wrap(db)
	acctA := mustCreateTestAccount(t, db, "overhead_a")
	acctB := mustCreateTestAccount(t, db, "overhead_b")

	const (
		triggerName  = "journal_entries_balanced_transaction"
		txnsPerTrial = 1000
		trials       = 7
		// This issue's acceptance criterion: trigger overhead must stay
		// under a 10% write latency increase.
		maxAllowedRatio = 0.10
	)

	post := func(prefix string, n int) time.Duration {
		t.Helper()
		start := time.Now()
		for i := 0; i < n; i++ {
			ref := fmt.Sprintf("txn_overhead_%s_%d_%d", prefix, time.Now().UnixNano(), i)
			entries := []Entry{
				{AccountID: acctA, Amount: 100, Currency: "INR"},
				{AccountID: acctB, Amount: -100, Currency: "INR"},
			}
			if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
				t.Fatalf("PostJournalEntry: %v", err)
			}
		}
		return time.Since(start)
	}

	median := func(ds []time.Duration) time.Duration {
		sorted := append([]time.Duration(nil), ds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[len(sorted)/2]
	}

	setTrigger := func(enabled bool) {
		t.Helper()
		verb := "DISABLE"
		if enabled {
			verb = "ENABLE"
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE journal_entries %s TRIGGER %s", verb, triggerName)); err != nil {
			t.Fatalf("%s TRIGGER %s: %v", verb, triggerName, err)
		}
	}

	// Always leave the trigger enabled afterwards, regardless of how (or
	// whether) the test below fails -- every other test in this package,
	// and any other table traffic on a shared throwaway database, depends
	// on the ledger's real invariant being enforced.
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			fmt.Sprintf("ALTER TABLE journal_entries ENABLE TRIGGER %s", triggerName)); err != nil {
			t.Errorf("re-enable trigger during cleanup: %v", err)
		}
	})

	// Warm up the connection pool and query plan cache identically before
	// any measurement, so neither phase below is penalized for paying a
	// one-time cost the other phase already paid.
	post("warmup", 50)

	setTrigger(false)
	withoutTrigger := make([]time.Duration, trials)
	for i := range withoutTrigger {
		withoutTrigger[i] = post("without", txnsPerTrial)
	}

	setTrigger(true)
	withTrigger := make([]time.Duration, trials)
	for i := range withTrigger {
		withTrigger[i] = post("with", txnsPerTrial)
	}

	baseline := median(withoutTrigger)
	withGuard := median(withTrigger)
	ratio := float64(withGuard-baseline) / float64(baseline)

	t.Logf("median latency over %d PostJournalEntry calls, %d trials each: without trigger %v, with trigger %v (%+.1f%% change)",
		txnsPerTrial, trials, baseline, withGuard, ratio*100)

	if ratio > maxAllowedRatio {
		t.Errorf("journal_entries_balanced_transaction adds %.1f%% write latency (without=%v, with=%v over %d txns), want <%.0f%% per this issue's acceptance criteria",
			ratio*100, baseline, withGuard, txnsPerTrial, maxAllowedRatio*100)
	}
}
