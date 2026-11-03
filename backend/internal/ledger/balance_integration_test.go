//go:build integration

// Package ledger's integration tests exercise GetBalance (see balance.go)
// against a real, migrated Postgres database instead of the fakes
// balance_test.go uses — the only way to actually prove the SQL aggregation
// itself is correct and fast, as opposed to proving GetBalance plumbs a
// fake's result through unchanged.
//
// Not run by `go test ./...` (and so not part of `make test-backend` / CI):
// these need a live, already-migrated database. Run them explicitly against
// a throwaway database (they append real, permanent rows to journal_entries
// — by design, see docs/ARCHITECTURE.md's "Ledger model" section — so don't
// point this at a long-lived dev database you care about staying small):
//
//	docker compose -f infra/docker-compose.yml up -d
//	DATABASE_URL=postgres://paygate:paygate@localhost:5432/paygate?sslmode=disable \
//	  go run ./cmd/migrate up
//	TEST_DATABASE_URL=postgres://paygate:paygate@localhost:5432/paygate?sslmode=disable \
//	  go test -tags=integration ./internal/ledger/... -run TestGetBalance -v
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// testDB opens a connection to the database named by TEST_DATABASE_URL
// (falling back to DATABASE_URL, so a local backend/.env-configured
// database works without extra setup), skipping the test entirely if
// neither is set — these tests are opt-in, never a silent requirement of
// `go test ./...`.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL (or DATABASE_URL) not set; skipping integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v (is Postgres up and migrated? see this file's package comment)", dsn, err)
	}
	return db
}

// mustCreateTestAccount inserts a fresh ledger_accounts row with a unique
// code (so concurrent/repeated test runs never collide) and returns its id.
func mustCreateTestAccount(t *testing.T, db *sql.DB, label string) int64 {
	t.Helper()

	code := fmt.Sprintf("test_balance_%s_%d", label, time.Now().UnixNano())
	var id int64
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO ledger_accounts (code, type, name) VALUES ($1, 'asset', $2) RETURNING id`,
		code, "integration test account: "+label,
	).Scan(&id)
	if err != nil {
		t.Fatalf("create test account %q: %v", code, err)
	}
	return id
}

// TestGetBalanceMatchesHandComputedSumAgainstRealPostgres posts a mixed set
// of balanced transactions — single-pair and multi-leg, credits and debits,
// varying magnitudes — against a real database via PostJournalEntry (the
// same write path every other package uses, see post.go), then asserts
// GetBalance's result, computed by real SQL against the real
// journal_entries table, matches a sum this test computes by hand in Go as
// it builds those transactions.
func TestGetBalanceMatchesHandComputedSumAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acctA := mustCreateTestAccount(t, db, "mixed_a")
	acctB := mustCreateTestAccount(t, db, "mixed_b")
	acctC := mustCreateTestAccount(t, db, "mixed_c")

	type leg struct {
		account int64
		amount  int64
	}
	transactions := [][]leg{
		{{acctA, 150000}, {acctB, -150000}},        // simple credit to A
		{{acctA, -42000}, {acctB, 42000}},          // simple debit from A
		{{acctA, 999}, {acctB, -1}, {acctC, -998}}, // multi-leg credit to A
		{{acctA, -500000}, {acctB, 250000}, {acctC, 250000}},
		{{acctA, 1}, {acctB, -1}},
		{{acctA, -1}, {acctB, 1}},
		{{acctA, 75000}, {acctC, -75000}},
		{{acctA, -75000}, {acctC, 75000}},
		{{acctA, 250}, {acctB, -250}},
	}

	var handComputed int64
	for i, txn := range transactions {
		entries := make([]Entry, 0, len(txn))
		for _, l := range txn {
			entries = append(entries, Entry{AccountID: l.account, Amount: l.amount, Currency: "INR"})
			if l.account == acctA {
				handComputed += l.amount
			}
		}
		ref := fmt.Sprintf("txn_balance_mixed_%d_%d", time.Now().UnixNano(), i)
		if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
			t.Fatalf("PostJournalEntry(%d): %v", i, err)
		}
	}

	got, err := GetBalance(ctx, conn, acctA)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got != handComputed {
		t.Errorf("GetBalance(acctA) = %d, want hand-computed sum %d", got, handComputed)
	}

	// Sanity: GetBalance must filter by account_id, not sum every row in
	// the table — acctB received the opposite legs of several of the same
	// transactions above, so its balance should differ from acctA's.
	gotB, err := GetBalance(ctx, conn, acctB)
	if err != nil {
		t.Fatalf("GetBalance(acctB): %v", err)
	}
	if gotB == got {
		t.Errorf("GetBalance(acctB) = %d, same as acctA's %d; want it filtered independently", gotB, got)
	}
}

// TestGetBalanceUnder100msAt100kEntries seeds one account with 100,000
// journal_entries rows (50,000 balanced pairs) and asserts a single
// GetBalance call against it completes in under 100ms, per this issue's
// acceptance criteria. It seeds via direct batched SQL inserts rather than
// 50,000 individual PostJournalEntry calls purely so setup itself finishes
// in reasonable time — the thing under measurement is GetBalance, not the
// write path post_test.go and the test above already cover.
func TestGetBalanceUnder100msAt100kEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100k-row seed in -short mode")
	}

	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acct := mustCreateTestAccount(t, db, "perf")
	counterparty := mustCreateTestAccount(t, db, "perf_counterparty")

	const (
		totalPairs     = 50000 // 100,000 rows total
		pairsPerBatch  = 1000  // 2,000 rows per batch/transaction
		amountPerEntry = int64(100)
	)

	for batchStart := 0; batchStart < totalPairs; batchStart += pairsPerBatch {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}

		query := `INSERT INTO journal_entries (transaction_ref, account_id, amount, currency) VALUES `
		args := make([]any, 0, pairsPerBatch*2*4)
		for i := 0; i < pairsPerBatch; i++ {
			n := batchStart + i
			ref := fmt.Sprintf("txn_balance_perf_%d_%d", time.Now().UnixNano(), n)
			base := len(args)
			if i > 0 {
				query += ", "
			}
			query += fmt.Sprintf("($%d,$%d,$%d,$%d), ($%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
			args = append(args,
				ref, acct, amountPerEntry, "INR",
				ref, counterparty, -amountPerEntry, "INR",
			)
		}

		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed batch at %d: %v", batchStart, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed batch at %d: %v", batchStart, err)
		}
	}

	start := time.Now()
	got, err := GetBalance(ctx, conn, acct)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}

	want := int64(totalPairs) * amountPerEntry
	if got != want {
		t.Errorf("GetBalance() = %d, want %d (%d entries of %d)", got, want, totalPairs, amountPerEntry)
	}

	t.Logf("GetBalance over %d entries took %s", totalPairs*2, elapsed)
	if elapsed > 100*time.Millisecond {
		t.Errorf("GetBalance took %s for %d entries, want <100ms", elapsed, totalPairs*2)
	}
}
