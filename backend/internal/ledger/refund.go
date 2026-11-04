package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRefundExceedsAvailableBalance is the domain error BuildRefundEntries
// wraps into its returned error when a refund would take more out of
// merchant_payable than this specific payment ever credited to it, net of
// any refunds already posted against it. It is distinct from
// ErrUnbalancedTransaction (which is about a set of entries failing to sum
// to zero) and from any database error, so a caller can tell "this refund
// was rejected because it over-refunds" apart from either with
// errors.Is(err, ledger.ErrRefundExceedsAvailableBalance).
var ErrRefundExceedsAvailableBalance = errors.New("ledger: refund exceeds the capture's available (unrefunded) balance")

// refundTransactionRefPrefix namespaces every refund's transaction_ref, the
// same way MerchantPayableAccountPrefix namespaces a merchant's payable
// account code.
const refundTransactionRefPrefix = "refund"

// RefundTransactionRef builds the transaction_ref a refund's journal
// entries are posted under: "refund:<captureTransactionRef>:<refundRef>".
//
// journal_entries has no column that points one transaction_ref at
// another, so this naming convention is what satisfies this issue's "refund
// posting references the original capture's transaction_ref for
// traceability" acceptance criterion without a schema change: reading a
// refund's transaction_ref back tells a human, or ResolveRefundContext
// below, exactly which capture it reverses. refundRef is the caller's own
// unique identifier for this particular refund (e.g. a Refund row's id,
// once the Payments package has one) — it is what lets the same capture be
// refunded more than once (see ResolveRefundContext's AlreadyRefunded)
// without two refunds colliding on one transaction_ref.
func RefundTransactionRef(captureTransactionRef, refundRef string) (string, error) {
	if captureTransactionRef == "" {
		return "", fmt.Errorf("ledger: capture transaction_ref must not be empty")
	}
	if refundRef == "" {
		return "", fmt.Errorf("ledger: refund ref must not be empty")
	}
	return fmt.Sprintf("%s:%s:%s", refundTransactionRefPrefix, captureTransactionRef, refundRef), nil
}

// refundLikeEscaper escapes the LIKE metacharacters ('%', '_') and the
// escape character itself ('\') in a capture's transaction_ref before it is
// embedded in a LIKE pattern (see refundsLikePattern) — without this, a
// capture transaction_ref that happens to contain a literal '%' or '_'
// would match more (or differently) than the exact refunds posted against
// it.
var refundLikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// refundsLikePattern returns the LIKE pattern (with its ESCAPE character)
// matching every transaction_ref RefundTransactionRef could have built for
// captureTransactionRef, for any refundRef — i.e. every refund ever posted
// against that one capture.
func refundsLikePattern(captureTransactionRef string) string {
	return refundTransactionRefPrefix + ":" + refundLikeEscaper.Replace(captureTransactionRef) + ":%"
}

// refundLikeEscapeChar is the ESCAPE character refundsLikePattern's pattern
// is built with, passed separately to the query so it never has to be
// interpolated into SQL text.
const refundLikeEscapeChar = `\`

// RefundContext is everything BuildRefundEntries needs to know about the
// capture being refunded, resolved once (by ResolveRefundContext) so both
// PostRefund (self-contained) and PostRefundTx (caller-owned transaction)
// build a refund's entries the same way.
type RefundContext struct {
	Accounts CaptureAccounts

	// GrossAmount is the original capture's full gross amount (the amount
	// that moved out of customer_suspense).
	GrossAmount int64

	// Fee is the original capture's fee (the amount that moved into
	// gateway_fee_revenue).
	Fee int64

	// AlreadyRefunded is the sum of every refund already posted against
	// this capture (via RefundTransactionRef's naming convention), in the
	// same units as GrossAmount.
	AlreadyRefunded int64
}

// sumEntries sums journal_entries.amount for accountID, optionally
// restricted to one transaction_ref (exact match) or to every
// transaction_ref matching a LIKE pattern — exactly one of transactionRef
// or likePattern is used, selected by which is non-empty. An account with
// no matching rows sums to 0, not an error, via the same COALESCE
// GetBalance (balance.go) relies on.
func sumEntries(ctx context.Context, db DB, accountID int64, transactionRef, likePattern string) (int64, error) {
	var (
		query string
		args  []any
	)
	switch {
	case likePattern != "":
		query = `SELECT COALESCE(SUM(amount), 0) FROM journal_entries WHERE account_id = $1 AND transaction_ref LIKE $2 ESCAPE '` + refundLikeEscapeChar + `'`
		args = []any{accountID, likePattern}
	default:
		query = `SELECT COALESCE(SUM(amount), 0) FROM journal_entries WHERE account_id = $1 AND transaction_ref = $2`
		args = []any{accountID, transactionRef}
	}

	var sum int64
	row := db.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&sum); err != nil {
		return 0, fmt.Errorf("ledger: sum journal entries for account %d: %w", accountID, err)
	}
	return sum, nil
}

// ResolveRefundContext resolves the 3 capture accounts (via
// ResolveCaptureAccounts) and reads back, from journal_entries itself, the
// original capture's gross amount and fee plus every refund already posted
// against it — everything BuildRefundEntries needs to compute a correctly
// proportional, correctly capped refund. It only reads — like
// ResolveCaptureAccounts, it never begins a transaction, so it is safe to
// call against either a bare *sql.DB (via Wrap) or a Tx already open for
// something else.
//
// A captureTransactionRef with no customer_suspense leg (nothing was ever
// captured under it, e.g. a typo'd ref) resolves GrossAmount to 0, which
// BuildRefundEntries rejects explicitly rather than silently posting a
// refund against a capture that doesn't exist.
func ResolveRefundContext(ctx context.Context, db DB, merchantID, captureTransactionRef string) (RefundContext, error) {
	if captureTransactionRef == "" {
		return RefundContext{}, fmt.Errorf("ledger: capture transaction_ref must not be empty")
	}

	accounts, err := ResolveCaptureAccounts(ctx, db, merchantID)
	if err != nil {
		return RefundContext{}, fmt.Errorf("ledger: resolve refund context: %w", err)
	}

	// The capture's customer_suspense leg is -grossAmount (see
	// BuildCaptureEntries); negate it back to a positive gross amount.
	suspenseLeg, err := sumEntries(ctx, db, accounts.CustomerSuspense.ID, captureTransactionRef, "")
	if err != nil {
		return RefundContext{}, fmt.Errorf("ledger: resolve refund context: read original capture: %w", err)
	}
	grossAmount := -suspenseLeg

	fee, err := sumEntries(ctx, db, accounts.GatewayFeeRevenue.ID, captureTransactionRef, "")
	if err != nil {
		return RefundContext{}, fmt.Errorf("ledger: resolve refund context: read original capture fee: %w", err)
	}

	alreadyRefunded, err := sumEntries(ctx, db, accounts.CustomerSuspense.ID, "", refundsLikePattern(captureTransactionRef))
	if err != nil {
		return RefundContext{}, fmt.Errorf("ledger: resolve refund context: read prior refunds: %w", err)
	}

	return RefundContext{
		Accounts:        accounts,
		GrossAmount:     grossAmount,
		Fee:             fee,
		AlreadyRefunded: alreadyRefunded,
	}, nil
}

// BuildRefundEntries builds — but does not post — the reversing entries for
// refunding refundAmount of the capture rc describes: a credit to
// customer_suspense for the full refundAmount (the customer gets exactly
// what they're refunded back), a debit to the merchant's payable account
// for refundAmount net of a proportionally reversed fee, and — only when
// that proportional fee reversal is non-zero — a debit to
// gateway_fee_revenue for it.
//
// The fee reversal is rc.Fee * refundAmount / rc.GrossAmount, truncated
// toward zero the same way FeeSchedule.Fee truncates a capture's fee: a
// partial refund never reverses more fee, proportionally, than the capture
// originally charged. When that truncates to exactly 0 (e.g. a very small
// partial refund), only 2 entries are posted — a zero-amount entry is never
// valid (see ValidateBalancedTransaction) — rather than a 3rd leg that
// moves no money.
//
// refundAmount must be positive and must not, combined with rc.AlreadyRefunded,
// exceed rc.GrossAmount: refunding more than a capture ever moved into
// merchant_payable for this payment is rejected with an error wrapping
// ErrRefundExceedsAvailableBalance, before any entry is built.
func BuildRefundEntries(rc RefundContext, refundAmount int64, currency string) ([]Entry, error) {
	if refundAmount <= 0 {
		return nil, fmt.Errorf("ledger: refund amount must be positive, got %d", refundAmount)
	}
	if err := ValidateCurrencyCode(currency); err != nil {
		return nil, err
	}
	if rc.GrossAmount <= 0 {
		return nil, fmt.Errorf("ledger: original capture has no gross amount on record (gross=%d); nothing to refund", rc.GrossAmount)
	}

	available := rc.GrossAmount - rc.AlreadyRefunded
	if refundAmount > available {
		return nil, fmt.Errorf("ledger: refund amount %d exceeds available (unrefunded) balance %d (gross %d, already refunded %d): %w",
			refundAmount, available, rc.GrossAmount, rc.AlreadyRefunded, ErrRefundExceedsAvailableBalance)
	}

	feeReversal := rc.Fee * refundAmount / rc.GrossAmount
	netReversal := refundAmount - feeReversal

	entries := make([]Entry, 0, 3)
	entries = append(entries,
		Entry{AccountID: rc.Accounts.CustomerSuspense.ID, Amount: refundAmount, Currency: currency},
		Entry{AccountID: rc.Accounts.MerchantPayable.ID, Amount: -netReversal, Currency: currency},
	)
	if feeReversal != 0 {
		entries = append(entries, Entry{AccountID: rc.Accounts.GatewayFeeRevenue.ID, Amount: -feeReversal, Currency: currency})
	}

	return entries, nil
}

// PostRefund resolves the refund's context, builds its reversing entries,
// and posts them as one transaction_ref (built by RefundTransactionRef) via
// PostJournalEntry — the self-contained path for a caller with no other
// write (e.g. a Refund row's status update, which doesn't exist in this
// codebase yet — the Payments package this belongs to, per
// docs/ARCHITECTURE.md's repo structure, is a later milestone) that needs
// to land in the same database transaction. Use PostRefundTx instead once
// such a caller exists.
func PostRefund(ctx context.Context, db Conn, captureTransactionRef, refundRef, merchantID string, refundAmount int64, currency string) error {
	transactionRef, err := RefundTransactionRef(captureTransactionRef, refundRef)
	if err != nil {
		return err
	}

	rc, err := ResolveRefundContext(ctx, db, merchantID, captureTransactionRef)
	if err != nil {
		return err
	}

	entries, err := BuildRefundEntries(rc, refundAmount, currency)
	if err != nil {
		return err
	}

	return PostJournalEntry(ctx, db, transactionRef, entries)
}

// PostRefundTx is PostRefund's composable counterpart: it builds the same
// entries but posts them via PostJournalEntryTx against a Tx the caller
// already began — the hook a future Payments package needs to update a
// Refund row's status atomically alongside these entries. rc must already
// be resolved (via ResolveRefundContext, called against the caller's db
// before — or, for the fixed system accounts, outside — its transaction
// began), the same way PostCaptureTx requires its CaptureAccounts resolved
// up front.
//
// As with PostJournalEntryTx, the caller owns committing or rolling back
// tx; this never does either itself.
func PostRefundTx(ctx context.Context, tx Tx, captureTransactionRef, refundRef string, rc RefundContext, refundAmount int64, currency string) error {
	transactionRef, err := RefundTransactionRef(captureTransactionRef, refundRef)
	if err != nil {
		return err
	}

	entries, err := BuildRefundEntries(rc, refundAmount, currency)
	if err != nil {
		return err
	}

	return PostJournalEntryTx(ctx, tx, transactionRef, entries)
}
