package ledger

import (
	"fmt"
	"regexp"
	"time"
)

// Entry mirrors one row of the journal_entries table (see
// backend/migrations/*_create_journal_entries): one leg of a double-entry
// transaction, posted against a LedgerAccount for a signed amount.
//
// Entry rows are never updated or deleted once inserted — the
// journal_entries_append_only trigger in that migration enforces this at
// the database level regardless of which code path or DB role writes to
// the table. A correction (e.g. a refund) is always a new, separately
// balanced transaction, never an edit to an existing Entry.
type Entry struct {
	ID int64

	// TransactionRef groups the ≥2 Entry rows that together make up one
	// balanced transaction (e.g. a payment capture: a credit to
	// customer_suspense and a debit to merchant_payable:<id>).
	TransactionRef string

	AccountID int64

	// Amount is signed, in the smallest unit of currency (e.g. paise for
	// INR), matching journal_entries.amount. Positive credits the
	// account, negative debits it. Must never be zero: a leg that moves
	// no money isn't a real leg of a transaction.
	Amount int64

	// Currency is an ISO 4217 code, matching journal_entries.currency.
	Currency string

	CreatedAt time.Time
}

// currencyCodePattern mirrors the journal_entries.currency CHECK
// constraint in the migration that creates the table.
var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidateCurrencyCode reports an error if currency does not conform to
// the ISO 4217 format the journal_entries table requires.
func ValidateCurrencyCode(currency string) error {
	if !currencyCodePattern.MatchString(currency) {
		return fmt.Errorf("ledger: currency code %q does not match %s", currency, currencyCodePattern.String())
	}
	return nil
}

// ValidateBalancedTransaction reports an error unless entries forms a
// valid double-entry transaction: at least 2 rows, all sharing one
// transaction_ref, each with a valid account id, currency, and non-zero
// amount, whose amounts sum to zero per currency.
//
// This mirrors, in Go, the journal_entries_check_balanced_transaction
// deferred constraint trigger from the migration that creates the table —
// it lets a caller building a transaction (e.g. a later issue's
// ledger.Post) fail fast before issuing any SQL. It is a convenience, not
// a substitute for that trigger: the trigger is what actually guarantees
// the invariant in the database, regardless of which code path performs
// the insert.
func ValidateBalancedTransaction(entries []Entry) error {
	if len(entries) < 2 {
		return fmt.Errorf("ledger: transaction has %d entry(ies), need at least 2 for double-entry", len(entries))
	}

	ref := entries[0].TransactionRef
	if ref == "" {
		return fmt.Errorf("ledger: entry transaction_ref must not be empty")
	}

	sums := make(map[string]int64, 1)
	for i, e := range entries {
		if e.TransactionRef != ref {
			return fmt.Errorf("ledger: entry %d has transaction_ref %q, want %q (every entry in one transaction must share it)", i, e.TransactionRef, ref)
		}
		if e.AccountID == 0 {
			return fmt.Errorf("ledger: entry %d has no account id", i)
		}
		if e.Amount == 0 {
			return fmt.Errorf("ledger: entry %d has a zero amount", i)
		}
		if err := ValidateCurrencyCode(e.Currency); err != nil {
			return fmt.Errorf("ledger: entry %d: %w", i, err)
		}
		sums[e.Currency] += e.Amount
	}

	for currency, sum := range sums {
		if sum != 0 {
			return fmt.Errorf("ledger: transaction_ref %q does not sum to zero for currency %s (sum = %d)", ref, currency, sum)
		}
	}

	return nil
}
