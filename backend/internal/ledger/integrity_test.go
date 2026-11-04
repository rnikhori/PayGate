package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeIntegrityRow is the row statementTestRow's sibling for this file's
// tests: a single-column Row (global sum) whose Scan is supplied directly,
// matching the fakeRow pattern in accounts_store_test.go.
type fakeIntegrityDB struct {
	globalSum    int64
	globalSumErr error
	rowCalls     []execCall

	rows     Rows
	queryErr error
	queries  []execCall
}

func (f *fakeIntegrityDB) QueryRowContext(_ context.Context, query string, args ...any) Row {
	f.rowCalls = append(f.rowCalls, execCall{query: query, args: args})
	return fakeRow{scan: func(dest ...any) error {
		if f.globalSumErr != nil {
			return f.globalSumErr
		}
		*dest[0].(*int64) = f.globalSum
		return nil
	}}
}

func (f *fakeIntegrityDB) QueryContext(_ context.Context, query string, args ...any) (Rows, error) {
	f.queries = append(f.queries, execCall{query: query, args: args})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

// unbalancedTestRow is one row fakeUnbalancedRows hands back, mirroring
// what unbalancedTransactionsQuery actually selects.
type unbalancedTestRow struct {
	transactionRef string
	currency       string
	sum            int64
	accountCodes   string // the raw Postgres array literal, e.g. "{a,b}"
}

type fakeUnbalancedRows struct {
	data []unbalancedTestRow
	idx  int
	err  error
}

func (r *fakeUnbalancedRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeUnbalancedRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	*dest[0].(*string) = row.transactionRef
	*dest[1].(*string) = row.currency
	*dest[2].(*int64) = row.sum
	return dest[3].(*accountCodeArray).Scan(row.accountCodes)
}

func (r *fakeUnbalancedRows) Err() error   { return r.err }
func (r *fakeUnbalancedRows) Close() error { return nil }

func TestCheckIntegrityReturnsPassingReportForBalancedLedger(t *testing.T) {
	db := &fakeIntegrityDB{globalSum: 0, rows: &fakeUnbalancedRows{}}

	report, err := CheckIntegrity(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckIntegrity returned error: %v", err)
	}
	if !report.Passed() {
		t.Errorf("report.Passed() = false, want true for a zero global sum and no unbalanced transactions")
	}
	if report.GlobalSum != 0 {
		t.Errorf("report.GlobalSum = %d, want 0", report.GlobalSum)
	}
	if len(report.UnbalancedTransactions) != 0 {
		t.Errorf("len(report.UnbalancedTransactions) = %d, want 0", len(report.UnbalancedTransactions))
	}
	if report.CheckedAt.IsZero() {
		t.Error("report.CheckedAt is zero, want it set to roughly now")
	}
}

func TestCheckIntegrityDetectsNonZeroGlobalSum(t *testing.T) {
	db := &fakeIntegrityDB{globalSum: 500, rows: &fakeUnbalancedRows{}}

	report, err := CheckIntegrity(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckIntegrity returned error: %v", err)
	}
	if report.Passed() {
		t.Error("report.Passed() = true, want false for a nonzero global sum")
	}
	if report.GlobalSum != 500 {
		t.Errorf("report.GlobalSum = %d, want 500", report.GlobalSum)
	}
}

func TestCheckIntegrityReportsUnbalancedTransactionsWithAccountCodes(t *testing.T) {
	rows := []unbalancedTestRow{
		{transactionRef: "txn_bad_1", currency: "INR", sum: 150, accountCodes: "{customer_suspense,merchant_payable:mch_1}"},
	}
	db := &fakeIntegrityDB{globalSum: 0, rows: &fakeUnbalancedRows{data: rows}}

	report, err := CheckIntegrity(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckIntegrity returned error: %v", err)
	}
	if report.Passed() {
		t.Error("report.Passed() = true, want false when an unbalanced transaction was found")
	}
	if len(report.UnbalancedTransactions) != 1 {
		t.Fatalf("len(report.UnbalancedTransactions) = %d, want 1", len(report.UnbalancedTransactions))
	}
	got := report.UnbalancedTransactions[0]
	want := UnbalancedTransaction{
		TransactionRef: "txn_bad_1",
		Currency:       "INR",
		Sum:            150,
		AccountCodes:   []string{"customer_suspense", "merchant_payable:mch_1"},
	}
	if got.TransactionRef != want.TransactionRef || got.Currency != want.Currency || got.Sum != want.Sum {
		t.Errorf("UnbalancedTransactions[0] = %+v, want %+v", got, want)
	}
	if len(got.AccountCodes) != 2 || got.AccountCodes[0] != want.AccountCodes[0] || got.AccountCodes[1] != want.AccountCodes[1] {
		t.Errorf("AccountCodes = %v, want %v", got.AccountCodes, want.AccountCodes)
	}
}

func TestCheckIntegrityPropagatesGlobalSumError(t *testing.T) {
	db := &fakeIntegrityDB{globalSumErr: errors.New("connection reset")}

	if _, err := CheckIntegrity(context.Background(), db); err == nil {
		t.Fatal("CheckIntegrity() = nil error, want the propagated global sum query error")
	}
	if len(db.queries) != 0 {
		t.Error("CheckIntegrity issued the unbalanced-transactions query after the global sum query already failed")
	}
}

func TestCheckIntegrityPropagatesQueryContextError(t *testing.T) {
	db := &fakeIntegrityDB{globalSum: 0, queryErr: errors.New("connection reset")}

	if _, err := CheckIntegrity(context.Background(), db); err == nil {
		t.Fatal("CheckIntegrity() = nil error, want the propagated QueryContext error")
	}
}

func TestCheckIntegrityPropagatesRowsErr(t *testing.T) {
	db := &fakeIntegrityDB{globalSum: 0, rows: &fakeUnbalancedRows{err: errors.New("row iteration failed")}}

	if _, err := CheckIntegrity(context.Background(), db); err == nil {
		t.Fatal("CheckIntegrity() = nil error, want the propagated rows.Err()")
	}
}

func TestAccountCodeArrayScanParsesPostgresArrayLiteral(t *testing.T) {
	tests := []struct {
		name string
		src  any
		want []string
	}{
		{"multiple codes", "{customer_suspense,merchant_payable:mch_1}", []string{"customer_suspense", "merchant_payable:mch_1"}},
		{"single code", "{gateway_fee_revenue}", []string{"gateway_fee_revenue"}},
		{"empty array", "{}", []string{}},
		{"bytes input", []byte("{settlement_clearing}"), []string{"settlement_clearing"}},
		{"nil input", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a accountCodeArray
			if err := a.Scan(tt.src); err != nil {
				t.Fatalf("Scan(%v) returned error: %v", tt.src, err)
			}
			if len(a) != len(tt.want) {
				t.Fatalf("Scan(%v) = %v, want %v", tt.src, a, tt.want)
			}
			for i := range tt.want {
				if a[i] != tt.want[i] {
					t.Errorf("Scan(%v)[%d] = %q, want %q", tt.src, i, a[i], tt.want[i])
				}
			}
		})
	}
}

func TestAccountCodeArrayScanRejectsMalformedValue(t *testing.T) {
	cases := []any{"not an array", "", 42}
	for _, c := range cases {
		var a accountCodeArray
		if err := a.Scan(c); err == nil {
			t.Errorf("Scan(%v) = nil error, want an error for a malformed value", c)
		}
	}
}

func TestIntegrityReportPassed(t *testing.T) {
	tests := []struct {
		name   string
		report IntegrityReport
		want   bool
	}{
		{"zero sum, no unbalanced", IntegrityReport{GlobalSum: 0}, true},
		{"nonzero sum", IntegrityReport{GlobalSum: 1}, false},
		{"zero sum but an unbalanced transaction", IntegrityReport{GlobalSum: 0, UnbalancedTransactions: []UnbalancedTransaction{{TransactionRef: "t"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.report.Passed(); got != tt.want {
				t.Errorf("Passed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordIntegrityCheckInsertsRowAndReturnsID(t *testing.T) {
	checkedAt := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	report := IntegrityReport{
		CheckedAt: checkedAt,
		GlobalSum: 0,
	}
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = 7
		return nil
	}}}

	id, err := RecordIntegrityCheck(context.Background(), db, ScheduledIntegrityCheck, report)
	if err != nil {
		t.Fatalf("RecordIntegrityCheck returned error: %v", err)
	}
	if id != 7 {
		t.Errorf("RecordIntegrityCheck() id = %d, want 7", id)
	}
	if !db.called {
		t.Fatal("RecordIntegrityCheck never called QueryRowContext")
	}

	args := db.args
	if args[0] != checkedAt {
		t.Errorf("args[0] (checked_at) = %v, want %v", args[0], checkedAt)
	}
	if args[1] != string(ScheduledIntegrityCheck) {
		t.Errorf("args[1] (trigger) = %v, want %q", args[1], ScheduledIntegrityCheck)
	}
	if args[2] != true {
		t.Errorf("args[2] (passed) = %v, want true", args[2])
	}
	if args[3] != int64(0) {
		t.Errorf("args[3] (global_sum) = %v, want 0", args[3])
	}
	if args[4] != 0 {
		t.Errorf("args[4] (unbalanced_count) = %v, want 0", args[4])
	}

	details, ok := args[5].(string)
	if !ok {
		t.Fatalf("args[5] (details) = %T, want string", args[5])
	}
	var decoded integrityReportDetails
	if err := json.Unmarshal([]byte(details), &decoded); err != nil {
		t.Fatalf("details is not valid JSON: %v", err)
	}
	if len(decoded.UnbalancedTransactions) != 0 {
		t.Errorf("decoded details has %d unbalanced transactions, want 0", len(decoded.UnbalancedTransactions))
	}
}

func TestRecordIntegrityCheckEncodesUnbalancedTransactionsInDetails(t *testing.T) {
	report := IntegrityReport{
		GlobalSum: 100,
		UnbalancedTransactions: []UnbalancedTransaction{
			{TransactionRef: "txn_bad", Currency: "INR", Sum: 100, AccountCodes: []string{"customer_suspense"}},
		},
	}
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = 1
		return nil
	}}}

	if _, err := RecordIntegrityCheck(context.Background(), db, ManualIntegrityCheck, report); err != nil {
		t.Fatalf("RecordIntegrityCheck returned error: %v", err)
	}

	if db.args[2] != false {
		t.Errorf("args[2] (passed) = %v, want false", db.args[2])
	}
	if db.args[4] != 1 {
		t.Errorf("args[4] (unbalanced_count) = %v, want 1", db.args[4])
	}

	var decoded integrityReportDetails
	if err := json.Unmarshal([]byte(db.args[5].(string)), &decoded); err != nil {
		t.Fatalf("details is not valid JSON: %v", err)
	}
	if len(decoded.UnbalancedTransactions) != 1 || decoded.UnbalancedTransactions[0].TransactionRef != "txn_bad" {
		t.Errorf("decoded details = %+v, want one entry for txn_bad", decoded.UnbalancedTransactions)
	}
}

func TestRecordIntegrityCheckRejectsInvalidTriggerWithoutTouchingDB(t *testing.T) {
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		t.Fatal("QueryRowContext.Scan should not be called for an invalid trigger")
		return nil
	}}}

	if _, err := RecordIntegrityCheck(context.Background(), db, IntegrityCheckTrigger("bogus"), IntegrityReport{}); err == nil {
		t.Error("RecordIntegrityCheck() = nil error, want an error for an invalid trigger")
	}
	if db.called {
		t.Error("RecordIntegrityCheck called QueryRowContext for an invalid trigger")
	}
}

func TestRecordIntegrityCheckPropagatesScanError(t *testing.T) {
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error { return errors.New("boom") }}}

	if _, err := RecordIntegrityCheck(context.Background(), db, ManualIntegrityCheck, IntegrityReport{}); err == nil {
		t.Fatal("RecordIntegrityCheck() = nil error, want the propagated scan error")
	}
}
