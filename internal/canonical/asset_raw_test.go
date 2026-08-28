package canonical

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// Red-proof (oracle capture-totality design, PR-1): on origin/main
// ParseAsset("raw:BTC") fell into the classic `<code>:<issuer>` split
// and failed with `invalid asset: account id "BTC" ...`, so a raw:
// row could never be written (Value → Validate) nor read back (every
// oracle_updates reader re-parses with ParseAsset → 500).
func TestOracleRawAsset_wireForm(t *testing.T) {
	cases := []struct{ wire, code string }{
		{"raw:BTC", "BTC"},
		{"raw:NOTACOIN", "NOTACOIN"},
		// RedStone feed_ids are ScString and carry `.`, `_`, `/`.
		{"raw:SolvBTC.BBN_FUNDAMENTAL/USD", "SolvBTC.BBN_FUNDAMENTAL/USD"},
		// Reflector/Band symbols are ScSymbol [A-Za-z0-9_]{1,32}.
		{"raw:EURC_ETHEREUM", "EURC_ETHEREUM"},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			parsed, err := ParseAsset(tc.wire)
			if err != nil {
				t.Fatalf("ParseAsset(%q): %v", tc.wire, err)
			}
			if parsed.Type != AssetOracleRaw {
				t.Errorf("Type = %q, want %q", parsed.Type, AssetOracleRaw)
			}
			if parsed.Code != tc.code {
				t.Errorf("Code = %q, want %q", parsed.Code, tc.code)
			}
			if got := parsed.String(); got != tc.wire {
				t.Errorf("String() = %q, want %q (round-trip)", got, tc.wire)
			}
			if err := parsed.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
			built, err := NewOracleRawAsset(tc.code)
			if err != nil {
				t.Fatalf("NewOracleRawAsset(%q): %v", tc.code, err)
			}
			if !built.Equal(parsed) {
				t.Errorf("constructor/parser disagree: %+v vs %+v", built, parsed)
			}
		})
	}
}

func TestOracleRawAsset_rejected(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"space":       "BT C",
		"lead-space":  " BTC",
		"tab":         "BTC\t",
		"newline":     "BTC\n",
		"control":     "BTC\x01",
		"del":         "BTC\x7f",
		"non-ascii":   "BTC€",
		"65-bytes":    strings.Repeat("A", 65),
		"only-spaces": "   ",
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOracleRawAsset(code); !errors.Is(err, ErrInvalidAsset) {
				t.Errorf("NewOracleRawAsset(%q): want ErrInvalidAsset, got %v", code, err)
			}
			a := Asset{Type: AssetOracleRaw, Code: code}
			if err := a.Validate(); !errors.Is(err, ErrInvalidAsset) {
				t.Errorf("Validate(%q): want ErrInvalidAsset, got %v", code, err)
			}
			if _, err := ParseAsset("raw:" + code); !errors.Is(err, ErrInvalidAsset) {
				t.Errorf("ParseAsset(%q): want ErrInvalidAsset, got %v", "raw:"+code, err)
			}
		})
	}
	// Boundary: exactly 64 bytes is the cap, and every printable ASCII
	// byte 0x21..0x7E is allowed.
	if _, err := NewOracleRawAsset(strings.Repeat("A", 64)); err != nil {
		t.Errorf("64-byte code must be accepted: %v", err)
	}
	var sb strings.Builder
	for c := byte(0x21); c <= 0x7E; c++ {
		sb.WriteByte(c)
	}
	printable := sb.String()
	if _, err := NewOracleRawAsset(printable[:64]); err != nil {
		t.Errorf("printable ASCII 0x21.. must be accepted: %v", err)
	}
	if _, err := NewOracleRawAsset(printable[len(printable)-64:]); err != nil {
		t.Errorf("printable ASCII ..0x7E must be accepted: %v", err)
	}
}

func TestOracleRawAsset_validateRejectsForbiddenFields(t *testing.T) {
	withIssuer := Asset{Type: AssetOracleRaw, Code: "BTC", Issuer: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := withIssuer.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for issuer on raw asset, got %v", err)
	}
	withContract := Asset{Type: AssetOracleRaw, Code: "BTC", ContractID: "CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := withContract.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for contract_id on raw asset, got %v", err)
	}
}

// Storage + JSON round-trip: this is what lets every oracle_updates
// reader (LatestOracleUpdatesForAssets / LatestOracleObservation /
// LatestAggregatorPricesForPair, which all re-parse the asset column
// through ParseAsset/Scan) tolerate a raw row instead of failing the
// whole request.
func TestOracleRawAsset_sqlAndJSON(t *testing.T) {
	a, err := NewOracleRawAsset("SolvBTC.BBN_FUNDAMENTAL/USD")
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if s, _ := v.(string); s != "raw:SolvBTC.BBN_FUNDAMENTAL/USD" {
		t.Errorf("Value = %v, want raw:SolvBTC.BBN_FUNDAMENTAL/USD", v)
	}
	var scanned Asset
	if err := scanned.Scan([]byte("raw:SolvBTC.BBN_FUNDAMENTAL/USD")); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !scanned.Equal(a) {
		t.Errorf("Scan round-trip lost info: %+v vs %+v", scanned, a)
	}

	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"raw:SolvBTC.BBN_FUNDAMENTAL/USD"` {
		t.Errorf("MarshalJSON = %s", b)
	}
	var a2 Asset
	if err := json.Unmarshal(b, &a2); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !a.Equal(a2) {
		t.Errorf("JSON round-trip lost info: %+v vs %+v", a, a2)
	}
}

func TestAsset_IsMapped(t *testing.T) {
	usdc, _ := NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	sac, _ := NewSorobanAsset("CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	usd, _ := NewFiatAsset("USD")
	btc, _ := NewCryptoAsset("BTC")
	benji, _ := NewRWAAsset("BENJI")
	for _, a := range []Asset{NativeAsset(), usdc, sac, usd, btc, benji} {
		if !a.IsMapped() {
			t.Errorf("%s: IsMapped() = false, want true", a)
		}
	}
	raw, _ := NewOracleRawAsset("BTC")
	if raw.IsMapped() {
		t.Errorf("%s: IsMapped() = true, want false", raw)
	}
	// A raw symbol that happens to spell a known ticker is STILL not the
	// mapped asset — the two variants are never Equal.
	if raw.Equal(btc) {
		t.Error("raw:BTC must not Equal crypto:BTC")
	}
}

// Record-layer only: a raw symbol is never a Pair leg (never a VWAP
// input, never compared by the interpretation layer).
func TestOracleRawAsset_neverPairLeg(t *testing.T) {
	raw, _ := NewOracleRawAsset("NOTACOIN")
	usd, _ := NewFiatAsset("USD")
	if _, err := NewPair(raw, usd); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("NewPair(raw, fiat): want ErrInvalidAsset, got %v", err)
	}
	if _, err := NewPair(usd, raw); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("NewPair(fiat, raw): want ErrInvalidAsset, got %v", err)
	}
}

// The record layer accepts it: an OracleUpdate whose asset is raw and
// whose quote is fiat validates (this is the shape PR-2's decoders
// will emit and InsertOracleUpdate will persist).
func TestOracleRawAsset_oracleUpdateValidates(t *testing.T) {
	raw, _ := NewOracleRawAsset("NOTACOIN")
	usd, _ := NewFiatAsset("USD")
	u := OracleUpdate{
		Source:    "reflector-cex",
		Ledger:    1,
		TxHash:    strings.Repeat("ab", 32),
		Timestamp: time.Unix(1_700_000_000, 0),
		Asset:     raw,
		Quote:     usd,
		Price:     NewAmount(big.NewInt(1)),
		Decimals:  14,
	}
	if err := u.Validate(); err != nil {
		t.Fatalf("OracleUpdate.Validate with raw asset: %v", err)
	}
}
