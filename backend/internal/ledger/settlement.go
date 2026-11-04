package ledger

import (
	"context"
	"fmt"
	"time"
)

// settlementTransactionRefPrefix namespaces every settlement clearing
// posting's transaction_ref, the same way refundTransactionRefPrefix
// namespaces a refund's.
const settlementTransactionRefPrefix = "settlement"

// SettlementTransactionRef builds the transaction_ref a settlement batch's
// clearing entries for one merchant are posted under:
// "settlement:<batchID>:<merchantID>".
//
// This is what makes "one transaction_ref per merchant per settlement
// batch" (this issue's first acceptance criterion) hold: every merchant
// swept into batchID gets its own transaction_ref, scoped to that batch,
// rather than one giant entry mixing every merchant's money together. It
// is also the key PostSettlementClearing's idempotency check (below) reads
// back by, so calling this twice for the same batchID and merchantID must
// always return the same string — it is a pure function of its inputs.
func SettlementTransactionRef(batchID, merchantID string) (string, error) {
	if batchID == "" {
		return "", fmt.Errorf("ledger: settlement batch id must not be empty")
	}
	if merchantID == "" {
		return "", fmt.Errorf("ledger: merchant id must not be empty")
	}
	return fmt.Sprintf("%s:%s:%s", settlementTransactionRefPrefix, batchID, merchantID), nil
}

// settlementsLikePattern returns the LIKE pattern (with its ESCAPE
// character, likeEscapeChar) matching every transaction_ref
// SettlementTransactionRef could have built for merchantID, for any
// batchID — i.e. every settlement clearing entry ever posted against that
// one merchant's payable account, across every batch that has ever run.
// ResolveSettlementAmount sums these (negated) to find how much of a
// merchant's captured total has already been cleared.
func settlementsLikePattern(merchantID string) string {
	return settlementTransactionRefPrefix + ":%:" + likeEscaper.Replace(merchantID)
}

// SettlementClearingAccounts holds the two ledger accounts a settlement
// batch's clearing posting moves money between for one merchant, resolved
// once up front so both PostSettlementClearing and a future caller posting
// inside its own transaction build their entries the same way — mirroring
// CaptureAccounts and RefundContext.Accounts.
type SettlementClearingAccounts struct {
	MerchantPayable    Account
	SettlementClearing Account
}

// ResolveSettlementClearingAccounts looks up the fixed settlement_clearing
// account and ensures merchantID's merchant_payable:* account exists,
// returning both together. It only reads (and, for the merchant's account,
// idempotently upserts) — it never begins a transaction — so it is safe to
// call with either a bare *sql.DB (wrapped via Wrap) or a Tx already open
// for something else.
func ResolveSettlementClearingAccounts(ctx context.Context, db DB, merchantID string) (SettlementClearingAccounts, error) {
	clearing, err := GetAccountByCode(ctx, db, SettlementClearingAccountCode)
	if err != nil {
		return SettlementClearingAccounts{}, fmt.Errorf("ledger: resolve settlement clearing accounts: %w", err)
	}

	payable, err := EnsureMerchantPayableAccount(ctx, db, merchantID)
	if err != nil {
		return SettlementClearingAccounts{}, fmt.Errorf("ledger: resolve settlement clearing accounts: %w", err)
	}

	return SettlementClearingAccounts{
		MerchantPayable:    payable,
		SettlementClearing: clearing,
	}, nil
}

// ResolveSettlementAmount computes how much of merchantID's captured
// balance is eligible to clear into a settlement batch as of cutoff: every
// payment captured strictly before cutoff, minus whatever has already been
// cleared by a previous settlement batch for this merchant.
//
// No Payment schema exists yet in this codebase (see capture.go's and
// refund.go's doc comments for the same constraint), so there is no
// Payment.status column this package can filter on directly. What it can
// read, directly from journal_entries, is exactly as reliable today: every
// credit (positive amount) ever posted to merchant_payable:<merchantID> is
// a capture's net leg (BuildCaptureEntries) — nothing else in this package
// posts a positive entry there, a refund's reversal is always a debit
// (BuildRefundEntries) — so "captured payments older than cutoff" is
// SUM(amount) over that account's credit rows with created_at strictly
// before cutoff. A Payments package landing with its own captured_at would
// let a caller pass a precise, independently-sourced capture set instead of
// this package inferring it from the ledger; until then, this is the
// honest subset this issue's acceptance criteria can be met with.
//
// "Already cleared" is read back the same way RefundContext.AlreadyRefunded
// is: summing every settlement clearing entry ever posted against this
// merchant (via settlementsLikePattern, across every batchID), not just the
// current batch — so a merchant settled in yesterday's batch is not
// swept again today, and a captured-but-not-yet-settled payment waits,
// uncleared, for whichever future batch's cutoff first reaches past it.
// This assumes settlement batches run with non-decreasing cutoffs over
// time, which is how a periodic settlement job is expected to be driven.
//
// The result can be zero (nothing new to settle) but is never negative:
// callers must not post a non-positive amount (see
// BuildSettlementClearingEntries), and PostSettlementClearing checks this
// before attempting to.
func ResolveSettlementAmount(ctx context.Context, db DB, merchantPayableAccountID int64, merchantID string, cutoff time.Time) (int64, error) {
	const capturedBeforeCutoffQuery = `
		SELECT COALESCE(SUM(amount), 0) FROM journal_entries
		WHERE account_id = $1 AND amount > 0 AND created_at < $2`

	var totalCaptured int64
	row := db.QueryRowContext(ctx, capturedBeforeCutoffQuery, merchantPayableAccountID, cutoff)
	if err := row.Scan(&totalCaptured); err != nil {
		return 0, fmt.Errorf("ledger: resolve settlement amount for %q: sum captured payments: %w", merchantID, err)
	}

	// Settlement clearing entries debit merchant_payable (a negative
	// amount, see BuildSettlementClearingEntries); negate the sum back to
	// a positive "already cleared" total.
	clearedLeg, err := sumEntries(ctx, db, merchantPayableAccountID, "", settlementsLikePattern(merchantID))
	if err != nil {
		return 0, fmt.Errorf("ledger: resolve settlement amount for %q: sum already-cleared total: %w", merchantID, err)
	}
	alreadyCleared := -clearedLeg

	available := totalCaptured - alreadyCleared
	if available < 0 {
		return 0, nil
	}
	return available, nil
}

// BuildSettlementClearingEntries builds — but does not post — the 2
// balanced entries that move a settlement batch's cleared amount out of a
// merchant's payable account into settlement_clearing: a debit to
// merchant_payable (that amount is no longer available to the merchant —
// it is now in transit to their bank) and a credit to settlement_clearing
// for the same amount.
//
// amount must be positive: a settlement batch with nothing newly eligible
// to clear for this merchant must not post a zero-amount transaction (see
// ValidateBalancedTransaction) — PostSettlementClearing checks this and
// skips posting entirely rather than calling this with a non-positive
// amount.
func BuildSettlementClearingEntries(accounts SettlementClearingAccounts, amount int64, currency string) ([]Entry, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("ledger: settlement clearing amount must be positive, got %d", amount)
	}
	if err := ValidateCurrencyCode(currency); err != nil {
		return nil, err
	}

	return []Entry{
		{AccountID: accounts.MerchantPayable.ID, Amount: -amount, Currency: currency},
		{AccountID: accounts.SettlementClearing.ID, Amount: amount, Currency: currency},
	}, nil
}

// PostSettlementClearing resolves merchantID's settlement accounts, finds
// how much of its captured balance is eligible to clear as of cutoff (via
// ResolveSettlementAmount), and posts it as this batch's one
// transaction_ref for this merchant (via SettlementTransactionRef) —
// unless that transaction_ref already has entries, in which case it is a
// no-op: it returns the amount that earlier call posted, read back from
// journal_entries, without posting anything again.
//
// That check is what makes a settlement job idempotent when retried after
// a partial failure (this issue's third acceptance criterion): a batch
// that clears merchants A and B, then fails before C, can be retried in
// full — A and B's already-posted transaction_refs are detected and
// skipped, and only C, which never got a transaction_ref, is posted.
//
// The returned int64 is the amount actually cleared for merchantID by this
// call (0 if there was nothing newly eligible to settle and nothing was
// posted).
func PostSettlementClearing(ctx context.Context, db Conn, batchID, merchantID string, cutoff time.Time, currency string) (int64, error) {
	ref, err := SettlementTransactionRef(batchID, merchantID)
	if err != nil {
		return 0, err
	}

	accounts, err := ResolveSettlementClearingAccounts(ctx, db, merchantID)
	if err != nil {
		return 0, err
	}

	alreadyPosted, err := sumEntries(ctx, db, accounts.SettlementClearing.ID, ref, "")
	if err != nil {
		return 0, fmt.Errorf("ledger: post settlement clearing: check idempotency for %q: %w", ref, err)
	}
	if alreadyPosted != 0 {
		return alreadyPosted, nil
	}

	amount, err := ResolveSettlementAmount(ctx, db, accounts.MerchantPayable.ID, merchantID, cutoff)
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, nil
	}

	entries, err := BuildSettlementClearingEntries(accounts, amount, currency)
	if err != nil {
		return 0, err
	}

	if err := PostJournalEntry(ctx, db, ref, entries); err != nil {
		return 0, err
	}
	return amount, nil
}
