-- Create the journal_entries table: the immutable, append-only double-entry
-- core described in docs/ARCHITECTURE.md's "Ledger model" section. Every row
-- is one leg of a money movement — it references a ledger_accounts row (see
-- backend/migrations/*_create_ledger_accounts) and a signed amount — and
-- rows sharing a transaction_ref together must sum to zero, the same way a
-- real double-entry book balances every transaction.
--
-- No balance is ever stored: a merchant's (or any account's) balance is
-- always SUM(amount) over its journal_entries rows, computed on read. That
-- is why this table must never be updated or deleted from — any correction
-- is itself a new, balancing journal entry (e.g. a refund), never an edit to
-- history.

CREATE TABLE journal_entries (
    id              BIGSERIAL PRIMARY KEY,

    -- Groups the ≥2 rows that make up one balanced transaction (e.g. a
    -- payment capture: a credit to customer_suspense and a debit to
    -- merchant_payable:<id>). Callers are expected to insert every row for
    -- a given transaction_ref inside a single database transaction and
    -- commit once — see the balance check below, which is evaluated at
    -- commit and re-reads the whole table, so legs inserted in separate,
    -- independently-committed transactions would each need to already
    -- balance on their own.
    transaction_ref TEXT         NOT NULL,

    account_id      BIGINT       NOT NULL REFERENCES ledger_accounts (id),

    -- Signed amount in the smallest unit of currency (e.g. paise for INR),
    -- matching Razorpay's own integer-amount convention. Positive credits
    -- the account, negative debits it. Never zero: a leg that moves no
    -- money isn't a real leg of a transaction.
    amount          BIGINT       NOT NULL CHECK (amount <> 0),

    -- ISO 4217 currency code. docs/ARCHITECTURE.md's non-goals scope this
    -- project to INR only, but the column stores the real code rather than
    -- hard-coding 'INR' here, so that scope decision lives in one place
    -- (the docs) instead of being duplicated into a DB constraint.
    currency        VARCHAR(3)   NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Supports both "all legs of this transaction" lookups (used by the balance
-- check below, and by anyone reconstructing a transaction) and "every entry
-- against this account" lookups (used to compute an account's balance).
CREATE INDEX journal_entries_transaction_ref_idx ON journal_entries (transaction_ref);
CREATE INDEX journal_entries_account_id_idx ON journal_entries (account_id);

-- Enforces "a transaction_ref always has at least 2 rows, and they sum to
-- zero per currency". This is a set-based rule (it depends on every row
-- sharing a transaction_ref, not just the row being inserted), so a plain
-- CHECK constraint can't express it — a deferred, per-row constraint
-- trigger can: it fires once per inserted row, but not until COMMIT (or an
-- explicit SET CONSTRAINTS ... IMMEDIATE), by which point every row a
-- single transaction meant to insert together is visible to it.
CREATE FUNCTION journal_entries_check_balanced_transaction() RETURNS TRIGGER AS $$
DECLARE
    row_count  INTEGER;
    unbalanced RECORD;
BEGIN
    SELECT COUNT(*) INTO row_count
    FROM journal_entries
    WHERE transaction_ref = NEW.transaction_ref;

    IF row_count < 2 THEN
        RAISE EXCEPTION 'journal_entries: transaction_ref % has % row(s), need at least 2 for double-entry', NEW.transaction_ref, row_count;
    END IF;

    SELECT currency, SUM(amount) AS total INTO unbalanced
    FROM journal_entries
    WHERE transaction_ref = NEW.transaction_ref
    GROUP BY currency
    HAVING SUM(amount) <> 0
    LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION 'journal_entries: transaction_ref % does not sum to zero for currency % (sum = %)', NEW.transaction_ref, unbalanced.currency, unbalanced.total;
    END IF;

    RETURN NULL; -- ignored for an AFTER trigger
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER journal_entries_balanced_transaction
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION journal_entries_check_balanced_transaction();

-- Enforces "append-only" at the table itself: UPDATE and DELETE are
-- rejected unconditionally, for every role, including the table owner.
-- This is what actually makes the table immutable in the current single
-- database-role setup (infra/docker-compose.yml's one POSTGRES_USER owns
-- every table, and Postgres lets an owner bypass its own GRANT/REVOKE —
-- "no UPDATE/DELETE grants" alone would not stop it from updating its own
-- table). The REVOKE/GRANT block further below is the other half: it makes
-- sure no role is ever granted UPDATE/DELETE either, so a future
-- least-privilege runtime role (paygate_app, also created below) is
-- correct by construction, not just blocked by this trigger.
CREATE FUNCTION journal_entries_reject_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'journal_entries is append-only: % is not permitted (row id %)', TG_OP, COALESCE(OLD.id, NEW.id);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER journal_entries_append_only
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW
    EXECUTE FUNCTION journal_entries_reject_mutation();

-- Least-privilege role for the API's runtime database connection, as
-- distinct from the role migrations run as (which must own the table, to
-- be able to create/alter it at all). No migration ever grants this role
-- UPDATE or DELETE on journal_entries — the absence of that grant is the
-- literal "no UPDATE/DELETE grants at the DB role level". Wiring the API's
-- DATABASE_URL to actually connect as paygate_app (today it connects as
-- the same owning role the migrator uses, per backend/internal/config) is
-- a deployment concern for a later issue; creating and correctly scoping
-- the role now means that switch is the only thing left to do.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'paygate_app') THEN
        CREATE ROLE paygate_app NOLOGIN;
    END IF;
END
$$;

REVOKE ALL ON journal_entries FROM PUBLIC;
REVOKE ALL ON journal_entries FROM paygate_app;
GRANT SELECT, INSERT ON journal_entries TO paygate_app;
GRANT USAGE, SELECT ON SEQUENCE journal_entries_id_seq TO paygate_app;
