package ledger

import (
	"errors"
	"testing"
)

func TestValidateBalancedTransactionAcceptsTwoLegBalancedEntries(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_1", AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_1", AccountID: 2, Amount: -10000, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err != nil {
		t.Errorf("ValidateBalancedTransaction() = %v, want nil", err)
	}
}

func TestValidateBalancedTransactionAcceptsMultiLegBalancedEntries(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_2", AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_2", AccountID: 2, Amount: -9500, Currency: "INR"},
		{TransactionRef: "txn_2", AccountID: 3, Amount: -500, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err != nil {
		t.Errorf("ValidateBalancedTransaction() = %v, want nil", err)
	}
}

func TestValidateBalancedTransactionRejectsFewerThanTwoEntries(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_3", AccountID: 1, Amount: 10000, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error for a single-row transaction")
	}
}

func TestValidateBalancedTransactionRejectsEmptyEntries(t *testing.T) {
	if err := ValidateBalancedTransaction(nil); err == nil {
		t.Error("ValidateBalancedTransaction(nil) = nil error, want an error")
	}
}

func TestValidateBalancedTransactionRejectsUnbalancedSum(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_4", AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_4", AccountID: 2, Amount: -9000, Currency: "INR"},
	}
	err := ValidateBalancedTransaction(entries)
	if err == nil {
		t.Fatal("ValidateBalancedTransaction() = nil error, want an error for an unbalanced sum")
	}
	if !errors.Is(err, ErrUnbalancedTransaction) {
		t.Errorf("ValidateBalancedTransaction() error = %v, want it to wrap ErrUnbalancedTransaction", err)
	}
}

func TestValidateBalancedTransactionRejectsEmptyTransactionRef(t *testing.T) {
	entries := []Entry{
		{AccountID: 1, Amount: 10000, Currency: "INR"},
		{AccountID: 2, Amount: -10000, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error when the first entry's transaction_ref is empty")
	}
}

func TestValidateBalancedTransactionRejectsMismatchedTransactionRef(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_5", AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_other", AccountID: 2, Amount: -10000, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error for mismatched transaction_ref")
	}
}

func TestValidateBalancedTransactionRejectsZeroAmount(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_6", AccountID: 1, Amount: 0, Currency: "INR"},
		{TransactionRef: "txn_6", AccountID: 2, Amount: 0, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error for a zero-amount entry")
	}
}

func TestValidateBalancedTransactionRejectsInvalidCurrency(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_7", AccountID: 1, Amount: 10000, Currency: "inr"},
		{TransactionRef: "txn_7", AccountID: 2, Amount: -10000, Currency: "inr"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error for a lowercase currency code")
	}
}

func TestValidateBalancedTransactionRejectsMissingAccountID(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_8", AccountID: 0, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_8", AccountID: 2, Amount: -10000, Currency: "INR"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error for a missing account id")
	}
}

func TestValidateBalancedTransactionBalancesPerCurrencyIndependently(t *testing.T) {
	entries := []Entry{
		{TransactionRef: "txn_9", AccountID: 1, Amount: 10000, Currency: "INR"},
		{TransactionRef: "txn_9", AccountID: 2, Amount: -10000, Currency: "INR"},
		{TransactionRef: "txn_9", AccountID: 3, Amount: 500, Currency: "USD"},
	}
	if err := ValidateBalancedTransaction(entries); err == nil {
		t.Error("ValidateBalancedTransaction() = nil error, want an error when one currency's legs don't sum to zero")
	}
}

func TestValidateCurrencyCodeRejectsEmpty(t *testing.T) {
	if err := ValidateCurrencyCode(""); err == nil {
		t.Error("ValidateCurrencyCode(\"\") = nil error, want an error")
	}
}

func TestValidateCurrencyCodeAcceptsValidISOCode(t *testing.T) {
	if err := ValidateCurrencyCode("INR"); err != nil {
		t.Errorf("ValidateCurrencyCode(\"INR\") = %v, want nil", err)
	}
}
