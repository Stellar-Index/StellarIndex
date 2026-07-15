package v1

import (
	"context"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestUsdMarketValue covers the F2 market-cap math helper directly
// (internal access — usdMarketValue is unexported because nothing
// outside the package needs it). Integration coverage of the helper
// via /v1/assets/{id} lives in assets_f2_test.go.
func TestUsdMarketValue(t *testing.T) {
	tests := []struct {
		name     string
		stroops  string
		price    string
		decimals int
		want     string
	}{
		// 100 XLM (1_000_000_000 stroops, 7 decimals) × $0.07 = $7.00
		{"XLM 100×0.07", "1000000000", "0.07", 7, "7.00"},
		// 1 USDC (10_000_000 stroops, 7 decimals) × $1 = $1.00
		{"USDC 1×1", "10000000", "1", 7, "1.00"},
		// 0 stroops reads $0.00 (legitimate zero, not error).
		{"zero stroops", "0", "0.07", 7, "0.00"},
		// Sub-cent products truncate to $0.00 via FloatString(2).
		{"sub-cent truncates", "1", "0.0001", 7, "0.00"},
		// Very large numbers stay precise — no float underflow.
		// 500_018_068_120_000_000 stroops / 10^7 = 50_001_806_812 XLM
		// × $0.07 = $3,500,126,476.84.
		{"giant XLM", "500018068120000000", "0.0700000", 7, "3500126476.84"},
		// decimals=0 means "stroops are already asset units"
		// (as for some SEP-41 tokens that emit raw integers).
		{"decimals=0", "100", "1.50", 0, "150.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := usdMarketValue(mustBigIntInternal(tc.stroops), tc.price, tc.decimals)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsdMarketValue_BadInputs(t *testing.T) {
	if _, err := usdMarketValue(nil, "1", 7); err == nil {
		t.Error("expected error for nil amountStroops")
	}
	if _, err := usdMarketValue(mustBigIntInternal("100"), "not-a-price", 7); err == nil {
		t.Error("expected error for unparseable price")
	}
	if _, err := usdMarketValue(mustBigIntInternal("100"), "1", -1); err == nil {
		t.Error("expected error for negative decimals")
	}
}

// TestPctChange covers the trailing-24h percentage helper. The
// signed-leading-"+" convention is part of the wire contract — a
// regression here would silently flip the field's interpretation
// for clients that distinguish "0.00" (no change) from "+0.00".
func TestPctChange(t *testing.T) {
	tests := []struct {
		name      string
		now, then string
		want      string
	}{
		{"up 1.27%", "0.07127", "0.07", "+1.81"},
		{"flat", "1.00", "1.00", "0.00"},
		{"down 5%", "0.95", "1.00", "-5.00"},
		{"big up", "150.00", "100.00", "+50.00"},
		{"sub-cent up", "1.0000001", "1.00", "0.00"},
		// Two-decimal rounding is half-away-from-zero (FloatString
		// behaviour) — pinned because consumer charts depend on it.
		{"rounds half up", "1.005", "1.00", "+0.50"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pctChange(tc.now, tc.then)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPctChange_BadInputs(t *testing.T) {
	if _, err := pctChange("not-a-price", "1"); err == nil {
		t.Error("expected error for unparseable now")
	}
	if _, err := pctChange("1", "not-a-price"); err == nil {
		t.Error("expected error for unparseable then")
	}
	if _, err := pctChange("1", "0"); err == nil {
		t.Error("expected error for then=0 (would divide by zero)")
	}
	if _, err := pctChange("1", "-1"); err == nil {
		t.Error("expected error for negative then")
	}
}

func mustBigIntInternal(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("mustBigIntInternal: bad input " + s)
	}
	return v
}

// TestSep1DisplayToRawUnits — the SEP-1 max_supply overlay's unit
// conversion. SEP-1 declares supply in DISPLAY units; the snapshot
// carries RAW units. Getting this wrong understates max_supply/FDV
// by 10^7 for every classic asset, so the scaling contract is
// pinned here.
func TestSep1DisplayToRawUnits(t *testing.T) {
	tests := []struct {
		name     string
		display  string
		decimals int
		want     string
		ok       bool
	}{
		{"whole tokens ×10^7", "50000000", 7, "500000000000000", true},
		{"fractional but integral raw", "21000000.5", 7, "210000005000000", true},
		{"decimals=0 passthrough", "1000", 0, "1000", true},
		{"zero is valid", "0", 7, "0", true},
		{"sub-raw fraction rejected", "1.00000005", 7, "", false}, // ×10^7 = 10000000.5 — fractional stroops don't exist
		{"negative rejected", "-100", 7, "", false},
		{"junk rejected", "TBD", 7, "", false},
		{"rational syntax rejected", "1/3", 7, "", false},
		{"e-notation rejected", "5e8", 7, "", false},
		{"negative decimals rejected", "100", -1, "", false},
		{"empty rejected", "", 7, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sep1DisplayToRawUnits(tc.display, tc.decimals)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSep1DeclaredMaxResolver — precedence + blocking rules for the
// serving-path SEP-1 max resolver: max_number beats fixed_number, an
// explicit is_unlimited=true blocks both, absent declarations return
// ok=false, and the resolver never errors.
func TestSep1DeclaredMaxResolver(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		name    string
		detail  *AssetDetail
		wantRaw string
		wantOK  bool
	}{
		{"nil detail", nil, "", false},
		{"no declaration", &AssetDetail{Decimals: 7}, "", false},
		{"max_number", &AssetDetail{Decimals: 7, MaxNumber: strPtr("100")}, "1000000000", true},
		{"fixed_number fallback", &AssetDetail{Decimals: 7, FixedNumber: strPtr("42")}, "420000000", true},
		{"max_number beats fixed_number", &AssetDetail{Decimals: 7, MaxNumber: strPtr("100"), FixedNumber: strPtr("42")}, "1000000000", true},
		{"is_unlimited blocks", &AssetDetail{Decimals: 7, MaxNumber: strPtr("100"), IsUnlimited: boolPtr(true)}, "", false},
		{"is_unlimited=false does not block", &AssetDetail{Decimals: 7, MaxNumber: strPtr("100"), IsUnlimited: boolPtr(false)}, "1000000000", true},
		{"whitespace-only declaration", &AssetDetail{Decimals: 7, MaxNumber: strPtr("  ")}, "", false},
		{"junk declaration", &AssetDetail{Decimals: 7, MaxNumber: strPtr("~21M")}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, ok, err := sep1DeclaredMaxResolver{detail: tc.detail}.SEP1MaxSupply(context.Background(), canonical.Asset{})
			if err != nil {
				t.Fatalf("resolver must never error; got %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (raw %q)", ok, tc.wantOK, raw)
			}
			if ok && raw != tc.wantRaw {
				t.Errorf("raw = %q, want %q", raw, tc.wantRaw)
			}
		})
	}
}
