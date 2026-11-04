package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeRows is a Rows backed by an in-memory table of scan destinations, so
// tests can exercise GetAccountStatement's paging/scan logic without a live
// database.
type fakeRows struct {
	data []statementTestRow
	idx  int
	err  error
}

type statementTestRow struct {
	id             int64
	transactionRef string
	accountID      int64
	amount         int64
	currency       string
	createdAt      time.Time
	runningBalance int64
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if len(dest) != 7 {
		panic(fmt.Sprintf("fakeRows.Scan: got %d dest, want 7", len(dest)))
	}
	row := r.data[r.idx-1]
	*dest[0].(*int64) = row.id
	*dest[1].(*string) = row.transactionRef
	*dest[2].(*int64) = row.accountID
	*dest[3].(*int64) = row.amount
	*dest[4].(*string) = row.currency
	*dest[5].(*time.Time) = row.createdAt
	*dest[6].(*int64) = row.runningBalance
	return nil
}

func (r *fakeRows) Err() error   { return r.err }
func (r *fakeRows) Close() error { return nil }

// fakeStatementDB is a StatementDB that records every call, so tests can
// assert on the args GetAccountStatement passes through without a live
// database. Its QueryRowContext (the DB half, used to resolve the
// merchant's account by code) defaults to reporting a fixed Account for any
// code, letting most tests skip wiring that up explicitly.
type fakeStatementDB struct {
	account    Account
	accountErr error
	rowQueries []execCall

	rows     Rows
	queryErr error
	queries  []execCall
}

func (f *fakeStatementDB) QueryRowContext(_ context.Context, query string, args ...any) Row {
	f.rowQueries = append(f.rowQueries, execCall{query: query, args: args})
	return fakeRow{scan: func(dest ...any) error {
		if f.accountErr != nil {
			return f.accountErr
		}
		*dest[0].(*int64) = f.account.ID
		*dest[1].(*string) = f.account.Code
		*dest[2].(*string) = string(f.account.Type)
		*dest[3].(*string) = f.account.Name
		*dest[4].(*time.Time) = f.account.CreatedAt
		return nil
	}}
}

func (f *fakeStatementDB) QueryContext(_ context.Context, query string, args ...any) (Rows, error) {
	f.queries = append(f.queries, execCall{query: query, args: args})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

func newFakeStatementDB(account Account, rows []statementTestRow) *fakeStatementDB {
	return &fakeStatementDB{account: account, rows: &fakeRows{data: rows}}
}

func TestGetAccountStatementResolvesMerchantsOwnAccount(t *testing.T) {
	account := Account{ID: 42, Code: "merchant_payable:mch_1", Type: LiabilityAccount}
	db := newFakeStatementDB(account, nil)

	if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{}); err != nil {
		t.Fatalf("GetAccountStatement returned error: %v", err)
	}

	if len(db.rowQueries) != 1 {
		t.Fatalf("QueryRowContext called %d times, want 1 (account lookup)", len(db.rowQueries))
	}
	wantCode := "merchant_payable:mch_1"
	if len(db.rowQueries[0].args) != 1 || db.rowQueries[0].args[0] != wantCode {
		t.Errorf("account lookup args = %v, want [%q]", db.rowQueries[0].args, wantCode)
	}

	if len(db.queries) != 1 {
		t.Fatalf("QueryContext called %d times, want 1", len(db.queries))
	}
	if got := db.queries[0].args[0]; got != account.ID {
		t.Errorf("QueryContext first arg (account_id) = %v, want %d", got, account.ID)
	}
}

func TestGetAccountStatementRejectsUnknownMerchantWithoutListingEntries(t *testing.T) {
	db := newFakeStatementDB(Account{}, nil)
	db.accountErr = errors.New("sql: no rows in result set")

	if _, err := GetAccountStatement(context.Background(), db, "mch_missing", StatementQuery{}); err == nil {
		t.Fatal("GetAccountStatement() = nil error, want an error for a merchant with no ledger account yet")
	}
	if len(db.queries) != 0 {
		t.Errorf("QueryContext called %d times, want 0 when the account lookup failed", len(db.queries))
	}
}

func TestGetAccountStatementIncludesRunningBalancePerEntry(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []statementTestRow{
		{id: 1, transactionRef: "txn_1", accountID: 42, amount: 10000, currency: "INR", createdAt: t0, runningBalance: 10000},
		{id: 2, transactionRef: "txn_2", accountID: 42, amount: -2500, currency: "INR", createdAt: t0.Add(time.Minute), runningBalance: 7500},
	}
	db := newFakeStatementDB(Account{ID: 42}, rows)

	page, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{})
	if err != nil {
		t.Fatalf("GetAccountStatement returned error: %v", err)
	}

	if len(page.Entries) != 2 {
		t.Fatalf("len(page.Entries) = %d, want 2", len(page.Entries))
	}
	if page.Entries[0].RunningBalance != 10000 || page.Entries[1].RunningBalance != 7500 {
		t.Errorf("running balances = [%d, %d], want [10000, 7500]", page.Entries[0].RunningBalance, page.Entries[1].RunningBalance)
	}
	if page.Entries[0].ID != 1 || page.Entries[1].ID != 2 {
		t.Errorf("entry ids = [%d, %d], want [1, 2] (time order)", page.Entries[0].ID, page.Entries[1].ID)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty for a page smaller than the limit", page.NextCursor)
	}
}

func TestGetAccountStatementSetsNextCursorWhenMoreRowsExist(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]statementTestRow, 0, 3)
	for i := int64(1); i <= 3; i++ {
		rows = append(rows, statementTestRow{
			id: i, transactionRef: fmt.Sprintf("txn_%d", i), accountID: 42,
			amount: 100, currency: "INR", createdAt: t0.Add(time.Duration(i) * time.Minute), runningBalance: 100 * i,
		})
	}
	db := newFakeStatementDB(Account{ID: 42}, rows)

	page, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{Limit: 2})
	if err != nil {
		t.Fatalf("GetAccountStatement returned error: %v", err)
	}

	if len(page.Entries) != 2 {
		t.Fatalf("len(page.Entries) = %d, want 2 (Limit), not the extra lookahead row", len(page.Entries))
	}
	if page.NextCursor == "" {
		t.Fatal("NextCursor = \"\", want non-empty: a 3rd row exists beyond the 2-row page")
	}

	// The stored LIMIT argument must be Limit+1 (the lookahead row), not
	// Limit itself.
	gotLimitArg := db.queries[0].args[len(db.queries[0].args)-1]
	if gotLimitArg != 3 {
		t.Errorf("QueryContext LIMIT arg = %v, want 3 (Limit=2 + 1 lookahead)", gotLimitArg)
	}

	// The cursor must decode back to the last *returned* entry (id=2), not
	// the lookahead row (id=3).
	gotCreatedAt, gotID, err := decodeStatementCursor(page.NextCursor)
	if err != nil {
		t.Fatalf("decodeStatementCursor(%q): %v", page.NextCursor, err)
	}
	if gotID != 2 || !gotCreatedAt.Equal(page.Entries[1].CreatedAt) {
		t.Errorf("decoded cursor = (%v, %d), want (%v, 2)", gotCreatedAt, gotID, page.Entries[1].CreatedAt)
	}
}

func TestGetAccountStatementDefaultsAndClampsLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"zero defaults", 0, DefaultStatementPageSize},
		{"negative defaults", -5, DefaultStatementPageSize},
		{"within range unchanged", 10, 10},
		{"over max clamped", MaxStatementPageSize + 500, MaxStatementPageSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newFakeStatementDB(Account{ID: 42}, nil)
			if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{Limit: tt.limit}); err != nil {
				t.Fatalf("GetAccountStatement returned error: %v", err)
			}
			gotLimitArg := db.queries[0].args[len(db.queries[0].args)-1]
			if gotLimitArg != tt.wantLimit+1 {
				t.Errorf("LIMIT arg = %v, want %d (%d + 1 lookahead)", gotLimitArg, tt.wantLimit+1, tt.wantLimit)
			}
		})
	}
}

func TestGetAccountStatementPassesDateRangeAsNilWhenUnset(t *testing.T) {
	db := newFakeStatementDB(Account{ID: 42}, nil)

	if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{}); err != nil {
		t.Fatalf("GetAccountStatement returned error: %v", err)
	}

	args := db.queries[0].args
	if args[1] != (*time.Time)(nil) {
		t.Errorf("from arg = %v, want nil (*time.Time) for an unset From", args[1])
	}
	if args[2] != (*time.Time)(nil) {
		t.Errorf("to arg = %v, want nil (*time.Time) for an unset To", args[2])
	}
}

func TestGetAccountStatementPassesDateRangeBounds(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	db := newFakeStatementDB(Account{ID: 42}, nil)

	if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{From: from, To: to}); err != nil {
		t.Fatalf("GetAccountStatement returned error: %v", err)
	}

	args := db.queries[0].args
	gotFrom, ok := args[1].(*time.Time)
	if !ok || gotFrom == nil || !gotFrom.Equal(from) {
		t.Errorf("from arg = %v, want pointer to %v", args[1], from)
	}
	gotTo, ok := args[2].(*time.Time)
	if !ok || gotTo == nil || !gotTo.Equal(to) {
		t.Errorf("to arg = %v, want pointer to %v", args[2], to)
	}
}

func TestGetAccountStatementRejectsFromAfterToWithoutTouchingDB(t *testing.T) {
	db := newFakeStatementDB(Account{ID: 42}, nil)
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{From: from, To: to})
	if err == nil {
		t.Fatal("GetAccountStatement() = nil error, want an error when From is after To")
	}
	if !errors.Is(err, ErrInvalidDateRange) {
		t.Errorf("error = %v, want it to wrap ErrInvalidDateRange", err)
	}
	if len(db.rowQueries) != 0 || len(db.queries) != 0 {
		t.Error("GetAccountStatement touched the database for an invalid date range")
	}
}

func TestGetAccountStatementRejectsInvalidCursorWithoutTouchingDB(t *testing.T) {
	db := newFakeStatementDB(Account{ID: 42}, nil)

	_, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{Cursor: "not valid base64url!!"})
	if err == nil {
		t.Fatal("GetAccountStatement() = nil error, want an error for a malformed cursor")
	}
	if !errors.Is(err, ErrInvalidStatementCursor) {
		t.Errorf("error = %v, want it to wrap ErrInvalidStatementCursor", err)
	}
	if len(db.queries) != 0 {
		t.Error("GetAccountStatement called QueryContext for a malformed cursor")
	}
}

func TestGetAccountStatementPropagatesQueryError(t *testing.T) {
	db := newFakeStatementDB(Account{ID: 42}, nil)
	db.queryErr = errors.New("connection reset")

	if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{}); err == nil {
		t.Fatal("GetAccountStatement() = nil error, want the propagated query error")
	}
}

func TestGetAccountStatementPropagatesRowsErr(t *testing.T) {
	db := newFakeStatementDB(Account{ID: 42}, nil)
	db.rows = &fakeRows{err: errors.New("row iteration failed")}

	if _, err := GetAccountStatement(context.Background(), db, "mch_1", StatementQuery{}); err == nil {
		t.Fatal("GetAccountStatement() = nil error, want the propagated rows.Err()")
	}
}

func TestStatementCursorRoundTrips(t *testing.T) {
	want := time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC)
	cursor := encodeStatementCursor(want, 123)

	gotTime, gotID, err := decodeStatementCursor(cursor)
	if err != nil {
		t.Fatalf("decodeStatementCursor(%q): %v", cursor, err)
	}
	if gotID != 123 {
		t.Errorf("decoded id = %d, want 123", gotID)
	}
	if !gotTime.Equal(want) {
		t.Errorf("decoded time = %v, want %v", gotTime, want)
	}
}

func TestDecodeStatementCursorRejectsGarbage(t *testing.T) {
	cases := []string{"", "not-base64!!", "aGVsbG8", "YWJjOmRlZg"} // last two decode but aren't "nanos:id"
	for _, c := range cases {
		if _, _, err := decodeStatementCursor(c); err == nil {
			t.Errorf("decodeStatementCursor(%q) = nil error, want an error", c)
		}
	}
}
