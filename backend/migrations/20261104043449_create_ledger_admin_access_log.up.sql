-- Create ledger_admin_access_log: the audit trail for
-- backend/internal/ledger/admin_inspect.go's InspectTransactions, the
-- admin-only raw journal entry inspection API described in this issue.
--
-- "Access to this endpoint is itself audit-logged (who looked up what,
-- when)" is this issue's own acceptance criterion applied reflexively: the
-- very act of an admin reading raw ledger data for a support ticket must
-- itself leave a durable record, independent of whatever application logs
-- the process emits (which rotate, and aren't queryable). One row is
-- written per InspectTransactions call -- both when access is granted and
-- when it is refused for lacking the admin role, per "who looked up what,
-- when" covering attempted lookups, not only successful ones.

CREATE TABLE ledger_admin_access_log (
    id               BIGSERIAL PRIMARY KEY,

    -- Who: the caller's identity and the role they presented, as handed to
    -- InspectTransactions by whatever authenticated the request (the Admin
    -- Ops Panel's auth layer, from the still-unbuilt "Auth & API Key
    -- Management" epic). actor_role is stored as presented, not assumed
    -- 'admin' -- a denied row (actor_role <> 'admin') is exactly what
    -- proves the role gate actually rejected a non-admin caller.
    actor_id         TEXT        NOT NULL,
    actor_role       TEXT        NOT NULL,

    -- What: the transaction_ref(s) the caller asked to inspect, exactly as
    -- requested (deduplicated, see InspectTransactions). JSONB rather than
    -- a Postgres TEXT[] so the application layer can write it with a plain
    -- json.Marshal'd string, the same way
    -- backend/migrations/*_create_ledger_integrity_checks' `details`
    -- column is written, without taking on a third-party array-binding
    -- dependency.
    transaction_refs JSONB       NOT NULL,

    -- Whether this lookup was actually allowed to proceed (actor_role was
    -- 'admin') or rejected by the role gate. A denied row never has a
    -- corresponding read of journal_entries -- the gate rejects before any
    -- query runs -- but the attempt itself is still "who looked up what,
    -- when" worth keeping.
    allowed          BOOLEAN     NOT NULL,

    -- When.
    accessed_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports "most recent accesses first" / "every lookup by this actor"
-- audit queries without a seq scan -- the only read pattern this table is
-- expected to serve.
CREATE INDEX ledger_admin_access_log_accessed_at_idx ON ledger_admin_access_log (accessed_at DESC);
CREATE INDEX ledger_admin_access_log_actor_id_idx ON ledger_admin_access_log (actor_id);

-- Least-privilege grant, mirroring journal_entries / ledger_integrity_checks
-- (see backend/migrations/*_create_journal_entries and
-- *_create_ledger_integrity_checks): the API only ever inserts one row per
-- access and reads prior rows back (e.g. for an ops audit review) -- never
-- UPDATE or DELETE, so a recorded access can never be altered or erased at
-- the DB role level either, the same append-only guarantee an audit trail
-- needs to actually be trustworthy.
REVOKE ALL ON ledger_admin_access_log FROM PUBLIC;
REVOKE ALL ON ledger_admin_access_log FROM paygate_app;
GRANT SELECT, INSERT ON ledger_admin_access_log TO paygate_app;
GRANT USAGE, SELECT ON SEQUENCE ledger_admin_access_log_id_seq TO paygate_app;
