//go:build integration

// See balance_integration_test.go's package comment for how to run these
// (they need a live, already-migrated Postgres via TEST_DATABASE_URL or
// DATABASE_URL, and are opt-in — not part of `go test ./...` / `make
// test-backend`):
//
//	go test -tags=integration ./internal/ledger/... -run TestGetAccountStatement -v
package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// mustEnsureMerchantAccount creates (or reuses) a merchant_payable account
// for a fresh, unique merchant id, so concurrent/repeated test runs never
// collide, and returns both.
func mustEnsureMerchantAccount(t *testing.T, conn Conn, label string) (merchantID string, account Account) {
	t.Helper()

	merchantID = fmt.Sprintf("mch_stmt_%s_%d", label, time.Now().UnixNano())
	account, err := EnsureMerchantPayableAccount(context.Background(), conn, merchantID)
	if err != nil {
		t.Fatalf("EnsureMerchantPayableAccount(%q): %v", merchantID, err)
	}
	return merchantID, account
}

// TestGetAccountStatementPaginatesInTimeOrderWithCorrectRunningBalance
// posts a sequence of real, balanced transactions against a merchant's
// payable account (via PostJournalEntry, the same write path every other
// package uses), then pages through GetAccountStatement one entry at a
// time and asserts: entries come back in time order, every running balance
// matches a running sum this test computes by hand, and reassembling every
// page's entries reconstructs the exact sequence that was posted.
func TestGetAccountStatementPaginatesInTimeOrderWithCorrectRunningBalance(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	stmtDB := WrapStatementDB(db)
	ctx := context.Background()

	merchantID, account := mustEnsureMerchantAccount(t, conn, "paging")
	counterparty := mustCreateTestAccount(t, db, "stmt_paging_counterparty")

	amounts := []int64{50000, -12000, 2500, -500, 100000, -30000}
	var handBalance int64
	for i, amount := range amounts {
		handBalance += amount
		ref := fmt.Sprintf("txn_stmt_paging_%d_%d", time.Now().UnixNano(), i)
		entries := []Entry{
			{AccountID: account.ID, Amount: amount, Currency: "INR"},
			{AccountID: counterparty, Amount: -amount, Currency: "INR"},
		}
		if err := PostJournalEntry(ctx, conn, ref, entries); err != nil {
			t.Fatalf("PostJournalEntry(%d): %v", i, err)
		}
		// journal_entries.created_at defaults to now(); space posts out so
		// (created_at, id) ordering is never ambiguous between them even
		// at low clock resolution.
		time.Sleep(5 * time.Millisecond)
	}

	var (
		all    []StatementEntry
		cursor string
	)
	for page := 0; ; page++ {
		if page > len(amounts)+2 {
			t.Fatal("GetAccountStatement pagination did not terminate")
		}
		got, err := GetAccountStatement(ctx, stmtDB, merchantID, StatementQuery{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("GetAccountStatement(page %d): %v", page, err)
		}
		if len(got.Entries) != 1 {
			t.Fatalf("page %d: len(Entries) = %d, want 1", page, len(got.Entries))
		}
		all = append(all, got.Entries[0])
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}

	if len(all) != len(amounts) {
		t.Fatalf("reassembled %d entries across pages, want %d", len(all), len(amounts))
	}

	var running int64
	for i, se := range all {
		running += amounts[i]
		if se.Amount != amounts[i] {
			t.Errorf("entry %d amount = %d, want %d (pages out of time order?)", i, se.Amount, amounts[i])
		}
		if se.RunningBalance != running {
			t.Errorf("entry %d running balance = %d, want %d", i, se.RunningBalance, running)
		}
		if se.AccountID != account.ID {
			t.Errorf("entry %d account_id = %d, want %d", i, se.AccountID, account.ID)
		}
	}
	if running != handBalance {
		t.Fatalf("final running balance = %d, want hand-computed %d", running, handBalance)
	}

	gotBalance, err := GetBalance(ctx, conn, account.ID)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if gotBalance != handBalance {
		t.Fatalf("GetBalance() = %d, want %d (same as the statement's final running balance)", gotBalance, handBalance)
	}
}

// TestGetAccountStatementFiltersByDateRangeWithoutResettingRunningBalance
// posts entries spanning an artificially backdated range (by inserting
// directly with an explicit created_at, since PostJournalEntry always uses
// now()) and asserts a date-range query returns only the in-range entries,
// while each one's RunningBalance still reflects the account's true
// balance across its *entire* history — not reset to 0 as of the range's
// start — the same way a real bank statement's running balance does.
func TestGetAccountStatementFiltersByDateRangeWithoutResettingRunningBalance(t *testing.T) {
	db := testDB(t)
	conn := Wrap(db)
	stmtDB := WrapStatementDB(db)
	ctx := context.Background()

	merchantID, account := mustEnsureMerchantAccount(t, conn, "daterange")
	counterparty := mustCreateTestAccount(t, db, "stmt_daterange_counterparty")

	base := time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)
	type seeded struct {
		amount    int64
		createdAt time.Time
	}
	rows := []seeded{
		{amount: 1000, createdAt: base},                      // before the window
		{amount: 2000, createdAt: base.Add(24 * time.Hour)},  // in window
		{amount: 3000, createdAt: base.Add(48 * time.Hour)},  // in window
		{amount: -500, createdAt: base.Add(240 * time.Hour)}, // after the window
	}

	var trueBalance int64
	for i, r := range rows {
		trueBalance += r.amount
		ref := fmt.Sprintf("txn_stmt_daterange_%d_%d", time.Now().UnixNano(), i)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO journal_entries (transaction_ref, account_id, amount, currency, created_at) VALUES
			 ($1, $2, $3, 'INR', $5), ($1, $4, $6, 'INR', $5)`,
			ref, account.ID, r.amount, counterparty, r.createdAt, -r.amount,
		)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed row %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed row %d: %v", i, err)
		}
	}

	from := base.Add(1 * time.Hour)
	to := base.Add(72 * time.Hour)
	page, err := GetAccountStatement(ctx, stmtDB, merchantID, StatementQuery{From: from, To: to, Limit: 10})
	if err != nil {
		t.Fatalf("GetAccountStatement: %v", err)
	}

	if len(page.Entries) != 2 {
		t.Fatalf("len(page.Entries) = %d, want 2 (only the 2 in-range postings)", len(page.Entries))
	}
	if page.Entries[0].Amount != 2000 || page.Entries[1].Amount != 3000 {
		t.Errorf("in-range amounts = [%d, %d], want [2000, 3000]", page.Entries[0].Amount, page.Entries[1].Amount)
	}
	// True running balance after the 2nd in-window entry is 1000+2000+3000,
	// not 2000+3000 — it must not be reset just because the 1st posting
	// fell outside the requested window.
	wantSecondRunning := rows[0].amount + rows[1].amount + rows[2].amount
	if page.Entries[1].RunningBalance != wantSecondRunning {
		t.Errorf("2nd in-range entry's running balance = %d, want %d (true balance, unaffected by the date filter)", page.Entries[1].RunningBalance, wantSecondRunning)
	}
}

// TestGetAccountStatementRejectsAnotherMerchantsAccountCode proves
// GetAccountStatement's merchant-scoping isn't just "well, nothing calls it
// with someone else's account" by construction: there is no way to make it
// read a different merchant_payable account at all, because it never
// accepts an account id or code — only a merchantID it resolves itself.
// This test asserts the negative space of that API: querying a merchantID
// for which no account has ever been created errors rather than, say,
// falling back to some other account.
func TestGetAccountStatementRejectsAnotherMerchantsAccountCode(t *testing.T) {
	db := testDB(t)
	stmtDB := WrapStatementDB(db)
	ctx := context.Background()

	neverCreatedMerchantID := fmt.Sprintf("mch_stmt_never_created_%d", time.Now().UnixNano())
	if _, err := GetAccountStatement(ctx, stmtDB, neverCreatedMerchantID, StatementQuery{}); err == nil {
		t.Fatal("GetAccountStatement() = nil error, want an error for a merchant with no ledger account")
	}
}
