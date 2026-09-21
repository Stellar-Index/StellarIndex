package canonical

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestNewCryptoAsset_accepted(t *testing.T) {
	cases := []string{
		"BTC", "ETH", "USDT", "USDC", "SOL", "XRP", "ADA", "AVAX", "DOT", "LINK",
		// 2026-07-24 RedStone relayer expansion (ADR-0014 Amendments).
		"USDe", "sUSDe", "savUSD_FUNDAMENTAL",
		"SolvBTC_FUNDAMENTAL_USD", "SolvBTC.BBN_FUNDAMENTAL_USD",
	}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			a, err := NewCryptoAsset(code)
			if err != nil {
				t.Fatalf("NewCryptoAsset(%q): %v", code, err)
			}
			if a.Type != AssetCrypto {
				t.Errorf("Type = %q", a.Type)
			}
			if a.Code != code {
				t.Errorf("Code = %q, want %q", a.Code, code)
			}
			if err := a.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestNewCryptoAsset_rejected(t *testing.T) {
	cases := []string{"NOTACOIN", "", "btc" /* lowercase */, "SOMENEWTOKEN"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			_, err := NewCryptoAsset(code)
			if !errors.Is(err, ErrInvalidAsset) {
				t.Errorf("expected ErrInvalidAsset for %q, got %v", code, err)
			}
		})
	}
}

func TestCryptoAsset_wireForm(t *testing.T) {
	// String round-trips through ParseAsset.
	a, err := NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	s := a.String()
	if s != "crypto:BTC" {
		t.Errorf("String() = %q, want %q", s, "crypto:BTC")
	}
	parsed, err := ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	if !parsed.Equal(a) {
		t.Errorf("round-trip lost info: %+v vs %+v", parsed, a)
	}
}

func TestCryptoAsset_distinctFromClassicSameCode(t *testing.T) {
	// Intentionally different: crypto:USDC vs classic USDC-<issuer>.
	cryptoUSDC, _ := NewCryptoAsset("USDC")
	classicUSDC, _ := NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if cryptoUSDC.Equal(classicUSDC) {
		t.Error("crypto:USDC should not equal classic USDC — different semantics (global vs Circle's Stellar-issued)")
	}
}

func TestCryptoAsset_json(t *testing.T) {
	a, _ := NewCryptoAsset("ETH")

	// MarshalJSON emits the canonical string form.
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"crypto:ETH"` {
		t.Errorf("MarshalJSON = %q, want %q", b, `"crypto:ETH"`)
	}

	// String round-trips through UnmarshalJSON.
	var a2 Asset
	if err := json.Unmarshal(b, &a2); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !a.Equal(a2) {
		t.Errorf("JSON round-trip lost info: %+v vs %+v", a, a2)
	}
}

func TestCryptoAsset_validateRejectsForbiddenFields(t *testing.T) {
	// If someone manually constructs {Type: AssetCrypto, Code: "BTC",
	// Issuer: "G…"}, Validate must reject — crypto carries only Code.
	a := Asset{Type: AssetCrypto, Code: "BTC", Issuer: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := a.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for issuer on crypto asset, got %v", err)
	}
	a2 := Asset{Type: AssetCrypto, Code: "BTC", ContractID: "CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := a2.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for contract_id on crypto asset, got %v", err)
	}
}

func TestIsKnownCrypto(t *testing.T) {
	if !IsKnownCrypto("BTC") {
		t.Error("BTC should be known")
	}
	if IsKnownCrypto("NOTAREALTICKER") {
		t.Error("NOTAREALTICKER should not be known")
	}
}

// TestADR0014DocumentsEveryAllocatedCode pins T519: every code in
// knownCryptoCodes must have a citation in ADR-0014's text — either
// the initial allow-list or an Amendments entry, per the file's own
// policy ("Append new crypto codes here as a one-liner. Never
// supersede this ADR for an addition."). A code landed in the map
// without a matching doc entry is exactly the drift this test exists
// to catch.
func TestADR0014DocumentsEveryAllocatedCode(t *testing.T) {
	adrPath := filepath.Join(repoRoot(), "docs", "adr", "0014-crypto-ticker-representation.md")
	doc, err := os.ReadFile(adrPath)
	if err != nil {
		t.Fatalf("read %s: %v", adrPath, err)
	}
	text := string(doc)

	var missing []string
	for code := range knownCryptoCodes {
		if !strings.Contains(text, code) {
			missing = append(missing, code)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("ADR-0014 doesn't document %d allow-listed code(s): %s — add an Amendments entry",
		len(missing), strings.Join(missing, ", "))
}
