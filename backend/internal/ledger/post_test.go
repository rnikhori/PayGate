package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// execCall records one ExecContext invocation against a fakeTx.
type execCall struct {
	query string
	args  []any
}

// fakeTx is a Tx that records every ExecContext call and whether Commit or
// Rollback was called, so tests can exercise PostJournalEntry's write path
// without a live database.
type fakeTx struct {
	execErr    error
	commitErr  error
	execCalls  []execCall
	committed  bool
	rolledBack bool
	execErrOnN int // if > 0, fail only the Nth ExecContext call (1-indexed)
}

func (tx *fakeTx) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	tx.execCalls = append(tx.execCalls, execCall{query: query, args: args})
	if tx.execErrOnN > 0 && len(tx.execCalls) == tx.execErrOnN {
		return nil, tx.execErr
	}
	if tx.execErrOnN == 0 && tx.execErr != nil {
		return nil, tx.execErr
	}
	return nil, nil
}

func (tx *fakeTx) Commit() error {
	tx.committed = true
	return tx.commitErr
}

func (tx *fakeTx) Rollback() error {
	tx.rolledBack = true
	return nil
}

// fakeBeginner is a Beginner that records whether BeginTx was called and
// hands back a fixed Tx (or error).
type fakeBeginner struct {
	tx         *fakeTx
	beginErr   error
	beginCalls int
}

func (b *fakeBeginner) BeginTx(context.Context, *sql.TxOptions) (Tx, error) {
	b.beginCalls++
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

func balancedEntries(ref string) []Entry {
	return []Entry{
		{TransactionRef: ref, AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: ref, AccountID: 2, Amount: -10000, Currency: "INR"},
	}
}

func TestPostJournalEntryWritesEveryEntryAndCommits(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	entries := balancedEntries("txn_post_1")
	if err := PostJournalEntry(context.Background(), db, "txn_post_1", entries); err != nil {
		t.Fatalf("PostJournalEntry() = %v, want nil", err)
	}

	if db.beginCalls != 1 {
		t.Errorf("BeginTx called %d times, want 1", db.beginCalls)
	}
	if len(tx.execCalls) != len(entries) {
		t.Fatalf("ExecContext called %d times, want %d", len(tx.execCalls), len(entries))
	}
	for i, call := range tx.execCalls {
		wantArgs := []any{"txn_post_1", entries[i].AccountID, entries[i].Amount, entries[i].Currency}
		if len(call.args) != len(wantArgs) {
			t.Fatalf("ExecContext call %d args = %v, want %v", i, call.args, wantArgs)
		}
		for j, got := range call.args {
			if got != wantArgs[j] {
				t.Errorf("ExecContext call %d args[%d] = %v, want %v", i, j, got, wantArgs[j])
			}
		}
	}
	if !tx.committed {
		t.Error("PostJournalEntry did not Commit the transaction")
	}
	if tx.rolledBack {
		t.Error("PostJournalEntry rolled back a transaction that should have committed")
	}
}

func TestPostJournalEntryStampsTransactionRefOntoEveryEntry(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	// Entries deliberately carry a stale/mismatched TransactionRef; the
	// transactionRef argument must win for every row written.
	entries := []Entry{
		{TransactionRef: "stale_ref", AccountID: 1, Amount: 500, Currency: "INR"},
		{TransactionRef: "other_stale_ref", AccountID: 2, Amount: -500, Currency: "INR"},
	}

	if err := PostJournalEntry(context.Background(), db, "txn_post_2", entries); err != nil {
		t.Fatalf("PostJournalEntry() = %v, want nil", err)
	}

	for i, call := range tx.execCalls {
		if call.args[0] != "txn_post_2" {
			t.Errorf("ExecContext call %d args[0] = %v, want %q", i, call.args[0], "txn_post_2")
		}
	}
}

func TestPostJournalEntryRejectsUnbalancedEntriesWithoutTouchingDB(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	entries := []Entry{
		{AccountID: 1, Amount: 10000, Currency: "INR"},
		{AccountID: 2, Amount: -9000, Currency: "INR"},
	}

	err := PostJournalEntry(context.Background(), db, "txn_post_3", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error for unbalanced entries")
	}
	if !errors.Is(err, ErrUnbalancedTransaction) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap ErrUnbalancedTransaction", err)
	}
	if db.beginCalls != 0 {
		t.Error("PostJournalEntry called BeginTx for an unbalanced transaction (partial write risk)")
	}
	if len(tx.execCalls) != 0 {
		t.Error("PostJournalEntry called ExecContext for an unbalanced transaction (partial write risk)")
	}
}

func TestPostJournalEntryRejectsEmptyTransactionRef(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	entries := balancedEntries("")
	if err := PostJournalEntry(context.Background(), db, "", entries); err == nil {
		t.Error("PostJournalEntry(..., \"\", ...) = nil error, want an error")
	}
	if db.beginCalls != 0 {
		t.Error("PostJournalEntry called BeginTx for an empty transaction_ref")
	}
}

func TestPostJournalEntryRollsBackAndReturnsErrorWhenExecFails(t *testing.T) {
	wantErr := errors.New("boom: constraint violation")
	tx := &fakeTx{execErr: wantErr}
	db := &fakeBeginner{tx: tx}

	entries := balancedEntries("txn_post_4")
	err := PostJournalEntry(context.Background(), db, "txn_post_4", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap %v", err, wantErr)
	}
	if !tx.rolledBack {
		t.Error("PostJournalEntry did not roll back after an ExecContext failure")
	}
	if tx.committed {
		t.Error("PostJournalEntry committed a transaction that failed to write")
	}
}

func TestPostJournalEntryReturnsErrorWhenBeginTxFails(t *testing.T) {
	wantErr := errors.New("connection refused")
	db := &fakeBeginner{beginErr: wantErr}

	entries := balancedEntries("txn_post_5")
	err := PostJournalEntry(context.Background(), db, "txn_post_5", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestPostJournalEntryReturnsErrorWhenCommitFails(t *testing.T) {
	wantErr := errors.New("commit failed: deferred constraint violated")
	tx := &fakeTx{commitErr: wantErr}
	db := &fakeBeginner{tx: tx}

	entries := balancedEntries("txn_post_6")
	err := PostJournalEntry(context.Background(), db, "txn_post_6", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap %v", err, wantErr)
	}
	if tx.rolledBack {
		t.Error("PostJournalEntry rolled back after Commit was already called")
	}
}
