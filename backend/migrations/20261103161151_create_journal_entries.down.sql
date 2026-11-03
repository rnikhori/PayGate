DROP TRIGGER IF EXISTS journal_entries_append_only ON journal_entries;
DROP TRIGGER IF EXISTS journal_entries_balanced_transaction ON journal_entries;
DROP TABLE IF EXISTS journal_entries;
DROP FUNCTION IF EXISTS journal_entries_reject_mutation();
DROP FUNCTION IF EXISTS journal_entries_check_balanced_transaction();
DROP ROLE IF EXISTS paygate_app;
