-- Create ledger_integrity_checks: the audit/history record for the nightly
-- (or on-demand) ledger integrity job described in
-- backend/internal/ledger/integrity.go. Every run of that job -- whether it
-- found the ledger balanced or not -- inserts exactly one row here, so
-- "did the ledger check out last night" and "when did it last fail" are
-- answerable from the database, not just from log lines that may have
-- rotated away.
--
-- This table is itself never written to by the job's integrity *check*
-- (ledger.CheckIntegrity only ever issues SELECTs against journal_entries
-- and ledger_accounts) -- only by recording the check's own result
-- afterwards (ledger.RecordIntegrityCheck). The job never writes to
-- journal_entries or ledger_accounts at all: per this issue's acceptance
-- criteria ("Job is read-only"), it only ever reports on the ledger, never
-- repairs it.

CREATE TABLE ledger_integrity_checks (
    id          BIGSERIAL PRIMARY KEY,

    checked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- How this run was invoked, matching the ledger.IntegrityCheckTrigger
    -- values in Go -- "on a schedule (configurable, default nightly) and on
    -- demand" from this issue's acceptance criteria.
    trigger     VARCHAR(16) NOT NULL CHECK (trigger IN ('scheduled', 'manual')),

    -- Whether this run found the ledger balanced: global_sum = 0 and no
    -- unbalanced_transactions entry in details. Stored redundantly
    -- alongside global_sum/details (rather than requiring a reader to
    -- recompute it) so "find the last failing run" is a plain WHERE NOT
    -- passed, no JSON inspection needed.
    passed      BOOLEAN NOT NULL,

    -- SUM(journal_entries.amount) across the entire ledger at the moment
    -- this check ran. Always 0 for a healthy ledger -- see
    -- ledger.CheckIntegrity's doc comment for why a nonzero value here
    -- would mean the double-entry invariant itself (normally enforced by
    -- the journal_entries_balanced_transaction trigger) has somehow been
    -- violated, not just that some account's own balance looks unusual.
    global_sum  BIGINT NOT NULL,

    -- How many distinct (transaction_ref, currency) groups, taken alone,
    -- failed to sum to zero -- this run's spot check, finer-grained than
    -- global_sum alone (see ledger.CheckIntegrity). Zero for a healthy
    -- ledger.
    unbalanced_count INTEGER NOT NULL DEFAULT 0,

    -- Full detail for audit: which transaction_ref(s) were found
    -- unbalanced, their currency, their sum, and which ledger_accounts.code
    -- values were party to each one. Shaped by ledger.IntegrityReport's
    -- JSON encoding (see MarshalDetails in integrity.go). Empty object for
    -- a healthy, fully-passed run -- there is nothing to detail.
    details     JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- Supports "most recent runs first" / "most recent failure" queries without
-- a seq scan, the only read pattern this table is expected to serve.
CREATE INDEX ledger_integrity_checks_checked_at_idx ON ledger_integrity_checks (checked_at DESC);

-- Least-privilege grant, mirroring journal_entries / journal_entry_postings
-- (see backend/migrations/*_create_journal_entries): the job only ever
-- inserts one row per run and reads prior rows back (e.g. to show history)
-- -- never UPDATE or DELETE, so a recorded run can never be altered or
-- erased at the DB role level either.
REVOKE ALL ON ledger_integrity_checks FROM PUBLIC;
REVOKE ALL ON ledger_integrity_checks FROM paygate_app;
GRANT SELECT, INSERT ON ledger_integrity_checks TO paygate_app;
GRANT USAGE, SELECT ON SEQUENCE ledger_integrity_checks_id_seq TO paygate_app;
