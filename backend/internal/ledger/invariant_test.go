package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This file is the issue-20 test suite: a focused set of tests exercising
// every posting path this package exposes (capture, refund, settlement
// clearing) together, against a single shared ledger, asserting the
// fundamental invariant the whole package exists to uphold -- the sum of
// every journal_entries row ever written is always zero -- and that an
// invalid posting is rejected without writing anything.
//
// Every other *_test.go file in this package checks one function's own
// output sums to zero in isolation (e.g. TestBuildCaptureEntriesProducesThreeBalancedLegs)
// or drives one posting through a narrow fake that only records
// ExecContext calls (e.g. captureTestConn). Neither accumulates real state
// across many postings, many merchants, and all three posting paths at
// once the way a long-running ledger actually does, which is what the
// property test below needs: an in-memory fake that actually behaves like
// journal_entries, ledger_accounts, and journal_entry_postings, not just
// one that records what was asked of it.

// memLedger is that in-memory fake: the full state a running ledger has --
// every account ever created, every entry ever posted, and every
// transaction_ref ever claimed (see claimTransactionRef in post.go) --
// shared by a memConn and every memTx it begins, so posting through
// PostCapture/PostRefund/PostSettlementClearing against it behaves exactly
// as it would against a real, migrated Postgres database as far as this
// package's own SQL ever observes.
type memLedger struct {
	accounts      []Account
	entries       []Entry
	postings      map[string]string // transaction_ref -> fingerprint
	nextAccountID int64
	nextEntryID   int64
}

func newMemLedger() *memLedger {
	return &memLedger{postings: map[string]string{}}
}

// newSeededMemLedger returns a memLedger with the 3 fixed system accounts
// already seeded -- exactly as backend/migrations/*_create_ledger_accounts
// seeds a real database -- so ResolveCaptureAccounts and
// ResolveSettlementClearingAccounts can find them by code without this
// test fixture needing to special-case "account doesn't exist yet" for
// them the way it legitimately does for a per-merchant payable account.
func newSeededMemLedger() *memLedger {
	m := newMemLedger()
	m.ensureAccount(CustomerSuspenseAccountCode, LiabilityAccount, "Customer suspense")
	m.ensureAccount(GatewayFeeRevenueAccountCode, RevenueAccount, "Gateway fee revenue")
	m.ensureAccount(SettlementClearingAccountCode, AssetAccount, "Settlement clearing")
	return m
}

// memSnapshot is a point-in-time copy of every slice/map memLedger owns,
// used by memTx.Rollback to undo whatever a failed posting wrote -- the
// in-memory equivalent of a real database transaction's ROLLBACK, needed
// so TestLedgerRejectsInvalidPostingWithoutSideEffects can assert a
// rejected posting truly left nothing behind.
type memSnapshot struct {
	accounts      []Account
	entries       []Entry
	postings      map[string]string
	nextAccountID int64
	nextEntryID   int64
}

func (m *memLedger) snapshot() memSnapshot {
	postings := make(map[string]string, len(m.postings))
	for k, v := range m.postings {
		postings[k] = v
	}
	return memSnapshot{
		accounts:      append([]Account(nil), m.accounts...),
		entries:       append([]Entry(nil), m.entries...),
		postings:      postings,
		nextAccountID: m.nextAccountID,
		nextEntryID:   m.nextEntryID,
	}
}

func (m *memLedger) restore(s memSnapshot) {
	m.accounts = s.accounts
	m.entries = s.entries
	m.postings = s.postings
	m.nextAccountID = s.nextAccountID
	m.nextEntryID = s.nextEntryID
}

// globalSum sums journal_entries.amount across every row this memLedger
// has ever recorded, over every account and every transaction_ref -- the
// quantity the global-zero invariant is about. A real ledger never
// computes this (GetBalance, by design, always scopes to one account --
// see balance.go), but a test asserting the invariant itself needs exactly
// this unscoped sum.
func (m *memLedger) globalSum() int64 {
	var sum int64
	for _, e := range m.entries {
		sum += e.Amount
	}
	return sum
}

func (m *memLedger) findAccountByCode(code string) (Account, bool) {
	for _, a := range m.accounts {
		if a.Code == code {
			return a, true
		}
	}
	return Account{}, false
}

// ensureAccount mirrors EnsureMerchantPayableAccount's upsert: returns the
// existing row for code if one exists, otherwise creates and returns a new
// one.
func (m *memLedger) ensureAccount(code string, accType AccountType, name string) Account {
	if a, ok := m.findAccountByCode(code); ok {
		return a
	}
	m.nextAccountID++
	a := Account{ID: m.nextAccountID, Code: code, Type: accType, Name: name, CreatedAt: time.Now()}
	m.accounts = append(m.accounts, a)
	return a
}

func (m *memLedger) sumByRef(accountID int64, ref string) int64 {
	var sum int64
	for _, e := range m.entries {
		if e.AccountID == accountID && e.TransactionRef == ref {
			sum += e.Amount
		}
	}
	return sum
}

func (m *memLedger) sumByLike(accountID int64, pattern string) int64 {
	var sum int64
	for _, e := range m.entries {
		if e.AccountID == accountID && likeMatch(pattern, likeEscapeChar, e.TransactionRef) {
			sum += e.Amount
		}
	}
	return sum
}

func (m *memLedger) sumCapturedBefore(accountID int64, cutoff time.Time) int64 {
	var sum int64
	for _, e := range m.entries {
		if e.AccountID == accountID && e.Amount > 0 && e.CreatedAt.Before(cutoff) {
			sum += e.Amount
		}
	}
	return sum
}

// likeMatch reports whether s matches the SQL "... LIKE pattern ESCAPE
// escape" semantics refundsLikePattern/settlementsLikePattern (refund.go,
// settlement.go) rely on: '%' matches any run of characters, '_' matches
// any one character, and escape immediately before either makes it
// literal. It is only ever asked to match the fixed-shape patterns this
// package itself builds, so this general translation to an anchored regexp
// is more rigor than those patterns strictly need, but it costs nothing to
// get right rather than special-cased.
func likeMatch(pattern, escape, s string) bool {
	var sb strings.Builder
	sb.WriteString("^")
	esc := escape[0]
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == esc && i+1 < len(pattern):
			sb.WriteString(regexp.QuoteMeta(string(pattern[i+1])))
			i++
		case c == '%':
			sb.WriteString(".*")
		case c == '_':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.MustCompile(sb.String()).MatchString(s)
}

// memConn adapts a memLedger to Conn (DB + Beginner), so PostCapture,
// PostRefund, and PostSettlementClearing -- every self-contained posting
// path this package exposes -- can run against it exactly as they would
// against Wrap(realDB).
type memConn struct{ store *memLedger }

func (c *memConn) QueryRowContext(_ context.Context, query string, args ...any) Row {
	return memQueryRow(c.store, query, args...)
}

func (c *memConn) BeginTx(context.Context, *sql.TxOptions) (Tx, error) {
	return &memTx{store: c.store, snapshot: c.store.snapshot()}, nil
}

// memTx adapts a memLedger to Tx. Every write happens immediately against
// the shared store (there is only ever one goroutine driving these tests,
// so no real isolation is needed), and Rollback undoes them by restoring
// the snapshot taken when this memTx began -- the fake's equivalent of a
// real ROLLBACK discarding an uncommitted transaction's writes.
type memTx struct {
	store    *memLedger
	snapshot memSnapshot
	resolved bool
}

func (t *memTx) QueryRowContext(_ context.Context, query string, args ...any) Row {
	return memQueryRow(t.store, query, args...)
}

func (t *memTx) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	if !strings.Contains(query, "INSERT INTO journal_entries") {
		panic(fmt.Sprintf("memTx.ExecContext: unrecognized query: %s", query))
	}
	ref, _ := args[0].(string)
	accountID, _ := args[1].(int64)
	amount, _ := args[2].(int64)
	currency, _ := args[3].(string)

	t.store.nextEntryID++
	t.store.entries = append(t.store.entries, Entry{
		ID:             t.store.nextEntryID,
		TransactionRef: ref,
		AccountID:      accountID,
		Amount:         amount,
		Currency:       currency,
		CreatedAt:      time.Now(),
	})
	return nil, nil
}

func (t *memTx) Commit() error {
	t.resolved = true
	return nil
}

func (t *memTx) Rollback() error {
	if !t.resolved {
		t.store.restore(t.snapshot)
	}
	t.resolved = true
	return nil
}

// memQueryRow dispatches every SELECT/INSERT...RETURNING this package ever
// issues via QueryRowContext (accounts_store.go, post.go, refund.go,
// settlement.go) against store, matched by a distinguishing substring of
// the query text -- the same literal queries defined as named consts
// elsewhere in this package, never reimplemented here, only interpreted.
func memQueryRow(store *memLedger, query string, args ...any) Row {
	switch {
	case strings.Contains(query, "INSERT INTO ledger_accounts"):
		code, _ := args[0].(string)
		accType, _ := args[1].(string)
		name, _ := args[2].(string)
		a := store.ensureAccount(code, AccountType(accType), name)
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = a.ID
			*dest[1].(*string) = a.Code
			*dest[2].(*string) = string(a.Type)
			*dest[3].(*string) = a.Name
			*dest[4].(*time.Time) = a.CreatedAt
			return nil
		}}

	case strings.Contains(query, "FROM ledger_accounts WHERE code"):
		code, _ := args[0].(string)
		a, ok := store.findAccountByCode(code)
		if !ok {
			return fakeRow{scan: func(...any) error { return sql.ErrNoRows }}
		}
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = a.ID
			*dest[1].(*string) = a.Code
			*dest[2].(*string) = string(a.Type)
			*dest[3].(*string) = a.Name
			*dest[4].(*time.Time) = a.CreatedAt
			return nil
		}}

	case strings.Contains(query, "INSERT INTO journal_entry_postings"):
		ref, _ := args[0].(string)
		fingerprint, _ := args[1].(string)
		if _, claimed := store.postings[ref]; claimed {
			return fakeRow{scan: func(...any) error { return sql.ErrNoRows }}
		}
		store.postings[ref] = fingerprint
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*string) = fingerprint
			return nil
		}}

	case strings.Contains(query, "SELECT fingerprint FROM journal_entry_postings"):
		ref, _ := args[0].(string)
		fp, ok := store.postings[ref]
		if !ok {
			return fakeRow{scan: func(...any) error { return sql.ErrNoRows }}
		}
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*string) = fp
			return nil
		}}

	case strings.Contains(query, "amount > 0 AND created_at <"):
		accountID, _ := args[0].(int64)
		cutoff, _ := args[1].(time.Time)
		sum := store.sumCapturedBefore(accountID, cutoff)
		return fakeRow{scan: func(dest ...any) error { *dest[0].(*int64) = sum; return nil }}

	case strings.Contains(query, "LIKE $2"):
		accountID, _ := args[0].(int64)
		pattern, _ := args[1].(string)
		sum := store.sumByLike(accountID, pattern)
		return fakeRow{scan: func(dest ...any) error { *dest[0].(*int64) = sum; return nil }}

	case strings.Contains(query, "transaction_ref = $2"):
		accountID, _ := args[0].(int64)
		ref, _ := args[1].(string)
		sum := store.sumByRef(accountID, ref)
		return fakeRow{scan: func(dest ...any) error { *dest[0].(*int64) = sum; return nil }}

	default:
		panic(fmt.Sprintf("memQueryRow: unrecognized query: %s", query))
	}
}

// TestLedgerGlobalZeroInvariantAcrossRandomPostings is this issue's
// property-based/table test: it drives a long, deterministically
// randomized sequence of valid postings -- captures, refunds of earlier
// captures, and settlement clearings, spread across several merchants --
// through PostCapture/PostRefund/PostSettlementClearing against one shared
// in-memory ledger, and after every single posting asserts the invariant
// this whole package exists to uphold: the sum of every journal_entries
// row ever written, across the entire ledger, is zero.
func TestLedgerGlobalZeroInvariantAcrossRandomPostings(t *testing.T) {
	const (
		seed          = 20260101 // fixed: a failure must reproduce deterministically
		iterations    = 500
		merchantCount = 6
	)
	rng := rand.New(rand.NewSource(seed))

	m := newSeededMemLedger()
	conn := &memConn{store: m}
	ctx := context.Background()

	merchants := make([]string, merchantCount)
	for i := range merchants {
		merchants[i] = fmt.Sprintf("mch_prop_%d", i)
	}

	// captures tracks, per merchant, every capture transaction_ref this
	// run has posted so far -- what a refund iteration picks from.
	captures := map[string][]string{}
	var batchCounter int

	for i := 0; i < iterations; i++ {
		merchant := merchants[rng.Intn(len(merchants))]
		op := rng.Intn(3)

		switch {
		case op == 0 || len(captures[merchant]) == 0:
			// Capture: gross amount large enough that a 1-500bps fee
			// never truncates to 0 (BuildCaptureEntries rejects that --
			// this loop posts only valid sequences).
			gross := int64(1000 + rng.Intn(500000))
			bps := int64(1 + rng.Intn(500))
			ref := fmt.Sprintf("txn_prop_%d", i)
			if err := PostCapture(ctx, conn, ref, merchant, gross, "INR", FeeSchedule{BasisPoints: bps}); err != nil {
				t.Fatalf("iteration %d: PostCapture(%s, gross=%d, bps=%d) = %v, want nil", i, merchant, gross, bps, err)
			}
			captures[merchant] = append(captures[merchant], ref)

		case op == 1:
			// Refund a randomly chosen prior capture for this merchant,
			// for a random amount within what remains available.
			refs := captures[merchant]
			captureRef := refs[rng.Intn(len(refs))]
			rc, err := ResolveRefundContext(ctx, conn, merchant, captureRef)
			if err != nil {
				t.Fatalf("iteration %d: ResolveRefundContext(%s, %s) = %v, want nil", i, merchant, captureRef, err)
			}
			available := rc.GrossAmount - rc.AlreadyRefunded
			if available <= 0 {
				continue // this capture is already fully refunded
			}
			amount := int64(1 + rng.Int63n(available))
			refundRef := fmt.Sprintf("r%d", i)
			if err := PostRefund(ctx, conn, captureRef, refundRef, merchant, amount, "INR"); err != nil {
				t.Fatalf("iteration %d: PostRefund(%s, %s, amount=%d) = %v, want nil", i, captureRef, refundRef, amount, err)
			}

		default:
			// Settle whatever is newly eligible for this merchant.
			// PostSettlementClearing is itself a no-op (posts nothing)
			// when nothing is newly eligible, so this is always a valid
			// call even when it ends up writing zero rows.
			batchCounter++
			batchID := fmt.Sprintf("batch_prop_%d", batchCounter)
			if _, err := PostSettlementClearing(ctx, conn, batchID, merchant, time.Now().Add(time.Second), "INR"); err != nil {
				t.Fatalf("iteration %d: PostSettlementClearing(%s, %s) = %v, want nil", i, batchID, merchant, err)
			}
		}

		if sum := m.globalSum(); sum != 0 {
			t.Fatalf("iteration %d: global sum of all journal entries = %d, want 0 (double-entry invariant violated)", i, sum)
		}
	}

	if len(m.entries) == 0 {
		t.Fatal("test posted no journal entries at all; the invariant above would have held vacuously")
	}
}

// TestLedgerRejectsInvalidPostingWithoutSideEffects is this issue's second
// acceptance criterion: an invalid posting -- entries that don't sum to
// zero, or a refund that would exceed what its capture ever credited --
// must be rejected and must leave the ledger exactly as it was: no new
// journal_entries row, no new ledger_accounts row, and the global sum
// still what it was before the rejected call.
func TestLedgerRejectsInvalidPostingWithoutSideEffects(t *testing.T) {
	m := newSeededMemLedger()
	conn := &memConn{store: m}
	ctx := context.Background()

	// A first, valid capture, so there is real state an invalid posting
	// could disturb.
	if err := PostCapture(ctx, conn, "txn_invalid_setup", "mch_invalid", 100000, "INR", FeeSchedule{BasisPoints: 250}); err != nil {
		t.Fatalf("setup PostCapture() = %v, want nil", err)
	}
	if sum := m.globalSum(); sum != 0 {
		t.Fatalf("test fixture itself left a non-zero global sum (%d) after setup", sum)
	}

	entriesBefore := len(m.entries)
	accountsBefore := len(m.accounts)

	// An unbalanced direct posting: two legs that don't sum to zero.
	badEntries := []Entry{
		{AccountID: 1, Amount: 10000, Currency: "INR"},
		{AccountID: 2, Amount: -9000, Currency: "INR"},
	}
	if err := PostJournalEntry(ctx, conn, "txn_invalid_unbalanced", badEntries); err == nil {
		t.Fatal("PostJournalEntry() with unbalanced entries = nil error, want an error")
	} else if !errors.Is(err, ErrUnbalancedTransaction) {
		t.Errorf("PostJournalEntry() error = %v, want it to wrap ErrUnbalancedTransaction", err)
	}

	// A refund that over-refunds the capture above.
	rc, err := ResolveRefundContext(ctx, conn, "mch_invalid", "txn_invalid_setup")
	if err != nil {
		t.Fatalf("ResolveRefundContext() = %v, want nil", err)
	}
	if err := PostRefund(ctx, conn, "txn_invalid_setup", "over_refund", "mch_invalid", rc.GrossAmount+1, "INR"); err == nil {
		t.Fatal("PostRefund() over-refunding = nil error, want an error")
	} else if !errors.Is(err, ErrRefundExceedsAvailableBalance) {
		t.Errorf("PostRefund() error = %v, want it to wrap ErrRefundExceedsAvailableBalance", err)
	}

	if got := len(m.entries); got != entriesBefore {
		t.Errorf("journal_entries row count = %d after rejected postings, want unchanged %d", got, entriesBefore)
	}
	if got := len(m.accounts); got != accountsBefore {
		t.Errorf("ledger_accounts row count = %d after rejected postings, want unchanged %d", got, accountsBefore)
	}
	if got := m.globalSum(); got != 0 {
		t.Errorf("global sum = %d after rejected postings, want still 0", got)
	}
}
