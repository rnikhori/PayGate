package ledger

import (
	"fmt"
	"regexp"
	"time"
)

// AccountType classifies a LedgerAccount the way double-entry bookkeeping
// does, matching the ledger_accounts.type CHECK constraint in
// backend/migrations.
type AccountType string

const (
	// AssetAccount tracks funds PayGate holds or is owed (e.g. funds in
	// transit to a merchant's bank).
	AssetAccount AccountType = "asset"
	// LiabilityAccount tracks funds PayGate owes out: to merchants
	// (payable accounts), or held on a customer's behalf pending
	// settlement.
	LiabilityAccount AccountType = "liability"
	// RevenueAccount tracks PayGate's own earned income (processing
	// fees).
	RevenueAccount AccountType = "revenue"
)

// Valid reports whether t is one of the account types the database allows.
func (t AccountType) Valid() bool {
	switch t {
	case AssetAccount, LiabilityAccount, RevenueAccount:
		return true
	default:
		return false
	}
}

// Account mirrors one row of the ledger_accounts table.
type Account struct {
	ID        int64
	Code      string
	Type      AccountType
	Name      string
	CreatedAt time.Time
}

// Fixed system account codes referenced throughout the design (see
// docs/ARCHITECTURE.md's "Ledger model" section). These are seeded exactly
// once, via the backend/migrations/*_create_ledger_accounts migration —
// never created ad hoc at runtime.
const (
	CustomerSuspenseAccountCode   = "customer_suspense"
	GatewayFeeRevenueAccountCode  = "gateway_fee_revenue"
	SettlementClearingAccountCode = "settlement_clearing"
)

// MerchantPayableAccountPrefix namespaces the per-merchant payable account
// convention: a merchant's available balance lives in the account coded
// "merchant_payable:<merchant_id>" (see MerchantPayableAccountCode).
const MerchantPayableAccountPrefix = "merchant_payable"

// MerchantPayableAccountType is the AccountType every merchant_payable:*
// account is created with: PayGate owes that balance out to the merchant.
const MerchantPayableAccountType = LiabilityAccount

// accountCodePattern is the account-code naming convention, enforced here
// at construction time — before any SQL is issued — and mirrored by the
// ledger_accounts_code_format CHECK constraint in the migration that
// creates the table, so no code path (application code or a raw SQL
// insert) can create an account that violates it.
//
// A code is either a bare lowercase snake_case token (the fixed system
// accounts above), or that same token namespacing an entity id after a
// single colon (e.g. "merchant_payable:<merchant_id>").
var accountCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z0-9][a-z0-9_-]*)?$`)

// maxAccountCodeLen matches the ledger_accounts.code column's VARCHAR(128).
const maxAccountCodeLen = 128

// ValidateAccountCode reports an error if code does not conform to the
// ledger's account-code naming convention.
func ValidateAccountCode(code string) error {
	if code == "" {
		return fmt.Errorf("ledger: account code must not be empty")
	}
	if len(code) > maxAccountCodeLen {
		return fmt.Errorf("ledger: account code %q exceeds %d characters", code, maxAccountCodeLen)
	}
	if !accountCodePattern.MatchString(code) {
		return fmt.Errorf("ledger: account code %q does not match the naming convention %s", code, accountCodePattern.String())
	}
	return nil
}

// MerchantPayableAccountCode returns the ledger_accounts.code for the given
// merchant's payable account, per the "merchant_payable:<merchant_id>"
// convention described in docs/ARCHITECTURE.md. merchantID must itself be
// non-empty and must produce a code that still satisfies the naming
// convention (in particular, it must not contain a colon, which would make
// the resulting code ambiguous to parse back apart).
func MerchantPayableAccountCode(merchantID string) (string, error) {
	if merchantID == "" {
		return "", fmt.Errorf("ledger: merchant id must not be empty")
	}
	code := MerchantPayableAccountPrefix + ":" + merchantID
	if err := ValidateAccountCode(code); err != nil {
		return "", fmt.Errorf("ledger: merchant id %q produces an invalid account code: %w", merchantID, err)
	}
	return code, nil
}
