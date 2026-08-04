// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

func strptr(s string) *string { return &s }

// TestDustLiquiditySuppressed pins the shared valuation-integrity predicate
// that BOTH the listing path (fillMarketCapsFromSupply → computeMarketCapUSD)
// and the detail path (populateMarketCap) gate on. The AND is load-bearing:
// only a single backing venue AND a POSITIVE sub-floor 24h volume suppresses.
func TestDustLiquiditySuppressed(t *testing.T) {
	const floor = 1000.0
	cases := []struct {
		name        string
		sourceCount int
		volume      *string
		floor       float64
		want        bool
	}{
		{"dust: 1 source, $10 vol", 1, strptr("10"), floor, true},
		// M5: a parseable "0" is a BOGUS artifact (pure-Soroban SEP-41 plain
		// reader, or a SorobanVolumeReader error fallback), not a real
		// sub-floor reading — treat it as UNMEASURED → KEEP, identical to nil.
		{"bogus zero volume → unmeasured (M5)", 1, strptr("0"), floor, false},
		{"bogus zero.zero volume → unmeasured (M5)", 1, strptr("0.00"), floor, false},
		{"negative volume → unmeasured (M5)", 1, strptr("-5"), floor, false},
		{"single venue but liquid $100k", 1, strptr("100000"), floor, false},
		{"single venue exactly at floor", 1, strptr("1000"), floor, false},
		{"single venue above floor $2000", 1, strptr("2000"), floor, false},
		{"single venue positive sub-floor $10 suppresses", 1, strptr("10"), floor, true},
		{"multi-source thin $10", 2, strptr("10"), floor, false},
		{"multi-source thin, 3 venues", 3, strptr("5"), floor, false},
		{"floor disabled (0)", 1, strptr("10"), 0, false},
		// 2026-08-04: an unmeasured venue count (0) with a POSITIVE,
		// measured, sub-floor volume now suppresses — the measured dust
		// volume is itself the positive evidence, and the unmeasured-
		// count arm is where impersonator assets lived.
		{"unmeasured source count (0) + measured dust vol suppresses", 0, strptr("10"), floor, true},
		{"unmeasured source count (0) + nil volume kept", 0, nil, floor, false},
		{"volume unknown (nil)", 1, nil, floor, false},
		{"volume unparseable", 1, strptr("n/a"), floor, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dustLiquiditySuppressed(tc.sourceCount, tc.volume, tc.floor); got != tc.want {
				t.Errorf("dustLiquiditySuppressed(%d, %v, %v) = %v, want %v",
					tc.sourceCount, tc.volume, tc.floor, got, tc.want)
			}
		})
	}
}

// TestListingMarketCap_DustGuard drives the exact decision the listing fill
// loop (fillMarketCapsFromSupply) makes: suppress-and-flag when dust, else
// compute the real market cap via computeMarketCapUSD. The three headline
// cases from the finding are asserted end-to-end over the real functions.
//
// Supply 10^17 raw @ 7dp × $0.50 = $5,000,000,000 — the "billions" an
// un-guarded dust price would assert.
func TestListingMarketCap_DustGuard(t *testing.T) {
	const (
		floor = 1000.0
		circ  = "100000000000000000"
		price = "0.50"
		dec   = 7
	)
	cases := []struct {
		name        string
		sourceCount int
		volume      *string
		wantMC      string // "" means suppressed (null on the wire)
		wantLowLiq  bool
	}{
		{"dust one-trade → suppressed", 1, strptr("10"), "", true},
		{"single-venue liquid → present", 1, strptr("100000"), "5000000000.00", false},
		{"multi-source thin → present", 2, strptr("10"), "5000000000.00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotMC     string
				gotLowLiq bool
			)
			if dustLiquiditySuppressed(tc.sourceCount, tc.volume, floor) {
				gotLowLiq = true // cap left null
			} else {
				gotMC = computeMarketCapUSD(circ, price, dec)
			}
			if gotMC != tc.wantMC {
				t.Errorf("market cap = %q, want %q", gotMC, tc.wantMC)
			}
			if gotLowLiq != tc.wantLowLiq {
				t.Errorf("low-liquidity flag = %v, want %v", gotLowLiq, tc.wantLowLiq)
			}
		})
	}
}

// TestFillRowMarketCap_UnverifiedCollisionSuppressed — 2026-08-04: an
// unverified look-alike of a verified ticker must not publish
// price × supply as a headline valuation (XRP-GBXRPL45… published a
// $109.5M cap under XRP's ticker off its own manipulable market).
// circulating_supply (a raw fact) still surfaces.
func TestFillRowMarketCap_UnverifiedCollisionSuppressed(t *testing.T) {
	s := &Server{minMarketCapVolumeUSD: 1000}
	price := "1.07"
	row := AssetDetail{
		AssetID:                   "XRP-GBXRPL45000000000000000000000000000000000000000000000",
		Code:                      "XRP",
		Decimals:                  7,
		PriceUSD:                  &price,
		UnverifiedTickerCollision: true,
	}
	precise := map[string]string{row.AssetID: "1000000000000000"}
	s.fillRowMarketCap(&row, precise, nil, map[string]int{row.AssetID: 5})
	if row.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want suppressed (nil) for an unverified ticker collision", *row.MarketCapUSD)
	}
	if row.CirculatingSupply == nil {
		t.Error("circulating_supply must still surface — it is a raw fact, not a valuation")
	}
}

// TestFillRowMarketCap_NativeNeverDustSuppressed — the listing SQL
// forces native's source_count to NULL (→ 0 here); with 0 now
// suppressible, native needs the same carve-out the detail path has.
func TestFillRowMarketCap_NativeNeverDustSuppressed(t *testing.T) {
	s := &Server{minMarketCapVolumeUSD: 1000}
	price := "0.16"
	vol := "10" // absurd, but must not matter for native
	row := AssetDetail{
		AssetID: "native", Code: "XLM", Decimals: 7,
		PriceUSD: &price, VolumeUSD24h: &vol,
	}
	precise := map[string]string{"native": "100000000000000000"}
	s.fillRowMarketCap(&row, precise, nil, map[string]int{})
	if row.MarketCapUSD == nil {
		t.Fatal("native market cap must never be dust-suppressed")
	}
	if row.MarketCapLowLiquidity {
		t.Error("native must not carry market_cap_low_liquidity")
	}
}

// TestApplyUnverifiedWarning_ReferenceOnlyTicker — post-52b04a63
// regression fix: a reference-only ticker (USDT — the catalogue knows
// it as a well-known EXTERNAL asset with no verified Stellar issuance)
// must still produce the warning + envelope flag. Pre-fix the
// StellarEntry()==nil "unreachable" bail silently killed the warning
// for exactly the tickers impersonators target hardest, so
// /v1/assets/USDT-G… served clean while the listing flagged the same
// row. Pinned against the real embedded catalogue.
func TestApplyUnverifiedWarning_ReferenceOnlyTicker(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	asset, err := canonical.ParseAsset("USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatal(err)
	}
	var detail AssetDetail
	if !applyUnverifiedWarning(&detail, asset, cat) {
		t.Fatal("reference-only ticker collision must produce a warning")
	}
	w := detail.UnverifiedWarning
	if w == nil {
		t.Fatal("UnverifiedWarning is nil")
	}
	if w.VerifiedAssetID != "" {
		t.Errorf("verified_asset_id = %q, want empty (no verified Stellar issuance exists)", w.VerifiedAssetID)
	}
	if w.Note == "" || !strings.Contains(w.Note, "NO verified issuance on Stellar") {
		t.Errorf("note = %q, want the no-verified-issuance wording", w.Note)
	}

	// And the Stellar-issued case keeps its redirect target.
	usdc, err := canonical.ParseAsset("USDC-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatal(err)
	}
	var d2 AssetDetail
	if !applyUnverifiedWarning(&d2, usdc, cat) {
		t.Fatal("USDC look-alike must warn")
	}
	if d2.UnverifiedWarning.VerifiedAssetID == "" {
		t.Error("USDC look-alike must carry the verified asset id to redirect to")
	}
}

// stubListingGate implements PriceSubstanceGate with a per-pair-string
// verdict map ("<base>|<quote>" keys; missing = deny).
type stubListingGate struct{ allow map[string]bool }

func (g *stubListingGate) Allowed(_ context.Context, base, quote canonical.Asset, _ string) bool {
	return g.allow[base.String()+"|"+quote.String()]
}

// TestApplySubstanceGateToListing — the listing's price_usd enrichment
// (7-day catalogue SQL, outside /v1/price's read path) must respect
// the same thin-market gate: withheld rows lose price_usd + the change
// pills, allowed rows keep them, and fiat/crypto catalogue rows are
// out of scope.
func TestApplySubstanceGateToListing(t *testing.T) {
	peg, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	deep := "AQUA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	thin := "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	gate := &stubListingGate{allow: map[string]bool{
		deep + "|native": true, // deep pair passes vs XLM
	}}
	s := &Server{substance: gate, usdPeggedClassics: []canonical.Asset{peg}}

	p1, p2, p3 := "0.001", "0.13", "0.16"
	ch := "+1.00"
	rows := []AssetDetail{
		{AssetID: deep, Code: "AQUA", PriceUSD: &p1, Change24hPct: &ch},
		{AssetID: thin, Code: "SCAM", PriceUSD: &p2, Change24hPct: &ch},
		{AssetID: "native", Code: "XLM", PriceUSD: &p3, Change24hPct: &ch},
		{AssetID: "fiat:EUR", Code: "EUR", PriceUSD: &p3},
	}
	s.applySubstanceGateToListing(context.Background(), rows)

	if rows[0].PriceUSD == nil {
		t.Error("deep pair must keep its listed price")
	}
	if rows[1].PriceUSD != nil {
		t.Error("thin pair must lose its listed price")
	}
	if rows[1].Change24hPct != nil {
		t.Error("thin pair must lose its change pills (derived from the withheld price)")
	}
	if rows[2].PriceUSD == nil {
		t.Error("native must never be gated on the listing")
	}
	if rows[3].PriceUSD == nil {
		t.Error("fiat rows are out of gate scope")
	}
}
