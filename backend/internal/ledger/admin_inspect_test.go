package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// inspectionRow is one row fakeInspectionRows yields, matching
// inspectionEntriesQuery's column order (id, transaction_ref, account_id,
// code, amount, currency, created_at).
type inspectionRow struct {
	id             int64
	transactionRef string
	accountID      int64
	accountCode    string
	amount         int64
	currency       string
	createdAt      time.Time
}

// fakeInspectionRows is a Rows backed by an in-memory table of
// inspectionRows, so tests can exercise queryInspectionEntries' grouping
// logic without a live database. It is distinct from statement_test.go's
// fakeRows: that type's Scan expects a different column shape (a running
// balance, no account code).
type fakeInspectionRows struct {
	data []inspectionRow
	idx  int
	err  error
}

func (r *fakeInspectionRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeInspectionRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	*dest[0].(*int64) = row.id
	*dest[1].(*string) = row.transactionRef
	*dest[2].(*int64) = row.accountID
	*dest[3].(*string) = row.accountCode
	*dest[4].(*int64) = row.amount
	*dest[5].(*string) = row.currency
	*dest[6].(*time.Time) = row.createdAt
	return nil
}

func (r *fakeInspectionRows) Err() error   { return r.err }
func (r *fakeInspectionRows) Close() error { return nil }

// fakeInspectionDB is an InspectionDB that records every QueryContext and
// ExecContext call, so tests can assert on what InspectTransactions sent
// the database without a live one.
type fakeInspectionDB struct {
	rows     Rows
	queryErr error
	queries  []execCall

	execErr error
	execs   []execCall
}

func (f *fakeInspectionDB) QueryRowContext(_ context.Context, _ string, _ ...any) Row {
	panic("fakeInspectionDB: QueryRowContext not used by InspectTransactions")
}

func (f *fakeInspectionDB) QueryContext(_ context.Context, query string, args ...any) (Rows, error) {
	f.queries = append(f.queries, execCall{query: query, args: args})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

func (f *fakeInspectionDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.execs = append(f.execs, execCall{query: query, args: args})
	if f.execErr != nil {
		return nil, f.execErr
	}
	return nil, nil
}

func newFakeInspectionDB(rows []inspectionRow) *fakeInspectionDB {
	return &fakeInspectionDB{rows: &fakeInspectionRows{data: rows}}
}

func TestInspectTransactionsRejectsNonAdminWithoutQueryingAndAuditsDenial(t *testing.T) {
	db := newFakeInspectionDB(nil)

	_, err := InspectTransactions(context.Background(), db, Actor{ID: "mch_1", Role: RoleMerchant}, []string{"txn_1"})
	if err == nil {
		t.Fatal("InspectTransactions() = nil error, want an error for a non-admin actor")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("error = %v, want it to wrap ErrForbidden", err)
	}
	if len(db.queries) != 0 {
		t.Errorf("QueryContext called %d times, want 0 for a denied actor", len(db.queries))
	}

	if len(db.execs) != 1 {
		t.Fatalf("ExecContext (audit log) called %d times, want 1 even for a denied actor", len(db.execs))
	}
	args := db.execs[0].args
	if args[0] != "mch_1" || args[1] != string(RoleMerchant) {
		t.Errorf("audit log args = %v, want actor_id=mch_1 actor_role=merchant", args)
	}
	if allowed, ok := args[3].(bool); !ok || allowed {
		t.Errorf("audit log allowed arg = %v, want false", args[3])
	}
}

func TestInspectTransactionsRejectsEmptyRefsWithoutTouchingDB(t *testing.T) {
	db := newFakeInspectionDB(nil)

	if _, err := InspectTransactions(context.Background(), db, Actor{ID: "admin_1", Role: RoleAdmin}, nil); err == nil {
		t.Fatal("InspectTransactions() = nil error, want an error for no transaction_ref")
	}
	if len(db.queries) != 0 || len(db.execs) != 0 {
		t.Error("InspectTransactions touched the database for an empty transaction_ref list")
	}
}

func TestInspectTransactionsGroupsByTransactionRefInChronologicalOrder(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []inspectionRow{
		{id: 1, transactionRef: "txn_1", accountID: 10, accountCode: "customer_suspense", amount: 10000, currency: "INR", createdAt: t0},
		{id: 2, transactionRef: "txn_1", accountID: 20, accountCode: "merchant_payable:mch_1", amount: -10000, currency: "INR", createdAt: t0.Add(time.Minute)},
		{id: 3, transactionRef: "txn_2", accountID: 10, accountCode: "customer_suspense", amount: 5000, currency: "INR", createdAt: t0.Add(2 * time.Minute)},
		{id: 4, transactionRef: "txn_2", accountID: 30, accountCode: "merchant_payable:mch_2", amount: -5000, currency: "INR", createdAt: t0.Add(3 * time.Minute)},
	}
	db := newFakeInspectionDB(rows)

	journals, err := InspectTransactions(context.Background(), db, Actor{ID: "admin_1", Role: RoleAdmin}, []string{"txn_1", "txn_2"})
	if err != nil {
		t.Fatalf("InspectTransactions returned error: %v", err)
	}

	if len(journals) != 2 {
		t.Fatalf("len(journals) = %d, want 2", len(journals))
	}
	if journals[0].TransactionRef != "txn_1" || len(journals[0].Entries) != 2 {
		t.Fatalf("journals[0] = %+v, want txn_1 with 2 entries", journals[0])
	}
	if journals[0].Entries[0].ID != 1 || journals[0].Entries[1].ID != 2 {
		t.Errorf("txn_1 entry ids = [%d, %d], want [1, 2] (chronological)", journals[0].Entries[0].ID, journals[0].Entries[1].ID)
	}
	if journals[0].Entries[0].AccountCode != "customer_suspense" {
		t.Errorf("entry AccountCode = %q, want %q (readable code, not just account_id)", journals[0].Entries[0].AccountCode, "customer_suspense")
	}
	if journals[1].TransactionRef != "txn_2" || len(journals[1].Entries) != 2 {
		t.Fatalf("journals[1] = %+v, want txn_2 with 2 entries", journals[1])
	}

	// InspectTransactions must have passed the ANY($1) argument once as a
	// single query, and recorded exactly one allowed audit log row.
	if len(db.queries) != 1 {
		t.Fatalf("QueryContext called %d times, want 1", len(db.queries))
	}
	if len(db.execs) != 1 {
		t.Fatalf("ExecContext (audit log) called %d times, want 1", len(db.execs))
	}
	if allowed, ok := db.execs[0].args[3].(bool); !ok || !allowed {
		t.Errorf("audit log allowed arg = %v, want true", db.execs[0].args[3])
	}
}

func TestInspectTransactionsDedupesTransactionRefs(t *testing.T) {
	db := newFakeInspectionDB(nil)

	if _, err := InspectTransactions(context.Background(), db, Actor{ID: "admin_1", Role: RoleAdmin}, []string{"txn_1", "txn_1", "", "txn_1"}); err != nil {
		t.Fatalf("InspectTransactions returned error: %v", err)
	}

	gotRefsArg := db.queries[0].args[0]
	if gotRefsArg != inspectionRefsArrayLiteral([]string{"txn_1"}) {
		t.Errorf("refs array literal = %v, want the deduped single-element literal", gotRefsArg)
	}

	gotAuditRefs := db.execs[0].args[2]
	if gotAuditRefs != `["txn_1"]` {
		t.Errorf("audit log transaction_refs = %v, want deduped JSON array", gotAuditRefs)
	}
}

func TestInspectTransactionsPropagatesQueryError(t *testing.T) {
	db := newFakeInspectionDB(nil)
	db.queryErr = errors.New("connection reset")

	if _, err := InspectTransactions(context.Background(), db, Actor{ID: "admin_1", Role: RoleAdmin}, []string{"txn_1"}); err == nil {
		t.Fatal("InspectTransactions() = nil error, want the propagated query error")
	}
	if len(db.execs) != 0 {
		t.Error("InspectTransactions wrote an audit log row despite the query failing")
	}
}

func TestInspectTransactionsFailsClosedWhenAuditLogWriteFails(t *testing.T) {
	db := newFakeInspectionDB([]inspectionRow{
		{id: 1, transactionRef: "txn_1", accountID: 10, accountCode: "customer_suspense", amount: 10000, currency: "INR", createdAt: time.Now()},
	})
	db.execErr = errors.New("audit log insert failed")

	if _, err := InspectTransactions(context.Background(), db, Actor{ID: "admin_1", Role: RoleAdmin}, []string{"txn_1"}); err == nil {
		t.Fatal("InspectTransactions() = nil error, want an error when the audit log write fails (fail closed)")
	}
}

func TestInspectionRefsArrayLiteralEscapesQuotesAndBackslashes(t *testing.T) {
	got := inspectionRefsArrayLiteral([]string{`has"quote`, `has\backslash`})
	want := `{"has\"quote","has\\backslash"}`
	if got != want {
		t.Errorf("inspectionRefsArrayLiteral = %q, want %q", got, want)
	}
}

func TestDedupeNonEmptyPreservesOrderAndDropsEmpty(t *testing.T) {
	got := dedupeNonEmpty([]string{"b", "", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupeNonEmpty = %v, want %v", got, want)
			break
		}
	}
}
