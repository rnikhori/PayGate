package ledger

import "testing"

func TestAccountTypeValid(t *testing.T) {
	valid := []AccountType{AssetAccount, LiabilityAccount, RevenueAccount}
	for _, typ := range valid {
		if !typ.Valid() {
			t.Errorf("AccountType(%q).Valid() = false, want true", typ)
		}
	}

	invalid := []AccountType{"", "ASSET", "equity", "asset "}
	for _, typ := range invalid {
		if typ.Valid() {
			t.Errorf("AccountType(%q).Valid() = true, want false", typ)
		}
	}
}

func TestValidateAccountCodeAcceptsSystemAccounts(t *testing.T) {
	for _, code := range []string{
		CustomerSuspenseAccountCode,
		GatewayFeeRevenueAccountCode,
		SettlementClearingAccountCode,
	} {
		if err := ValidateAccountCode(code); err != nil {
			t.Errorf("ValidateAccountCode(%q) = %v, want nil", code, err)
		}
	}
}

func TestValidateAccountCodeAcceptsNamespacedCodes(t *testing.T) {
	cases := []string{
		"merchant_payable:550e8400-e29b-41d4-a716-446655440000",
		"merchant_payable:mch_0123abc",
		"a:b",
	}
	for _, code := range cases {
		if err := ValidateAccountCode(code); err != nil {
			t.Errorf("ValidateAccountCode(%q) = %v, want nil", code, err)
		}
	}
}

func TestValidateAccountCodeRejectsInvalid(t *testing.T) {
	cases := []string{
		"",                         // empty
		"Merchant_Payable",         // uppercase
		"1_starts_with_digit",      // must start with a letter
		"merchant payable",         // space
		"merchant_payable:",        // empty namespace segment
		"merchant_payable:UPPER",   // uppercase namespace segment
		"merchant_payable::double", // namespace segment starting with ':'
		"merchant:payable:id",      // two colons
		"-leading-hyphen",
	}
	for _, code := range cases {
		if err := ValidateAccountCode(code); err == nil {
			t.Errorf("ValidateAccountCode(%q) = nil, want an error", code)
		}
	}
}

func TestValidateAccountCodeRejectsTooLong(t *testing.T) {
	long := make([]byte, maxAccountCodeLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateAccountCode(string(long)); err == nil {
		t.Errorf("ValidateAccountCode(<%d bytes>) = nil, want an error", len(long))
	}
}

func TestMerchantPayableAccountCode(t *testing.T) {
	got, err := MerchantPayableAccountCode("mch_abc123")
	if err != nil {
		t.Fatalf("MerchantPayableAccountCode(%q) returned error: %v", "mch_abc123", err)
	}
	want := "merchant_payable:mch_abc123"
	if got != want {
		t.Errorf("MerchantPayableAccountCode(%q) = %q, want %q", "mch_abc123", got, want)
	}
}

func TestMerchantPayableAccountCodeRejectsEmpty(t *testing.T) {
	if _, err := MerchantPayableAccountCode(""); err == nil {
		t.Error("MerchantPayableAccountCode(\"\") = nil error, want an error")
	}
}

func TestMerchantPayableAccountCodeRejectsIDWithColon(t *testing.T) {
	// A merchant id containing a colon would make the resulting code
	// ambiguous to parse back apart into prefix + id, so it must be
	// rejected rather than silently producing a code with two colons.
	if _, err := MerchantPayableAccountCode("mch:evil"); err == nil {
		t.Error("MerchantPayableAccountCode(\"mch:evil\") = nil error, want an error")
	}
}
