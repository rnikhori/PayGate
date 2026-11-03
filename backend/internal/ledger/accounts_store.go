package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// Row is the part of *sql.Row this package needs, so callers can satisfy DB
// with lightweight fakes in tests instead of a live database.
type Row interface {
	Scan(dest ...any) error
}

// DB is the subset of *sql.DB (or *sql.Tx) the ledger package needs to read
// and write accounts. Use Wrap to get one from a *sql.DB.
type DB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) Row
}

// Tx is the subset of *sql.Tx that PostJournalEntry (see post.go) needs to
// write rows and then either commit or roll back, so callers can satisfy it
// with a lightweight fake in tests instead of a live database.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

// Beginner starts a database transaction. *sql.DB satisfies it via Wrap.
type Beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error)
}

// Conn is everything this package needs from a database handle: reading a
// single row (DB) and starting the transaction PostJournalEntry writes every
// entry of one logical transaction inside (Beginner). Wrap adapts a *sql.DB
// to it; a Conn can be passed anywhere a DB or a Beginner is expected.
type Conn interface {
	DB
	Beginner
}

// sqlDB adapts *sql.DB to Conn: *sql.DB's QueryRowContext and BeginTx return
// the concrete *sql.Row / *sql.Tx, which already implement Row / Tx, but
// Go's interfaces require an exact method signature match, so this thin
// wrapper is what actually makes *sql.DB satisfy Conn.
type sqlDB struct{ *sql.DB }

func (s sqlDB) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	return s.DB.QueryRowContext(ctx, query, args...)
}

func (s sqlDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	tx, err := s.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// Wrap adapts a *sql.DB for use as a Conn (satisfying both DB and Beginner).
func Wrap(db *sql.DB) Conn {
	return sqlDB{db}
}

// EnsureMerchantPayableAccount creates the merchant_payable:<merchantID>
// account if it does not already exist, and returns it either way.
//
// This is the only way a per-merchant payable account comes into being —
// called once a Merchant transitions to active. That caller lives in the
// merchants package (a later issue, once the Merchant schema exists), never
// here: internal/ledger must not depend on any other internal/* package
// (see doc.go), so this takes a bare merchantID rather than a Merchant.
//
// The insert is idempotent: ON CONFLICT (code) DO UPDATE SET code = code is
// a no-op write that still lets RETURNING hand back the existing row, so
// calling this more than once for the same merchant is safe.
func EnsureMerchantPayableAccount(ctx context.Context, db DB, merchantID string) (Account, error) {
	code, err := MerchantPayableAccountCode(merchantID)
	if err != nil {
		return Account{}, err
	}

	const query = `
		INSERT INTO ledger_accounts (code, type, name)
		VALUES ($1, $2, $3)
		ON CONFLICT (code) DO UPDATE SET code = ledger_accounts.code
		RETURNING id, code, type, name, created_at`

	name := fmt.Sprintf("Merchant payable: %s", merchantID)

	var (
		a       Account
		accType string
	)
	row := db.QueryRowContext(ctx, query, code, string(MerchantPayableAccountType), name)
	if err := row.Scan(&a.ID, &a.Code, &accType, &a.Name, &a.CreatedAt); err != nil {
		return Account{}, fmt.Errorf("ledger: ensure merchant payable account for %q: %w", merchantID, err)
	}
	a.Type = AccountType(accType)
	return a, nil
}
