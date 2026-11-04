-- This issue's acceptance criteria call for an index on
-- journal_entries(account_id, created_at) to support the statement-API
-- query pattern (backend/internal/ledger/statement.go's statementPageQuery,
-- which filters WHERE account_id = $1 and then orders by created_at, id).
-- journal_entries_account_id_idx (from
-- backend/migrations/*_create_journal_entries) only covers account_id, so
-- that query still has to sort the matching rows after the index scan.
--
-- A composite (account_id, created_at) index serves that lookup-then-order
-- pattern directly, and its leading column alone still satisfies every
-- account_id-only lookup the single-column index served (GetBalance in
-- backend/internal/ledger/balance.go, and the DB-level balance guard) —
-- the same way a phone book sorted by (last_name, first_name) still answers
-- "find everyone named Smith" without needing a last_name-only index too.
-- Keeping both would just be a second index to maintain on every insert
-- for no query this schema has, so the old one is dropped here rather than
-- left alongside the new one.
DROP INDEX IF EXISTS journal_entries_account_id_idx;

CREATE INDEX journal_entries_account_id_created_at_idx
    ON journal_entries (account_id, created_at);
