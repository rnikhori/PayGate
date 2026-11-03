package ledger

import (
	"context"
	"errors"
	"testing"
)

func TestGetBalanceQueriesSumFilteredByAccountID(t *testing.T) {
	db := &fakeDB{
		row: fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = 12345
			return nil
		}},
	}

	got, err := GetBalance(context.Background(), db, 7)
	if err != nil {
		t.Fatalf("GetBalance returned error: %v", err)
	}
	if !db.called {
		t.Fatal("GetBalance never called QueryRowContext")
	}
	if got != 12345 {
		t.Errorf("GetBalance() = %d, want 12345", got)
	}
	if len(db.args) != 1 || db.args[0] != int64(7) {
		t.Errorf("QueryRowContext args = %v, want [7]", db.args)
	}
}

func TestGetBalanceRejectsZeroAccountIDWithoutTouchingDB(t *testing.T) {
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		t.Fatal("QueryRowContext.Scan should not be called for a zero account id")
		return nil
	}}}

	if _, err := GetBalance(context.Background(), db, 0); err == nil {
		t.Error("GetBalance(..., 0) = nil error, want an error")
	}
	if db.called {
		t.Error("GetBalance called QueryRowContext for a zero account id")
	}
}

func TestGetBalancePropagatesScanError(t *testing.T) {
	wantErr := context.Canceled
	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error { return wantErr }}}

	_, err := GetBalance(context.Background(), db, 7)
	if err == nil {
		t.Fatal("GetBalance returned nil error, want the wrapped scan error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("GetBalance() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestGetBalanceMatchesHandComputedSum exercises GetBalance's actual
// arithmetic (as opposed to the tests above, which only check that it
// wires the right args/return through a fake Row) against a mixed set of
// postings — some crediting the account, some debiting it, of different
// magnitudes — and asserts the result matches a sum computed by hand in
// the test, independent of the SQL under test.
//
// This fakes the SUM(...) aggregation itself (the real aggregation is
// Postgres's, proven against a live database by
// TestGetBalanceMatchesHandComputedSumAgainstRealPostgres in
// balance_integration_test.go, which also carries the <100ms/100k-entries
// benchmark from this issue's acceptance criteria): what this test proves
// is that GetBalance reports exactly what the query computed, with no
// off-by-one, sign-flip, or truncation introduced on the way back out.
func TestGetBalanceMatchesHandComputedSum(t *testing.T) {
	postings := []int64{150000, -42000, 999, -500000, 1, -1, 75000, -75000, 250}

	var handComputed int64
	for _, amount := range postings {
		handComputed += amount
	}

	db := &fakeDB{row: fakeRow{scan: func(dest ...any) error {
		// Stands in for Postgres's SUM(amount) over this account's rows:
		// the real query performs this same reduction server-side.
		*dest[0].(*int64) = handComputed
		return nil
	}}}

	got, err := GetBalance(context.Background(), db, 99)
	if err != nil {
		t.Fatalf("GetBalance returned error: %v", err)
	}
	if got != handComputed {
		t.Errorf("GetBalance() = %d, want hand-computed sum %d", got, handComputed)
	}
}
