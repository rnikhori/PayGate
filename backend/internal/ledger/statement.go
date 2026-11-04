package ledger

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rows is the part of *sql.Rows this package needs for a multi-row query,
// mirroring Row (see accounts_store.go): it lets tests satisfy StatementDB
// with a lightweight fake instead of a live database. *sql.Rows already
// implements it (Next/Scan/Err/Close all match exactly), so no adapter type
// is needed on the production side beyond the StatementDB wrapper below.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// StatementDB is what GetAccountStatement needs from a database handle:
// QueryRowContext (DB, to resolve the merchant's own account via
// GetAccountByCode) plus QueryContext, to page through that account's
// journal_entries. Conn (accounts_store.go) deliberately does not embed
// this — none of the write paths that accept a Conn ever need a multi-row
// query — so this is its own, narrower capability, with its own adapter
// (WrapStatementDB) from *sql.DB.
type StatementDB interface {
	DB
	QueryContext(ctx context.Context, query string, args ...any) (Rows, error)
}

// WrapStatementDB adapts a *sql.DB for use as a StatementDB, the same way
// Wrap (accounts_store.go) adapts one to a Conn.
func WrapStatementDB(db *sql.DB) StatementDB {
	return sqlDB{db}
}

// QueryContext makes sqlDB (already DB and Beginner, see accounts_store.go)
// also satisfy StatementDB. *sql.DB's QueryContext returns the concrete
// *sql.Rows, which already implements Rows, but Go requires an exact
// method signature match, so this thin wrapper is what actually makes
// sqlDB satisfy StatementDB.
func (s sqlDB) QueryContext(ctx context.Context, query string, args ...any) (Rows, error) {
	return s.DB.QueryContext(ctx, query, args...)
}

// StatementEntry is one row of a ledger account's statement: an Entry (see
// entry.go) plus the account's running balance immediately after it, per
// this issue's acceptance criteria ("Response includes running balance
// after each entry").
type StatementEntry struct {
	Entry

	// RunningBalance is the account's balance immediately after this entry
	// posted: SUM(amount) over every entry ever posted to the account with
	// a (created_at, id) at or before this one. It is computed the exact
	// same way GetBalance computes an account's current balance (see
	// balance.go) — SUM() over journal_entries, never a stored counter —
	// just evaluated as of this entry instead of as of now.
	RunningBalance int64
}

// StatementQuery narrows one GetAccountStatement call. Every field is
// optional; its zero value means "no filter" (From/To) or "first page"
// (Cursor) or "default page size" (Limit).
type StatementQuery struct {
	// From and To bound created_at inclusively (From <= created_at <= To).
	// A zero Time leaves that bound off. Both set, with From after To, is
	// an error (ErrInvalidDateRange).
	From, To time.Time

	// Cursor continues a previous call from where it left off: pass the
	// StatementPage.NextCursor a prior call returned. Empty starts from
	// the account's very first entry.
	Cursor string

	// Limit caps how many entries one page returns. <=0 defaults to
	// DefaultStatementPageSize; a value over MaxStatementPageSize is
	// silently clamped to it.
	Limit int
}

const (
	// DefaultStatementPageSize is the page size GetAccountStatement uses
	// when StatementQuery.Limit is <=0.
	DefaultStatementPageSize = 50
	// MaxStatementPageSize is the largest page GetAccountStatement ever
	// returns, regardless of what StatementQuery.Limit asks for — so one
	// call can't force an unbounded scan/response.
	MaxStatementPageSize = 200
)

// StatementPage is one page of GetAccountStatement's result.
type StatementPage struct {
	Entries []StatementEntry

	// NextCursor, when non-empty, is passed as a later call's
	// StatementQuery.Cursor to continue immediately after the last entry
	// in Entries. Empty means Entries already reached the account's most
	// recent (or, with StatementQuery.To set, last in-range) entry.
	NextCursor string
}

// ErrInvalidStatementCursor is the domain error GetAccountStatement wraps
// into its returned error when StatementQuery.Cursor is not a cursor this
// package itself produced (a forged, corrupted, or stale-format value).
var ErrInvalidStatementCursor = errors.New("ledger: invalid statement cursor")

// ErrInvalidDateRange is the domain error GetAccountStatement wraps into
// its returned error when StatementQuery.From is after StatementQuery.To.
var ErrInvalidDateRange = errors.New("ledger: statement query 'from' is after 'to'")

// statementPageQuery lists the requested page of one account's
// journal_entries, each paired with the account's running balance as of
// that row.
//
// The running balance is computed by a window function over every entry
// ever posted to the account ("running", scoped by account_id alone, with
// no date filter) — never just over the rows a date-filtered page happens
// to return — so RunningBalance always reflects the account's true balance
// at that point in its full history, the way a real bank statement's
// running balance does even when the statement itself only lists one
// month's entries. The outer SELECT then applies the optional date-range
// filter and the cursor, in that order, to pick which rows of that true
// running history to actually return.
//
// $1 = account_id
// $2, $3 = optional created_at lower/upper bound (NULL = unbounded)
// $4, $5 = optional cursor (created_at, id) — NULL $4 = no cursor, start
// from the beginning
// $6 = LIMIT (callers pass pageSize+1, to detect whether a next page
// exists, and trim the extra row themselves before returning)
const statementPageQuery = `
	WITH running AS (
		SELECT id, transaction_ref, account_id, amount, currency, created_at,
		       SUM(amount) OVER (ORDER BY created_at, id) AS running_balance
		FROM journal_entries
		WHERE account_id = $1
	)
	SELECT id, transaction_ref, account_id, amount, currency, created_at, running_balance
	FROM running
	WHERE ($2::timestamptz IS NULL OR created_at >= $2)
	  AND ($3::timestamptz IS NULL OR created_at <= $3)
	  AND ($4::timestamptz IS NULL OR (created_at, id) > ($4, $5))
	ORDER BY created_at, id
	LIMIT $6`

// GetAccountStatement returns one page of merchantID's own merchant_payable
// account statement: journal_entries posted against it, ordered by time
// (created_at, then id to break ties), each paired with the account's
// running balance immediately after it.
//
// GetAccountStatement is merchant-scoped by construction, per this issue's
// acceptance criteria ("a merchant can only query their own
// merchant_payable account"): it takes a merchantID, never an accountID, and
// resolves it to that merchant's own account itself, via
// MerchantPayableAccountCode + GetAccountByCode — there is no parameter
// through which a caller could name a different account, merchant_payable
// or otherwise. A caller (e.g. a future HTTP handler) is responsible for
// authenticating the request to a merchantID in the first place; once it
// has one, this function cannot be made to read any other merchant's
// ledger.
//
// Pagination is cursor-based (StatementQuery.Cursor /
// StatementPage.NextCursor), not offset-based, so a page is stable under
// concurrent inserts: a dashboard polling for new entries, or support/ops
// paging through a very active account, never skips or re-sees a row
// because some other transaction posting in between pages shifted what an
// offset would have pointed at.
func GetAccountStatement(ctx context.Context, db StatementDB, merchantID string, query StatementQuery) (StatementPage, error) {
	if !query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To) {
		return StatementPage{}, fmt.Errorf("ledger: statement query for merchant %q has from %s after to %s: %w", merchantID, query.From, query.To, ErrInvalidDateRange)
	}

	account, err := merchantPayableAccountFor(ctx, db, merchantID)
	if err != nil {
		return StatementPage{}, err
	}

	limit := query.Limit
	switch {
	case limit <= 0:
		limit = DefaultStatementPageSize
	case limit > MaxStatementPageSize:
		limit = MaxStatementPageSize
	}

	var cursorArg *time.Time
	var afterID int64
	if query.Cursor != "" {
		afterCreatedAt, id, err := decodeStatementCursor(query.Cursor)
		if err != nil {
			return StatementPage{}, fmt.Errorf("ledger: statement query for merchant %q: %w", merchantID, err)
		}
		cursorArg = &afterCreatedAt
		afterID = id
	}

	rows, err := db.QueryContext(ctx, statementPageQuery,
		account.ID,
		timePtrOrNil(query.From),
		timePtrOrNil(query.To),
		cursorArg,
		afterID,
		limit+1,
	)
	if err != nil {
		return StatementPage{}, fmt.Errorf("ledger: list statement for merchant %q account %d: %w", merchantID, account.ID, err)
	}
	defer rows.Close()

	entries := make([]StatementEntry, 0, limit+1)
	for rows.Next() {
		var se StatementEntry
		if err := rows.Scan(&se.ID, &se.TransactionRef, &se.AccountID, &se.Amount, &se.Currency, &se.CreatedAt, &se.RunningBalance); err != nil {
			return StatementPage{}, fmt.Errorf("ledger: scan statement row for merchant %q account %d: %w", merchantID, account.ID, err)
		}
		entries = append(entries, se)
	}
	if err := rows.Err(); err != nil {
		return StatementPage{}, fmt.Errorf("ledger: list statement for merchant %q account %d: %w", merchantID, account.ID, err)
	}

	page := StatementPage{Entries: entries}
	if len(entries) > limit {
		// The +1 row only existed to signal "there is more" — never
		// returned to the caller.
		page.Entries = entries[:limit]
		last := page.Entries[limit-1]
		page.NextCursor = encodeStatementCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// merchantPayableAccountFor resolves merchantID to its own merchant_payable
// account, the one and only account GetAccountStatement is ever allowed to
// read.
func merchantPayableAccountFor(ctx context.Context, db DB, merchantID string) (Account, error) {
	code, err := MerchantPayableAccountCode(merchantID)
	if err != nil {
		return Account{}, err
	}
	account, err := GetAccountByCode(ctx, db, code)
	if err != nil {
		return Account{}, fmt.Errorf("ledger: statement for merchant %q: %w", merchantID, err)
	}
	return account, nil
}

// timePtrOrNil returns nil for a zero time.Time (so the ($N::timestamptz IS
// NULL OR ...) clauses in statementPageQuery see an unbounded filter) or a
// pointer to t otherwise. database/sql substitutes a nil *time.Time
// argument as SQL NULL and dereferences a non-nil one, so this is all a
// caller needs to do to pass an optional timestamp bound.
func timePtrOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// encodeStatementCursor packs the (created_at, id) of one statement row
// into the opaque string GetAccountStatement hands back as
// StatementPage.NextCursor. It is deliberately not a raw "timestamp:id"
// string — base64 keeps it one URL-safe token a caller can round-trip
// through a query parameter without further escaping, and signals (even if
// it doesn't cryptographically enforce) that callers should treat it as
// opaque rather than parse or construct one themselves.
func encodeStatementCursor(createdAt time.Time, id int64) string {
	raw := fmt.Sprintf("%d:%d", createdAt.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeStatementCursor reverses encodeStatementCursor, reporting an error
// wrapping ErrInvalidStatementCursor for anything that isn't a cursor this
// package itself produced.
func decodeStatementCursor(cursor string) (createdAt time.Time, id int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: %v", ErrInvalidStatementCursor, err)
	}

	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("%w: wrong number of fields", ErrInvalidStatementCursor)
	}

	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: %v", ErrInvalidStatementCursor, err)
	}
	rowID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: %v", ErrInvalidStatementCursor, err)
	}

	return time.Unix(0, nanos).UTC(), rowID, nil
}
