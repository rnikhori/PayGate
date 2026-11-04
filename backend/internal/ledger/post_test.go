package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// execCall records one ExecContext invocation against a fakeTx.
type execCall struct {
	query string
	args  []any
}

// fakeTx is a Tx that records every ExecContext/QueryRowContext call and
// whether Commit or Rollback was called, so tests can exercise
// PostJournalEntry's write path without a live database.
type fakeTx struct {
	execErr    error
	commitErr  error
	execCalls  []execCall
	committed  bool
	rolledBack bool
	execErrOnN int // if > 0, fail only the Nth ExecContext call (1-indexed)

	// queryRowCalls records every QueryRowContext call (claimTransactionRef
	// is, today, the only thing that issues one against a Tx).
	queryRowCalls []execCall

	// claimConflict, when true, makes QueryRowContext simulate
	// claimTransactionRef finding transaction_ref already claimed by an
	// earlier call — as if a journal_entry_postings row already existed —
	// reporting existingFingerprint back and claimed = false. The default
	// (false) simulates the common case every pre-existing PostJournalEntry
	// test relies on: a fresh transaction_ref, claimed for whatever
	// fingerprint this call passes in, so the entries insert proceeds
	// exactly as it did before this guard existed.
	claimConflict       bool
	existingFingerprint string
	queryRowErr         error
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

// QueryRowContext fakes claimTransactionRef's two-statement protocol (see
// post.go): the 1st call is always the claiming INSERT, which either
// succeeds (the default: a fresh transaction_ref, echoing back whatever
// fingerprint this call itself passed in) or — if claimConflict is set —
// reports sql.ErrNoRows, the way a real conflicted
// "ON CONFLICT DO NOTHING RETURNING ..." does. A 2nd call is only ever the
// follow-up SELECT claimTransactionRef issues after that sql.ErrNoRows,
// reading back existingFingerprint.
func (tx *fakeTx) QueryRowContext(_ context.Context, query string, args ...any) Row {
	tx.queryRowCalls = append(tx.queryRowCalls, execCall{query: query, args: args})
	call := len(tx.queryRowCalls)

	if tx.queryRowErr != nil && call == 1 {
		return fakeRow{scan: func(dest ...any) error { return tx.queryRowErr }}
	}

	scanString := func(value string) func(dest ...any) error {
		return func(dest ...any) error {
			if len(dest) != 1 {
				panic(fmt.Sprintf("fakeTx.QueryRowContext: Scan called with %d dest(s), want 1", len(dest)))
			}
			dst, ok := dest[0].(*string)
			if !ok {
				panic(fmt.Sprintf("fakeTx.QueryRowContext: dest[0] is %T, want *string", dest[0]))
			}
			*dst = value
			return nil
		}
	}

	if tx.claimConflict {
		if call == 1 {
			return fakeRow{scan: func(dest ...any) error { return sql.ErrNoRows }}
		}
		return fakeRow{scan: scanString(tx.existingFingerprint)}
	}

	// Fresh claim: echo back the fingerprint this call itself passed in
	// (claimTransactionRefInsertQuery's $2).
	fingerprint, _ := args[1].(string)
	return fakeRow{scan: scanString(fingerprint)}
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

// TestPostJournalEntryRetryWithIdenticalEntriesIsANoOp simulates
// transaction_ref already having been claimed (in journal_entry_postings)
// for the exact fingerprint this retry's entries produce — the "retried
// post with an identical entry set" acceptance criterion — and asserts it
// returns success without ever calling ExecContext (no duplicate rows) or
// Commit.
func TestPostJournalEntryRetryWithIdenticalEntriesIsANoOp(t *testing.T) {
	entries := balancedEntries("txn_post_retry_same")
	tx := &fakeTx{
		claimConflict:       true,
		existingFingerprint: fingerprintEntries(entries),
	}
	db := &fakeBeginner{tx: tx}

	if err := PostJournalEntry(context.Background(), db, "txn_post_retry_same", entries); err != nil {
		t.Fatalf("PostJournalEntry() = %v, want nil for an identical retry", err)
	}
	if len(tx.execCalls) != 0 {
		t.Errorf("PostJournalEntry called ExecContext %d time(s) for an identical retry, want 0 (no duplicate rows)", len(tx.execCalls))
	}
	if tx.committed {
		t.Error("PostJournalEntry committed a transaction for an identical retry that wrote nothing")
	}
	if !tx.rolledBack {
		t.Error("PostJournalEntry did not roll back the empty transaction for an identical retry")
	}
}

// TestPostJournalEntryRetryWithDifferentEntriesIsRejected simulates
// transaction_ref already claimed for a different fingerprint than this
// call's entries produce — the "retried post with a different entry set"
// acceptance criterion — and asserts it is rejected with a clear error
// wrapping ErrTransactionRefConflict, writing nothing.
func TestPostJournalEntryRetryWithDifferentEntriesIsRejected(t *testing.T) {
	entries := balancedEntries("txn_post_retry_diff")
	tx := &fakeTx{
		claimConflict:       true,
		existingFingerprint: "some-other-fingerprint-entirely",
	}
	db := &fakeBeginner{tx: tx}

	err := PostJournalEntry(context.Background(), db, "txn_post_retry_diff", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error for a conflicting retry")
	}
	if !errors.Is(err, ErrTransactionRefConflict) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap ErrTransactionRefConflict", err)
	}
	if len(tx.execCalls) != 0 {
		t.Errorf("PostJournalEntry called ExecContext %d time(s) for a conflicting retry, want 0", len(tx.execCalls))
	}
	if tx.committed {
		t.Error("PostJournalEntry committed a transaction for a rejected conflicting retry")
	}
	if !tx.rolledBack {
		t.Error("PostJournalEntry did not roll back after a conflicting retry")
	}
}

// TestPostJournalEntryReturnsErrorWhenClaimFails asserts a failure reading
// back the claim (a connection error, say) is surfaced as an error and
// rolls back, rather than silently proceeding to insert entries.
func TestPostJournalEntryReturnsErrorWhenClaimFails(t *testing.T) {
	wantErr := errors.New("connection reset")
	tx := &fakeTx{queryRowErr: wantErr}
	db := &fakeBeginner{tx: tx}

	entries := balancedEntries("txn_post_claim_fail")
	err := PostJournalEntry(context.Background(), db, "txn_post_claim_fail", entries)
	if err == nil {
		t.Fatal("PostJournalEntry() = nil error, want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap %v", err, wantErr)
	}
	if len(tx.execCalls) != 0 {
		t.Error("PostJournalEntry called ExecContext despite the claim query failing")
	}
	if !tx.rolledBack {
		t.Error("PostJournalEntry did not roll back after the claim query failed")
	}
}

// TestPostJournalEntryTxRetryWithIdenticalEntriesIsANoOp is
// TestPostJournalEntryRetryWithIdenticalEntriesIsANoOp's counterpart for
// PostJournalEntryTx: it must also detect an identical retry and write
// nothing, but — since it never owns tx — must not call Commit or
// Rollback either.
func TestPostJournalEntryTxRetryWithIdenticalEntriesIsANoOp(t *testing.T) {
	entries := balancedEntries("txn_posttx_retry_same")
	tx := &fakeTx{
		claimConflict:       true,
		existingFingerprint: fingerprintEntries(entries),
	}

	if err := PostJournalEntryTx(context.Background(), tx, "txn_posttx_retry_same", entries); err != nil {
		t.Fatalf("PostJournalEntryTx() = %v, want nil for an identical retry", err)
	}
	if len(tx.execCalls) != 0 {
		t.Errorf("PostJournalEntryTx called ExecContext %d time(s) for an identical retry, want 0", len(tx.execCalls))
	}
	if tx.committed || tx.rolledBack {
		t.Error("PostJournalEntryTx must never Commit or Rollback its caller's transaction")
	}
}

// TestPostJournalEntryTxRetryWithDifferentEntriesIsRejected is
// TestPostJournalEntryRetryWithDifferentEntriesIsRejected's counterpart for
// PostJournalEntryTx.
func TestPostJournalEntryTxRetryWithDifferentEntriesIsRejected(t *testing.T) {
	entries := balancedEntries("txn_posttx_retry_diff")
	tx := &fakeTx{
		claimConflict:       true,
		existingFingerprint: "some-other-fingerprint-entirely",
	}

	err := PostJournalEntryTx(context.Background(), tx, "txn_posttx_retry_diff", entries)
	if !errors.Is(err, ErrTransactionRefConflict) {
		t.Errorf("PostJournalEntryTx() error = %v, want it to wrap ErrTransactionRefConflict", err)
	}
	if len(tx.execCalls) != 0 {
		t.Errorf("PostJournalEntryTx called ExecContext %d time(s) for a conflicting retry, want 0", len(tx.execCalls))
	}
}

// TestFingerprintEntriesIsOrderIndependentAndDetectsDifferences asserts the
// fingerprint claimTransactionRef compares by: two slices with the same
// legs in a different order fingerprint identically (so a caller that
// doesn't control entry order still gets recognized as "the same
// transaction" on retry), while any change to an entry's account, amount,
// or currency changes the fingerprint (so claimTransactionRef can actually
// detect a conflicting retry).
func TestFingerprintEntriesIsOrderIndependentAndDetectsDifferences(t *testing.T) {
	a := []Entry{
		{AccountID: 1, Amount: 500, Currency: "INR"},
		{AccountID: 2, Amount: -500, Currency: "INR"},
	}
	reordered := []Entry{a[1], a[0]}
	if got, want := fingerprintEntries(reordered), fingerprintEntries(a); got != want {
		t.Errorf("fingerprintEntries(reordered) = %q, want %q (order-independent)", got, want)
	}

	differentAmount := []Entry{
		{AccountID: 1, Amount: 501, Currency: "INR"},
		{AccountID: 2, Amount: -501, Currency: "INR"},
	}
	if fingerprintEntries(differentAmount) == fingerprintEntries(a) {
		t.Error("fingerprintEntries did not change for a different amount")
	}

	differentAccount := []Entry{
		{AccountID: 3, Amount: 500, Currency: "INR"},
		{AccountID: 2, Amount: -500, Currency: "INR"},
	}
	if fingerprintEntries(differentAccount) == fingerprintEntries(a) {
		t.Error("fingerprintEntries did not change for a different account id")
	}
}
