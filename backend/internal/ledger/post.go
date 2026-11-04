package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// insertJournalEntryQuery mirrors the journal_entries columns this package
// (and only this package — see doc.go and PostJournalEntry below) is
// permitted to write: backend/migrations/*_create_journal_entries grants
// paygate_app nothing beyond SELECT/INSERT on the table, and never grants
// UPDATE/DELETE to any role.
const insertJournalEntryQuery = `
	INSERT INTO journal_entries (transaction_ref, account_id, amount, currency)
	VALUES ($1, $2, $3, $4)`

// ErrTransactionRefConflict is the domain error claimTransactionRef wraps
// into its returned error when transactionRef was already used to post a
// different set of entries than this call is attempting. It is distinct
// from ErrUnbalancedTransaction and from any other database error, so a
// caller can tell "this retry doesn't match what was posted before" apart
// from either with errors.Is(err, ledger.ErrTransactionRefConflict).
var ErrTransactionRefConflict = errors.New("ledger: transaction_ref already used to post a different entry set")

// claimTransactionRefInsertQuery is claimTransactionRef's first attempt:
// claim transactionRef in journal_entry_postings
// (backend/migrations/*_create_journal_entry_postings) for fingerprint. If
// transactionRef has no row yet, the table's PRIMARY KEY lets this succeed
// and RETURNING hands the row straight back. If it conflicts (a row
// already exists), ON CONFLICT DO NOTHING makes it a no-op that returns no
// row at all — Scan reports that as sql.ErrNoRows, which claimTransactionRef
// takes as its cue to fall back to claimTransactionRefSelectQuery.
const claimTransactionRefInsertQuery = `
	INSERT INTO journal_entry_postings (transaction_ref, fingerprint)
	VALUES ($1, $2)
	ON CONFLICT (transaction_ref) DO NOTHING
	RETURNING fingerprint`

// claimTransactionRefSelectQuery reads back whichever fingerprint
// transactionRef was actually claimed with, once claimTransactionRef knows
// (from claimTransactionRefInsertQuery returning no row) that this call
// lost the claim to someone else.
//
// This deliberately a second, separate statement rather than one combined
// with the INSERT above in a single WITH — the two cannot be merged
// safely. Postgres (read committed, the default) takes each statement's
// MVCC snapshot at that statement's own start, and when
// claimTransactionRefInsertQuery's INSERT blocks on another transaction's
// uncommitted claim of the very same transaction_ref, it is only that
// blocked INSERT's own conflict check that gets re-evaluated against fresh
// data once the blocker commits — a sibling SELECT sharing the same
// top-level statement (e.g. in a WITH, or a UNION ALL) would still run
// against the *original* snapshot taken before the blocker committed, and
// so can miss the very row the INSERT just discovered. Issuing this SELECT
// as its own, later statement gives it a fresh snapshot — taken only after
// the INSERT above has already unblocked, by which point the blocking
// transaction's commit (or rollback) has necessarily already happened —
// so it reliably sees what the INSERT just lost to.
const claimTransactionRefSelectQuery = `SELECT fingerprint FROM journal_entry_postings WHERE transaction_ref = $1`

// claimTransactionRef is PostJournalEntry's (and PostJournalEntryTx's)
// idempotency guard. It reports:
//
//   - (false, nil) if transactionRef was fresh and is now claimed for
//     fingerprint — the caller should proceed to insert its entries.
//   - (true, nil) if transactionRef was already claimed for this exact
//     fingerprint — an identical retry; the caller must not insert its
//     entries again (claimTransactionRef made no write this time, so there
//     is nothing to roll back or commit on account of this call alone).
//   - (false, non-nil) if transactionRef was already claimed for a
//     different fingerprint (wrapping ErrTransactionRefConflict), or if
//     either query failed.
//
// Postgres's unique index on transaction_ref is what makes two goroutines
// calling PostJournalEntry for the same transactionRef at the same time
// resolve to exactly one claim rather than two: whichever INSERT loses the
// race blocks on the winner's row until it commits or rolls back, then
// falls back to claimTransactionRefSelectQuery exactly as a call that
// arrived after the winner had already committed would.
func claimTransactionRef(ctx context.Context, tx DB, transactionRef, fingerprint string) (alreadyPosted bool, err error) {
	var claimedFingerprint string
	insertErr := tx.QueryRowContext(ctx, claimTransactionRefInsertQuery, transactionRef, fingerprint).Scan(&claimedFingerprint)
	if insertErr == nil {
		return false, nil // freshly claimed for fingerprint
	}
	if !errors.Is(insertErr, sql.ErrNoRows) {
		return false, fmt.Errorf("ledger: claim transaction_ref %q: %w", transactionRef, insertErr)
	}

	var existingFingerprint string
	if err := tx.QueryRowContext(ctx, claimTransactionRefSelectQuery, transactionRef).Scan(&existingFingerprint); err != nil {
		return false, fmt.Errorf("ledger: read back existing claim for transaction_ref %q: %w", transactionRef, err)
	}
	if existingFingerprint != fingerprint {
		return false, fmt.Errorf("ledger: transaction_ref %q was already posted with a different entry set: %w", transactionRef, ErrTransactionRefConflict)
	}
	return true, nil
}

// fingerprintEntries returns a deterministic digest of entries' economic
// content — each leg's account, amount, and currency — used to tell an
// identical retry of one transaction_ref apart from a conflicting one (see
// claimTransactionRef). Legs are sorted before hashing so two calls posting
// the same set of legs in a different slice order still fingerprint the
// same. It deliberately excludes TransactionRef (every entry passed here
// was just stamped with the one being claimed, by prepareEntries) and
// ID/CreatedAt (never set by a caller building entries to post).
func fingerprintEntries(entries []Entry) string {
	legs := make([]string, len(entries))
	for i, e := range entries {
		legs[i] = fmt.Sprintf("%d:%d:%s", e.AccountID, e.Amount, e.Currency)
	}
	sort.Strings(legs)

	sum := sha256.Sum256([]byte(strings.Join(legs, "|")))
	return hex.EncodeToString(sum[:])
}

// PostJournalEntry is the single write path for moving money in PayGate:
// every other package must call this — never INSERT INTO journal_entries
// directly (see doc.go, the "code review checklist" entry in
// CONTRIBUTING.md, and the `make lint-ledger-writes` grep in the Makefile,
// which together enforce that) — to post one balanced double-entry
// transaction.
//
// It rejects the call, with no partial write, unless entries forms a valid
// double-entry transaction per ValidateBalancedTransaction (in particular:
// sum(entries.amount) == 0 for every currency) — that check runs entirely in
// Go, before any statement is sent to the database, so an unbalanced call
// never touches journal_entries at all. The error it returns for that case
// wraps ErrUnbalancedTransaction, distinct from any error the database
// itself can return.
//
// Entries need not set Entry.TransactionRef themselves: PostJournalEntry
// stamps every row with transactionRef before validating or writing, so a
// stale or mismatched field on a caller-built Entry can never split one
// logical transaction across two transaction_ref values.
//
// Once validated, every entry is written inside a single database
// transaction and committed together. Committing is what actually lets the
// journal_entries_balanced_transaction deferred constraint trigger (see
// backend/migrations/*_create_journal_entries) verify the whole set — it
// fires per row but isn't evaluated until COMMIT, by which point every row
// this call means to insert together is visible to it. That trigger is the
// database's own backstop for the same invariant this function already
// checks in Go; PostJournalEntry does not depend on it to behave correctly,
// but relies on it being there so no other, buggy or malicious, write path
// could leave the ledger unbalanced even if it bypassed this function.
//
// PostJournalEntry always begins and commits its own transaction, so it
// can't be used when the entries must land atomically alongside some other
// write (e.g. a Payment row's status moving to captured) — see
// PostJournalEntryTx for that case.
//
// PostJournalEntry is safe to retry: before writing anything, it claims
// transactionRef in journal_entry_postings (via claimTransactionRef) for a
// fingerprint of entries' economic content. A retry with the identical
// entry set finds transactionRef already claimed for that same fingerprint
// and returns nil without inserting any rows a second time. A retry with a
// *different* entry set for the same transactionRef is rejected, wrapping
// ErrTransactionRefConflict, and also writes nothing. Both the claim and
// the entries insert happen inside the one transaction this call begins,
// so a failure after a fresh claim (e.g. the entries insert itself failing)
// rolls the claim back too — transactionRef is left unclaimed, free for a
// genuine retry to succeed.
func PostJournalEntry(ctx context.Context, db Beginner, transactionRef string, entries []Entry) error {
	stamped, err := prepareEntries(transactionRef, entries)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin transaction for %q: %w", transactionRef, err)
	}

	alreadyPosted, err := claimTransactionRef(ctx, tx, transactionRef, fingerprintEntries(stamped))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if alreadyPosted {
		// claimTransactionRef made no write for an identical retry — there
		// is nothing of this call's own to commit, so roll back the
		// otherwise-empty transaction rather than leave it open.
		_ = tx.Rollback()
		return nil
	}

	if err := insertJournalEntries(ctx, tx, stamped); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit transaction %q: %w", transactionRef, err)
	}

	return nil
}

// PostJournalEntryTx is PostJournalEntry's composable counterpart: it
// performs the exact same stamping, validation, and inserts, but against a
// Tx the caller already began — and, critically, it never calls Commit or
// Rollback itself. That makes it the hook a future caller needs to satisfy
// "ledger entries post in the same database transaction as some other
// write": e.g. the Payments package's capture handler (not yet implemented
// — see docs/ARCHITECTURE.md's Payment state machine — but which will need
// a Payment row's status update to `captured` and this transaction's
// journal entries to commit or roll back together) begins its own Tx,
// updates the Payment row, calls this with that same Tx, and only then
// commits once.
//
// Callers must roll back tx themselves if this returns an error — exactly
// as they would for any other failed write inside their own transaction.
//
// Like PostJournalEntry, this is safe to retry: it claims transactionRef
// (via claimTransactionRef) against tx before inserting anything, and
// returns nil without writing entries again if it was already claimed for
// an identical entry set, or an error wrapping ErrTransactionRefConflict
// (and still writes nothing) if it was claimed for a different one.
func PostJournalEntryTx(ctx context.Context, tx Tx, transactionRef string, entries []Entry) error {
	stamped, err := prepareEntries(transactionRef, entries)
	if err != nil {
		return err
	}

	alreadyPosted, err := claimTransactionRef(ctx, tx, transactionRef, fingerprintEntries(stamped))
	if err != nil {
		return err
	}
	if alreadyPosted {
		return nil
	}

	return insertJournalEntries(ctx, tx, stamped)
}

// prepareEntries stamps every entry with transactionRef and validates the
// resulting set forms a balanced double-entry transaction, shared by
// PostJournalEntry and PostJournalEntryTx so the two can never drift apart
// on what "a valid call" means.
func prepareEntries(transactionRef string, entries []Entry) ([]Entry, error) {
	if transactionRef == "" {
		return nil, fmt.Errorf("ledger: transaction_ref must not be empty")
	}

	stamped := make([]Entry, len(entries))
	for i, e := range entries {
		e.TransactionRef = transactionRef
		stamped[i] = e
	}

	if err := ValidateBalancedTransaction(stamped); err != nil {
		return nil, err
	}

	return stamped, nil
}

// insertJournalEntries writes each of entries (already stamped and
// validated by prepareEntries) via tx, in order, stopping at the first
// failure. It does not commit or roll back tx — both PostJournalEntry and
// PostJournalEntryTx own that decision themselves, since only they know
// whether tx is theirs alone or shared with other writes.
func insertJournalEntries(ctx context.Context, tx Tx, entries []Entry) error {
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, insertJournalEntryQuery, e.TransactionRef, e.AccountID, e.Amount, e.Currency); err != nil {
			return fmt.Errorf("ledger: insert journal entry for %q: %w", e.TransactionRef, err)
		}
	}
	return nil
}
