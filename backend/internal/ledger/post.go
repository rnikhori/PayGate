package ledger

import (
	"context"
	"fmt"
)

// insertJournalEntryQuery mirrors the journal_entries columns this package
// (and only this package — see doc.go and PostJournalEntry below) is
// permitted to write: backend/migrations/*_create_journal_entries grants
// paygate_app nothing beyond SELECT/INSERT on the table, and never grants
// UPDATE/DELETE to any role.
const insertJournalEntryQuery = `
	INSERT INTO journal_entries (transaction_ref, account_id, amount, currency)
	VALUES ($1, $2, $3, $4)`

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
func PostJournalEntry(ctx context.Context, db Beginner, transactionRef string, entries []Entry) error {
	if transactionRef == "" {
		return fmt.Errorf("ledger: transaction_ref must not be empty")
	}

	stamped := make([]Entry, len(entries))
	for i, e := range entries {
		e.TransactionRef = transactionRef
		stamped[i] = e
	}

	if err := ValidateBalancedTransaction(stamped); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin transaction for %q: %w", transactionRef, err)
	}

	for _, e := range stamped {
		if _, err := tx.ExecContext(ctx, insertJournalEntryQuery, e.TransactionRef, e.AccountID, e.Amount, e.Currency); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("ledger: insert journal entry for %q: %w", transactionRef, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit transaction %q: %w", transactionRef, err)
	}

	return nil
}
