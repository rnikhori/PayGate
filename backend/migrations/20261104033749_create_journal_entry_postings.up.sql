-- Create journal_entry_postings: the idempotency guard for
-- ledger.PostJournalEntry / PostJournalEntryTx (backend/internal/ledger/post.go).
--
-- journal_entries.transaction_ref cannot itself carry a uniqueness
-- constraint: every balanced transaction legitimately posts >= 2 rows
-- sharing one transaction_ref (see
-- backend/migrations/*_create_journal_entries's
-- journal_entries_balanced_transaction trigger). Without some other guard,
-- retrying a PostJournalEntry call for a transaction_ref that already
-- posted would insert a second, duplicate set of rows -- that trigger only
-- checks that each commit's rows sum to zero, and a duplicated balanced
-- set still sums to zero.
--
-- This table is that guard: exactly one row per transaction_ref, enforced
-- by its PRIMARY KEY, claimed via INSERT ... ON CONFLICT DO NOTHING inside
-- the same database transaction that inserts a call's journal_entries rows
-- (see post.go's claimTransactionRef). Postgres's unique index serializes
-- concurrent claims of the same transaction_ref -- a second concurrent
-- INSERT blocks on the first's uncommitted row until it commits or rolls
-- back, then re-reads and either sees the committed claim (and compares
-- fingerprints) or proceeds to claim it itself -- which is what makes two
-- goroutines posting the same transaction_ref simultaneously resolve to
-- exactly one set of journal_entries rows rather than a race.
--
-- fingerprint is a deterministic digest of the entry set (see
-- fingerprintEntries in post.go), not the raw entries -- it is what lets a
-- retried call with an identical entry set be told apart from one with a
-- different entry set for the same transaction_ref, without this table
-- duplicating journal_entries' own data.
CREATE TABLE journal_entry_postings (
    transaction_ref TEXT        PRIMARY KEY,
    fingerprint     TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Least-privilege grant, mirroring journal_entries' own (see
-- backend/migrations/*_create_journal_entries): paygate_app, created by
-- that earlier migration, only ever needs to claim a transaction_ref
-- (INSERT) and read back what was claimed (SELECT) -- never UPDATE or
-- DELETE, so a claimed transaction_ref can never be un-claimed or altered
-- at the DB role level either.
REVOKE ALL ON journal_entry_postings FROM PUBLIC;
REVOKE ALL ON journal_entry_postings FROM paygate_app;
GRANT SELECT, INSERT ON journal_entry_postings TO paygate_app;
