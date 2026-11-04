package ledger

import (
	"context"
	"fmt"
)

// FeeSchedule is the per-merchant fee configuration a capture charges
// against the gross amount it moves out of customer_suspense.
//
// Its real home, per docs/ARCHITECTURE.md's core domain entities, is a
// merchant's own configuration — "Design Merchant schema with status
// lifecycle" (the onboarding epic) and "Implement fee calculation and
// deduction (MDR, GST) per transaction" (the settlements epic) are what
// will eventually define how a FeeSchedule is stored and computed per
// merchant. Neither exists yet in this codebase, and internal/ledger must
// not import internal/merchants or internal/payments even once they do
// (see doc.go). FeeSchedule is this issue's honest stand-in: a plain value
// the caller supplies — today, a test; once the Merchant schema and fee
// calculation land, that later code reading a merchant's stored
// configuration — so PostCapture's ledger math is correct now and ready to
// receive a real, merchant-sourced value without this package changing.
type FeeSchedule struct {
	// BasisPoints is the percentage fee rate in hundredths of a percent
	// (e.g. 200 = 2.00%), matching how merchant discount rates (MDR) are
	// conventionally quoted.
	BasisPoints int64
}

// basisPointsDenominator converts BasisPoints into a fraction: 10000 basis
// points is 100%.
const basisPointsDenominator = 10000

// Fee computes this schedule's cut of grossAmount, truncating toward zero
// (rounding down) so a merchant is never charged more than the configured
// rate.
func (s FeeSchedule) Fee(grossAmount int64) int64 {
	return grossAmount * s.BasisPoints / basisPointsDenominator
}

// Validate reports an error unless s is a legal fee schedule: a
// non-negative rate strictly less than 100%. A schedule at or above 100%
// would take the merchant's entire gross amount, or more, leaving nothing
// (or a negative amount) to credit merchant_payable, which is never a
// valid capture.
func (s FeeSchedule) Validate() error {
	if s.BasisPoints < 0 {
		return fmt.Errorf("ledger: fee schedule basis points must not be negative, got %d", s.BasisPoints)
	}
	if s.BasisPoints >= basisPointsDenominator {
		return fmt.Errorf("ledger: fee schedule basis points %d must be less than %d (100%%)", s.BasisPoints, basisPointsDenominator)
	}
	return nil
}

// CaptureAccounts holds the three ledger accounts a payment capture posts
// against, resolved once up front by ResolveCaptureAccounts so both
// PostCapture (self-contained) and PostCaptureTx (caller-owned transaction)
// build their entries the same way.
type CaptureAccounts struct {
	CustomerSuspense  Account
	MerchantPayable   Account
	GatewayFeeRevenue Account
}

// ResolveCaptureAccounts looks up the fixed customer_suspense and
// gateway_fee_revenue accounts and ensures merchantID's merchant_payable:*
// account exists, returning all three together. It only reads (and, for
// the merchant's account, idempotently upserts) — it never begins a
// transaction — so it is safe to call with either a bare *sql.DB (wrapped
// via Wrap) or a Tx already open for something else.
func ResolveCaptureAccounts(ctx context.Context, db DB, merchantID string) (CaptureAccounts, error) {
	suspense, err := GetAccountByCode(ctx, db, CustomerSuspenseAccountCode)
	if err != nil {
		return CaptureAccounts{}, fmt.Errorf("ledger: resolve capture accounts: %w", err)
	}

	feeRevenue, err := GetAccountByCode(ctx, db, GatewayFeeRevenueAccountCode)
	if err != nil {
		return CaptureAccounts{}, fmt.Errorf("ledger: resolve capture accounts: %w", err)
	}

	payable, err := EnsureMerchantPayableAccount(ctx, db, merchantID)
	if err != nil {
		return CaptureAccounts{}, fmt.Errorf("ledger: resolve capture accounts: %w", err)
	}

	return CaptureAccounts{
		CustomerSuspense:  suspense,
		MerchantPayable:   payable,
		GatewayFeeRevenue: feeRevenue,
	}, nil
}

// BuildCaptureEntries builds — but does not post — the 3 balanced entries a
// payment capture requires: a debit to customer_suspense for the full
// gross amount, a credit to the merchant's payable account for the gross
// amount net of the fee schedule's cut, and a credit to
// gateway_fee_revenue for that fee.
//
// grossAmount must be positive, and schedule must compute a fee strictly
// between 0 and grossAmount: this issue's acceptance criteria call for
// exactly 3 entries on every capture, so a fee schedule that would yield a
// zero fee (or one at or above the gross amount) is rejected rather than
// silently posting only 2 legs.
func BuildCaptureEntries(accounts CaptureAccounts, grossAmount int64, currency string, schedule FeeSchedule) ([]Entry, error) {
	if grossAmount <= 0 {
		return nil, fmt.Errorf("ledger: capture gross amount must be positive, got %d", grossAmount)
	}
	if err := ValidateCurrencyCode(currency); err != nil {
		return nil, err
	}
	if err := schedule.Validate(); err != nil {
		return nil, err
	}

	fee := schedule.Fee(grossAmount)
	if fee <= 0 {
		return nil, fmt.Errorf("ledger: fee schedule yields a non-positive fee (%d) for gross amount %d; a capture always posts a fee leg", fee, grossAmount)
	}
	net := grossAmount - fee
	if net <= 0 {
		return nil, fmt.Errorf("ledger: fee %d is not less than gross amount %d", fee, grossAmount)
	}

	return []Entry{
		{AccountID: accounts.CustomerSuspense.ID, Amount: -grossAmount, Currency: currency},
		{AccountID: accounts.MerchantPayable.ID, Amount: net, Currency: currency},
		{AccountID: accounts.GatewayFeeRevenue.ID, Amount: fee, Currency: currency},
	}, nil
}

// PostCapture resolves the capture's accounts, builds its 3 balanced
// entries, and posts them as one transaction_ref via PostJournalEntry —
// the self-contained path for a caller with no other write (e.g. a Payment
// row's status update, which doesn't exist in this codebase yet — see
// FeeSchedule's doc comment) that needs to land in the same database
// transaction. Use PostCaptureTx instead once such a caller exists.
func PostCapture(ctx context.Context, db Conn, transactionRef, merchantID string, grossAmount int64, currency string, schedule FeeSchedule) error {
	if merchantID == "" {
		return fmt.Errorf("ledger: capture merchant id must not be empty")
	}

	accounts, err := ResolveCaptureAccounts(ctx, db, merchantID)
	if err != nil {
		return err
	}

	entries, err := BuildCaptureEntries(accounts, grossAmount, currency, schedule)
	if err != nil {
		return err
	}

	return PostJournalEntry(ctx, db, transactionRef, entries)
}

// PostCaptureTx is PostCapture's composable counterpart: it builds the same
// 3 entries but posts them via PostJournalEntryTx against a Tx the caller
// already began — the hook a future Payments package needs to update a
// Payment row's status to captured and post these entries atomically.
// accounts must already be resolved (via ResolveCaptureAccounts, called
// against the caller's db before — or, for the fixed system accounts,
// outside — its transaction began) since Tx here exposes no read method.
//
// As with PostJournalEntryTx, the caller owns committing or rolling back
// tx; this never does either itself.
func PostCaptureTx(ctx context.Context, tx Tx, transactionRef string, accounts CaptureAccounts, grossAmount int64, currency string, schedule FeeSchedule) error {
	entries, err := BuildCaptureEntries(accounts, grossAmount, currency, schedule)
	if err != nil {
		return err
	}
	return PostJournalEntryTx(ctx, tx, transactionRef, entries)
}
