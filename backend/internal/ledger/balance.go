package ledger

import (
	"context"
	"fmt"
)

// GetBalance returns accountID's current balance: SUM(journal_entries.amount)
// over every row ever posted against it, recomputed on every call rather
// than read from any stored counter — see doc.go and docs/ARCHITECTURE.md's
// "Ledger model" section ("A merchant's available balance is a computed
// view ..., never a stored counter"). ledger_accounts
// (backend/migrations/*_create_ledger_accounts) deliberately has no
// `balance` column: a mutable counter could drift from what
// journal_entries actually records, which is exactly what double-entry
// accounting is meant to make impossible.
//
// The query filters on account_id alone (never transaction_ref, and never
// per-currency — docs/ARCHITECTURE.md's non-goals scope PayGate to INR
// only, so an account's postings are expected to share one currency), which
// the composite index backend/migrations/*_add_journal_entries_account_created_at_index
// creates (account_id, created_at) already covers via its leading column.
// This function relies on that existing index rather than adding one of its
// own.
//
// A materialized or cached balance is explicitly out of scope here: if a
// future issue adds one (e.g. to avoid re-scanning a very large account on
// every dashboard load), it must land as its own tracked, deliberate
// change — never folded silently into this function.
//
// An account with no journal_entries rows yet (or an accountID that does
// not exist) has a balance of 0, not an error: SUM() over zero matching
// rows is SQL NULL, which the query's COALESCE turns into 0 before this
// function ever sees it.
func GetBalance(ctx context.Context, db DB, accountID int64) (int64, error) {
	if accountID == 0 {
		return 0, fmt.Errorf("ledger: account id must not be zero")
	}

	const query = `SELECT COALESCE(SUM(amount), 0) FROM journal_entries WHERE account_id = $1`

	var balance int64
	row := db.QueryRowContext(ctx, query, accountID)
	if err := row.Scan(&balance); err != nil {
		return 0, fmt.Errorf("ledger: get balance for account %d: %w", accountID, err)
	}
	return balance, nil
}
