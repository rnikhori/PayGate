-- Create the ledger_accounts table: the fixed set of named accounts that
-- JournalEntry rows (added in a later issue) will post against. See
-- docs/ARCHITECTURE.md's "Ledger model" section and
-- backend/internal/ledger/account.go for the account-code naming
-- convention this table's CHECK constraint mirrors.

CREATE TABLE ledger_accounts (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(128) NOT NULL,
    type        VARCHAR(16)  NOT NULL,
    name        TEXT         NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT ledger_accounts_code_unique UNIQUE (code),
    CONSTRAINT ledger_accounts_type_valid CHECK (type IN ('asset', 'liability', 'revenue')),

    -- Naming convention, kept in sync with the accountCodePattern regexp in
    -- backend/internal/ledger/account.go: lowercase snake_case, optionally
    -- namespaced with a single ":<id>" suffix for per-entity accounts such
    -- as merchant_payable:<merchant_id>. Enforced here too (not only in Go)
    -- so no code path, including a raw SQL insert, can create an account
    -- that violates it.
    CONSTRAINT ledger_accounts_code_format CHECK (
        code ~ '^[a-z][a-z0-9_]*(:[a-z0-9][a-z0-9_-]*)?$'
    )
);

-- code is immutable once set. JournalEntry rows (added in a later issue)
-- reference accounts by id, but everything else in the system — seed data,
-- application code, humans reading a statement — refers to them by their
-- code, so letting it drift after creation would silently break those
-- references. Nothing else about a row is restricted here; e.g. `name` may
-- still be edited.
CREATE FUNCTION ledger_accounts_code_is_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.code <> OLD.code THEN
        RAISE EXCEPTION 'ledger_accounts.code is immutable (was %, attempted %)', OLD.code, NEW.code;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_accounts_code_immutable
    BEFORE UPDATE ON ledger_accounts
    FOR EACH ROW
    EXECUTE FUNCTION ledger_accounts_code_is_immutable();

-- Seed the fixed system accounts referenced throughout the design (see
-- docs/ARCHITECTURE.md's "Ledger model" section). Per-merchant
-- merchant_payable:<merchant_id> accounts are deliberately NOT seeded here:
-- they don't exist until a merchant does, and are created on demand by
-- ledger.EnsureMerchantPayableAccount when a Merchant is activated (a later
-- issue, once the Merchant schema exists, wires that call in) rather than
-- ahead of time for merchants that may never sign up.
INSERT INTO ledger_accounts (code, type, name) VALUES
    ('customer_suspense',   'liability', 'Customer funds held pending settlement'),
    ('gateway_fee_revenue', 'revenue',   'PayGate processing fee revenue'),
    ('settlement_clearing', 'asset',     'In-transit funds clearing to merchant bank accounts');
