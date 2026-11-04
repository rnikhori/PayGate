DROP INDEX IF EXISTS journal_entries_account_id_created_at_idx;

CREATE INDEX journal_entries_account_id_idx ON journal_entries (account_id);
