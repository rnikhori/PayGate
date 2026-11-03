// Package ledger implements PayGate's double-entry accounting core: immutable
// JournalEntry rows against LedgerAccounts, with balances always computed as
// SUM(journal_entries) rather than stored counters. It is the single source of
// truth for money movement and must not import any other internal/* package.
package ledger
