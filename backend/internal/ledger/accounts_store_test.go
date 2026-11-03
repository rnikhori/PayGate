package ledger

import (
	"context"
	"testing"
	"time"
)

// fakeRow is a Row whose Scan is supplied by the test, so tests can exercise
// EnsureMerchantPayableAccount's SQL-adjacent logic without a live database.
type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// fakeDB is a DB that records the query/args it was called with and hands
// back a fixed Row.
type fakeDB struct {
	called bool
	query  string
	args   []any
	row    Row
}

func (f *fakeDB) QueryRowContext(_ context.Context, query string, args ...any) Row {
	f.called = true
	f.query = query
	f.args = args
	return f.row
}

func TestEnsureMerchantPayableAccountBuildsExpectedCodeAndType(t *testing.T) {
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	db := &fakeDB{
		row: fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = 42
			*dest[1].(*string) = "merchant_payable:mch_1"
			*dest[2].(*string) = string(LiabilityAccount)
			*dest[3].(*string) = "Merchant payable: mch_1"
			*dest[4].(*time.Time) = createdAt
			return nil
		}},
	}

	got, err := EnsureMerchantPayableAccount(context.Background(), db, "mch_1")
	if err != nil {
		t.Fatalf("EnsureMerchantPayableAccount returned error: %v", err)
	}

	if !db.called {
		t.Fatal("EnsureMerchantPayableAccount never called QueryRowContext")
	}

	wantCode := "merchant_payable:mch_1"
	if len(db.args) < 2 || db.args[0] != wantCode {
		t.Errorf("QueryRowContext args[0] = %v, want %q", db.args, wantCode)
	}
	if len(db.args) < 2 || db.args[1] != string(LiabilityAccount) {
		t.Errorf("QueryRowContext args[1] = %v, want %q (merchant payable accounts are liabilities)", db.args, LiabilityAccount)
	}

	want := Account{
		ID:        42,
		Code:      wantCode,
		Type:      LiabilityAccount,
		Name:      "Merchant payable: mch_1",
		CreatedAt: createdAt,
	}
	if got != want {
		t.Errorf("EnsureMerchantPayableAccount() = %+v, want %+v", got, want)
	}
}

func TestEnsureMerchantPayableAccountRejectsInvalidMerchantIDWithoutTouchingDB(t *testing.T) {
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		t.Fatal("QueryRowContext.Scan should not be called for an invalid merchant id")
		return nil
	}}}

	if _, err := EnsureMerchantPayableAccount(context.Background(), db, ""); err == nil {
		t.Error("EnsureMerchantPayableAccount(..., \"\") = nil error, want an error")
	}
	if db.called {
		t.Error("EnsureMerchantPayableAccount called QueryRowContext for an invalid merchant id")
	}
}

func TestEnsureMerchantPayableAccountPropagatesScanError(t *testing.T) {
	wantErr := context.Canceled
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error { return wantErr }}}

	_, err := EnsureMerchantPayableAccount(context.Background(), db, "mch_1")
	if err == nil {
		t.Fatal("EnsureMerchantPayableAccount returned nil error, want the wrapped scan error")
	}
}
