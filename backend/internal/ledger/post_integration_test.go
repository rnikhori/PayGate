//go:build integration

// Package ledger's integration test for PostJournalEntry's idempotency
// guard (see post.go and
// backend/migrations/*_create_journal_entry_postings) exercises the three
// things post_test.go's fakes cannot prove on their own: that
// transaction_ref really does carry a uniqueness constraint at the DB
// level, that a real retry (a second, separate PostJournalEntry call, not
// a pre-seeded fake) is recognized, and that two goroutines racing to post
// the same transaction_ref against a real database resolve to exactly one
// set of rows rather than a duplicate.
//
// Not run by `go test ./...` (and so not part of `make test-backend` / CI):
// see balance_integration_test.go's package comment for how to run these
// against a throwaway database.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestJournalEntryPostingsTransactionRefIsUniqueAtTheDBLevel inserts two
// journal_entry_postings rows for the same transaction_ref directly via
// SQL — bypassing claimTransactionRef and PostJournalEntry entirely — and
// asserts the second insert is rejected by the table's own PRIMARY KEY,
// proving the uniqueness constraint this issue's first acceptance
// criterion calls for is enforced by the database itself, not only by
// claimTransactionRef's application-level logic.
func TestJournalEntryPostingsTransactionRefIsUniqueAtTheDBLevel(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	ref := fmt.Sprintf("txn_post_it_unique_%d", time.Now().UnixNano())

	const insert = `INSERT INTO journal_entry_postings (transaction_ref, fingerprint) VALUES ($1, $2)`
	if _, err := db.ExecContext(ctx, insert, ref, "fingerprint-a"); err != nil {
		t.Fatalf("first insert for %q: %v", ref, err)
	}

	_, err := db.ExecContext(ctx, insert, ref, "fingerprint-b")
	if err == nil {
		t.Fatalf("second insert for transaction_ref %q succeeded, want a uniqueness violation", ref)
	}
	t.Logf("second insert for duplicate transaction_ref correctly rejected: %v", err)
}

// TestPostJournalEntryRetryWithIdenticalEntriesAgainstRealPostgres posts
// the same transactionRef and entries twice, via two entirely separate
// PostJournalEntry calls, and asserts: both calls succeed, and exactly one
// set of rows exists afterward — the second call did not insert a
// duplicate pair.
func TestPostJournalEntryRetryWithIdenticalEntriesAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acctA := mustCreateTestAccount(t, db, "idem_retry_a")
	acctB := mustCreateTestAccount(t, db, "idem_retry_b")
	ref := fmt.Sprintf("txn_post_it_retry_same_%d", time.Now().UnixNano())
	entries := []Entry{
		{AccountID: acctA, Amount: 12345, Currency: "INR"},
		{AccountID: acctB, Amount: -12345, Currency: "INR"},
	}

	if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
		t.Fatalf("first PostJournalEntry: %v", err)
	}
	if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
		t.Fatalf("retried PostJournalEntry with an identical entry set returned an error, want nil: %v", err)
	}

	var rowCount int
	row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE transaction_ref = $1`, ref)
	if err := row.Scan(&rowCount); err != nil {
		t.Fatalf("count rows for %q: %v", ref, err)
	}
	if rowCount != len(entries) {
		t.Errorf("journal_entries has %d row(s) for %q after a retried identical post, want %d (no duplicate rows)", rowCount, ref, len(entries))
	}
}

// TestPostJournalEntryRetryWithDifferentEntriesAgainstRealPostgres posts
// transactionRef once, then again with a different (still individually
// balanced) entry set, and asserts: the second call is rejected wrapping
// ErrTransactionRefConflict, and only the first call's rows exist
// afterward.
func TestPostJournalEntryRetryWithDifferentEntriesAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acctA := mustCreateTestAccount(t, db, "idem_conflict_a")
	acctB := mustCreateTestAccount(t, db, "idem_conflict_b")
	ref := fmt.Sprintf("txn_post_it_retry_diff_%d", time.Now().UnixNano())

	first := []Entry{
		{AccountID: acctA, Amount: 500, Currency: "INR"},
		{AccountID: acctB, Amount: -500, Currency: "INR"},
	}
	if err := PostJournalEntry(ctx, conn, ref, first); err != nil {
		t.Fatalf("first PostJournalEntry: %v", err)
	}

	second := []Entry{
		{AccountID: acctA, Amount: 999, Currency: "INR"},
		{AccountID: acctB, Amount: -999, Currency: "INR"},
	}
	err := PostJournalEntry(ctx, conn, ref, second)
	if err == nil {
		t.Fatal("PostJournalEntry with a different entry set for an already-posted transaction_ref returned nil, want an error")
	}
	if !errors.Is(err, ErrTransactionRefConflict) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap ErrTransactionRefConflict", err)
	}

	gotBalance, err := GetBalance(ctx, conn, acctA)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if gotBalance != 500 {
		t.Errorf("GetBalance(acctA) = %d after a rejected conflicting retry, want 500 (only the first post's rows)", gotBalance)
	}
}

// TestPostJournalEntryConcurrentRetriesProduceExactlyOneSetOfRows is this
// issue's concurrency acceptance criterion: two goroutines call
// PostJournalEntry for the same transaction_ref and the same entries at
// the same time. Exactly one set of rows must exist afterward — neither a
// duplicate pair (the race this issue's guard exists to close) nor zero
// rows (both calls losing).
func TestPostJournalEntryConcurrentRetriesProduceExactlyOneSetOfRows(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	ctx := context.Background()

	acctA := mustCreateTestAccount(t, db, "idem_concurrent_a")
	acctB := mustCreateTestAccount(t, db, "idem_concurrent_b")
	ref := fmt.Sprintf("txn_post_it_concurrent_%d", time.Now().UnixNano())
	entries := []Entry{
		{AccountID: acctA, Amount: 7700, Currency: "INR"},
		{AccountID: acctB, Amount: -7700, Currency: "INR"},
	}

	const goroutines = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		readyWg sync.WaitGroup
	)
	readyWg.Add(goroutines)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readyWg.Done()
			<-start // line every goroutine up before any of them posts
			if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	readyWg.Wait()
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent PostJournalEntry call returned an error, want every call to succeed (either by claiming %q or recognizing the identical retry): %v", ref, err)
	}

	var rowCount int
	row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE transaction_ref = $1`, ref)
	if err := row.Scan(&rowCount); err != nil {
		t.Fatalf("count rows for %q: %v", ref, err)
	}
	if rowCount != len(entries) {
		t.Errorf("journal_entries has %d row(s) for %q after %d concurrent identical posts, want exactly %d (one set of rows)", rowCount, ref, goroutines, len(entries))
	}
}
