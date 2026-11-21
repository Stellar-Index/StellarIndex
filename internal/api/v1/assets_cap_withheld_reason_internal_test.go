// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// wireCapReason is the market_cap_withheld_reason the detail serialises, "" when omitted.
func wireCapReason(t *testing.T, d AssetDetail) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Reason string `json:"market_cap_withheld_reason"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	return body.Reason
}

// TestMarketCapWithheldReason_EveryRefusalIsNamed — a detail that serves price_usd and circulating_supply but no cap says why,
// on every guard, and a served cap carries no reason.
func TestMarketCapWithheldReason_EveryRefusalIsNamed(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	direct := capTestAsset + "/" + defaultPriceQuote.String()
	vol := func(v string) func(*AssetDetail) { return func(d *AssetDetail) { d.VolumeUSD24h = &v } }
	stale := capTestSupply()
	stale.ObservedAt = time.Now().Add(-preciseSupplyMaxAge - time.Hour)
	held := capTestSupply()
	held.Basis = supply.BasisContractStorageBalances
	for _, tc := range []struct {
		name    string
		reader  withheldUSDReader
		snap    supply.Supply
		assetID string
		floor   float64
		between func(*AssetDetail)
		want    string
	}{
		{name: "direct price withheld for substance", reader: withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldSubstance)}}, want: "substance"},
		{name: "direct price withheld upstream", reader: withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldUpstreamLeg)}}, want: "upstream_leg"},
		{name: "direct price read failed", reader: withheldUSDReader{direct: {err: errors.New("price store unreachable")}}, want: "price_unread"},
		{name: "stale supply", snap: stale, want: "supply_stale"},
		{name: "unknown scale", between: func(d *AssetDetail) { d.DecimalsUndeclared = true }, want: "decimals_unknown"},
		{name: "decimals mismatch", between: func(d *AssetDetail) { d.MarketCapDecimalsMismatch = true }, want: "decimals_mismatch"},
		{name: "ticker collision", assetID: capTestImpostor, want: "ticker_collision"},
		{name: "dust floor", floor: 1000, between: vol("100"), want: "low_liquidity"},
		{name: "turnover ceiling", between: vol("100"), want: "low_liquidity"},
		{name: "held-balances supply", snap: held, want: "supply_floor"},
		{name: "thin price", between: func(d *AssetDetail) { p := "0.0767"; d.PriceUSD, d.ThinMarket = &p, true }, want: "thin_market"},
		{name: "cap served", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.snap.CirculatingSupply == nil {
				tc.snap = capTestSupply()
			}
			if tc.assetID == "" {
				tc.assetID = capTestAsset
			}
			s := capTestServer(tc.reader, tc.snap)
			s.VerifiedCurrencies = cat
			s.MinMarketCapVolumeUSD = tc.floor
			d := capTestRunAsset(t, s, tc.assetID, false, tc.between)
			if d.PriceUSD == nil || d.CirculatingSupply == nil {
				t.Fatalf("price_usd = %v circulating_supply = %v, want both served (the case under test)", d.PriceUSD, d.CirculatingSupply)
			}
			if got := wireCapReason(t, d); got != tc.want {
				t.Errorf("market_cap_withheld_reason = %q, want %q", got, tc.want)
			}
			if served := d.MarketCapUSD != nil; served != (tc.want == "") {
				t.Errorf("market_cap_usd = %q with reason %q: a cap must be served exactly when no reason is", strOrEmpty(d.MarketCapUSD), tc.want)
			}
			if tc.want != "" && d.FDVUSD != nil {
				t.Errorf("fdv_usd = %q beside a withheld cap", *d.FDVUSD)
			}
		})
	}
}

// TestMarketCapWithheldReason_LateFillsAreNamed — the thin pass and the declared peg fill after the cap was valued, so the
// handler names their refusal; a served cap or a missing price carries none.
func TestMarketCapWithheldReason_LateFillsAreNamed(t *testing.T) {
	p, c := "1.00", "10000000"
	for _, tc := range []struct {
		name string
		d    AssetDetail
		want string
	}{
		{"thin pass", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, ThinMarket: true}, "thin_market"},
		{"declared peg", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, PriceBasis: priceBasisDeclaredPeg}, "price_basis"},
		{"global market", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, PriceBasis: priceBasisGlobalMarket}, "price_basis"},
		{"earlier reason kept", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, ThinMarket: true, MarketCapWithheldReason: MarketCapWithheldSupplyFloor}, "supply_floor"},
		{"cap served", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, MarketCapUSD: &p, ThinMarket: true}, ""},
		{"no supply", AssetDetail{PriceUSD: &p, ThinMarket: true}, ""},
		{"unpriced after the reason", AssetDetail{CirculatingSupply: &c, MarketCapWithheldReason: MarketCapWithheldLowLiquidity}, ""},
		{"thin pass re-read an unread price", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, priceUnread: true}, "price_unread"},
		{"peg replaced a thin price", AssetDetail{PriceUSD: &p, CirculatingSupply: &c, PriceBasis: priceBasisDeclaredPeg, MarketCapWithheldReason: MarketCapWithheldThinMarket}, "price_basis"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nameLateCapRefusal(&tc.d)
			if got := wireCapReason(t, tc.d); got != tc.want {
				t.Errorf("market_cap_withheld_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

type undeclaredDecimals struct{}

func (undeclaredDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return 0, false, nil
}

type lakeDecimals uint32

func (f lakeDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return uint32(f), true, nil
}

// TestFillRowMarketCap_WithholdsOffScalePrice — a listing price normalised on a scale the row cannot vouch for is withheld with
// its pills and cap; a declared peg is per whole token and stays.
func TestFillRowMarketCap_WithholdsOffScalePrice(t *testing.T) {
	const contract = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	for _, tc := range []struct {
		name  string
		dec   TokenDecimalsReader
		basis string
		want  PriceWithheldReason
	}{
		{"undeclared", undeclaredDecimals{}, "", PriceWithheldDecimalsUnknown},
		{"lake 18, no projection row", lakeDecimals(18), "", PriceWithheldDecimalsMismatch},
		{"declared peg on an unknown scale", undeclaredDecimals{}, priceBasisDeclaredPeg, ""},
		{"lake agrees", lakeDecimals(7), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price, pct := "1.00", "2.5"
			row := AssetDetail{AssetID: contract, Type: string(canonical.AssetSoroban), Decimals: 7, PriceUSD: &price, PriceBasis: tc.basis, Change1hPct: &pct}
			s := &Server{Options: Options{TokenDecimals: tc.dec}}
			s.fillRowMarketCap(context.Background(), &row, nil, nil, nil, nil, nil)
			if row.PriceWithheldReason != tc.want || (row.PriceUSD == nil) != (tc.want != "") || (row.Change1hPct == nil) != (tc.want != "") {
				t.Errorf("price_usd = %q change_1h = %v reason = %q, want reason %q", strOrEmpty(row.PriceUSD), row.Change1hPct, row.PriceWithheldReason, tc.want)
			}
		})
	}
}

// TestFillRowMarketCap_NamesTheRefusal — a listing row that keeps its price and supply but loses its cap says why.
func TestFillRowMarketCap_NamesTheRefusal(t *testing.T) {
	const asset = "FOO-GAFOO"
	precise := observedSupply(map[string]string{asset: "10000000000"})
	for _, tc := range []struct {
		name string
		row  func(*AssetDetail)
		s    *Server
		want string
	}{
		{"dust floor", func(r *AssetDetail) { v := "1"; r.VolumeUSD24h = &v }, &Server{Options: Options{MinMarketCapVolumeUSD: 1000}}, "low_liquidity"},
		{"turnover ceiling", func(r *AssetDetail) { v := "1"; r.VolumeUSD24h = &v }, &Server{Options: Options{MaxMarketCapVolumeRatio: 10}}, "low_liquidity"},
		{"ticker collision", func(r *AssetDetail) { r.UnverifiedTickerCollision = true }, &Server{}, "ticker_collision"},
		{"cap served", func(*AssetDetail) {}, &Server{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price := "1.00"
			row := AssetDetail{AssetID: asset, Code: "FOO", Decimals: 7, PriceUSD: &price}
			tc.row(&row)
			tc.s.fillRowMarketCap(context.Background(), &row, nil, precise, nil, nil, map[string]int{asset: 1})
			if got := wireCapReason(t, row); got != tc.want || (row.MarketCapUSD != nil) != (tc.want == "") {
				t.Errorf("market_cap_withheld_reason = %q market_cap_usd = %q, want %q", got, strOrEmpty(row.MarketCapUSD), tc.want)
			}
		})
	}
}

// TestReadProtocolToken_CarriesCapWithheldReason — the lending token block copies the asset route's cap, so it copies the
// reason a missing cap carries there: the BLND shape, a transitive price after a substance refusal.
func TestReadProtocolToken_CarriesCapWithheldReason(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	direct := capTestAsset + "/" + defaultPriceQuote.String()
	s := capTestServer(withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldSubstance)}}, capTestSupply())
	s.VerifiedCurrencies = cat
	s.AssetsReader = &truthAssetsReader{byID: map[string]timescale.AssetRow{}}
	var flags Flags
	tok, why := s.readProtocolToken(context.Background(), "BLND", &flags)
	if tok == nil || why != "" {
		t.Fatalf("token = %+v reason = %q, want a read", tok, why)
	}
	if tok.PriceUSD == nil || tok.CirculatingSupply == nil || tok.MarketCapUSD != nil {
		t.Fatalf("token = %+v, want a price and a supply with no cap (the case under test)", tok)
	}
	if tok.MarketCapWithheldReason != MarketCapWithheldReason(PriceWithheldSubstance) {
		t.Errorf("market_cap_withheld_reason = %q, want substance", tok.MarketCapWithheldReason)
	}
}

// TestFillDeclaredPegPricesInListing_NamesPriceBasis — a listing row the peg or global-market fill prices after the cap fill
// serves its supply with no cap, and says why, as the detail route does for the same asset.
func TestFillDeclaredPegPricesInListing_NamesPriceBasis(t *testing.T) {
	circ := "10000000000"
	peg := pegTestServer(t, &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
	}})
	global := globalMarketServer(t, refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now().Add(-5*time.Minute)))
	for _, tc := range []struct {
		name  string
		s     *Server
		row   AssetDetail
		basis string
	}{
		{"declared peg", peg, AssetDetail{AssetID: pegTestAUDD, Code: "AUDD", CirculatingSupply: &circ}, priceBasisDeclaredPeg},
		{"global market", global, AssetDetail{AssetID: globalTestUSDC, Code: "USDC", CirculatingSupply: &circ}, priceBasisGlobalMarket},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []AssetDetail{tc.row}
			tc.s.fillDeclaredPegPricesInListing(context.Background(), rows)
			if rows[0].PriceUSD == nil || rows[0].PriceBasis != tc.basis {
				t.Fatalf("price_usd = %v basis = %q, want the %s fill (the case under test)", rows[0].PriceUSD, rows[0].PriceBasis, tc.basis)
			}
			if got := wireCapReason(t, rows[0]); got != "price_basis" || rows[0].MarketCapUSD != nil {
				t.Errorf("market_cap_withheld_reason = %q market_cap_usd = %q, want price_basis and no cap", got, strOrEmpty(rows[0].MarketCapUSD))
			}
		})
	}
}

// TestMergeTwinStats_CapReasonIsTrueForTheRow — a catalogue row takes its twin's reasons only where they hold for the row: a
// thin twin does not make a global-tier price thin, and a twin's withheld price names the row's missing cap.
func TestMergeTwinStats_CapReasonIsTrueForTheRow(t *testing.T) {
	price, circ := "1.00", "10000000"
	for _, tc := range []struct {
		name      string
		twin      AssetDetail
		dstThin   bool
		want      string
		wantLowLq bool
	}{
		{name: "thin twin, row not thin", twin: AssetDetail{ThinMarket: true, MarketCapWithheldReason: MarketCapWithheldThinMarket}, want: ""},
		{name: "thin twin, row thin", twin: AssetDetail{ThinMarket: true, MarketCapWithheldReason: MarketCapWithheldThinMarket}, dstThin: true, want: "thin_market"},
		{name: "twin withheld for substance", twin: AssetDetail{PriceWithheldReason: PriceWithheldSubstance}, want: "substance", wantLowLq: true},
		{name: "twin withheld upstream", twin: AssetDetail{PriceWithheldReason: PriceWithheldUpstreamLeg}, want: "upstream_leg"},
		{name: "twin withheld by the manipulation guard", twin: AssetDetail{PriceWithheldReason: PriceWithheldManipulationGuard}, want: "manipulation_guard"},
		{name: "twin withheld for the FX leg", twin: AssetDetail{PriceWithheldReason: PriceWithheldFXLeg}, want: "fx_leg_unavailable"},
		{name: "unattributed twin names nothing", twin: AssetDetail{PriceWithheldReason: PriceWithheldUnattributed}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := AssetDetail{Type: "global", PriceUSD: &price, CirculatingSupply: &circ, ThinMarket: tc.dstThin}
			mergeTwinStats(&dst, tc.twin)
			flagTwinSubstanceWithheldCap(&dst, tc.twin)
			if got := wireCapReason(t, dst); got != tc.want {
				t.Errorf("market_cap_withheld_reason = %q, want %q", got, tc.want)
			}
			if dst.MarketCapLowLiquidity != tc.wantLowLq || dst.MarketCapUSD != nil {
				t.Errorf("market_cap_low_liquidity = %v market_cap_usd = %q, want %v and no cap", dst.MarketCapLowLiquidity, strOrEmpty(dst.MarketCapUSD), tc.wantLowLq)
			}
		})
	}
}

// TestMarketCapWithheldReason_GuardsUnchanged — the reason is additive: on every guard the detail and listing fills serve the
// cap, FDV and booleans the guard tests pin, whatever reason they name.
func TestMarketCapWithheldReason_GuardsUnchanged(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	direct := capTestAsset + "/" + defaultPriceQuote.String()
	vol := func(v string) func(*AssetDetail) { return func(d *AssetDetail) { d.VolumeUSD24h = &v } }
	stale := capTestSupply()
	stale.ObservedAt = time.Now().Add(-preciseSupplyMaxAge - time.Hour)
	held := capTestSupply()
	held.Basis = supply.BasisContractStorageBalances
	thin := func(d *AssetDetail) { p := "0.0767"; d.PriceUSD, d.ThinMarket = &p, true }
	type want struct {
		cap, fdv                    string
		lowLq, mismatch, lowerBound bool
	}
	for _, tc := range []struct {
		name    string
		reader  withheldUSDReader
		snap    supply.Supply
		assetID string
		floor   float64
		between func(*AssetDetail)
		want    want
	}{
		{name: "cap served", want: want{cap: "767000.00", fdv: "1534000.00"}},
		{name: "within the turnover ceiling", floor: 1000, between: vol("20000"), want: want{cap: "767000.00", fdv: "1534000.00"}},
		{name: "FDV alone above the turnover ceiling", floor: 1000, between: vol("10000"), want: want{cap: "767000.00", lowLq: true}},
		{name: "dust floor", floor: 1000, between: vol("100"), want: want{lowLq: true}},
		{name: "turnover ceiling", between: vol("100"), want: want{lowLq: true}},
		{name: "decimals mismatch", between: func(d *AssetDetail) { d.MarketCapDecimalsMismatch = true }, want: want{mismatch: true}},
		{name: "unknown scale", between: func(d *AssetDetail) { d.DecimalsUndeclared = true }},
		{name: "ticker collision", assetID: capTestImpostor},
		{name: "stale supply", snap: stale},
		{name: "held-balances supply", snap: held},
		{name: "thin price", between: thin},
		{name: "direct price withheld", reader: withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldSubstance)}}},
		{name: "direct price read failed", reader: withheldUSDReader{direct: {err: errors.New("price store unreachable")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.snap.CirculatingSupply == nil {
				tc.snap = capTestSupply()
			}
			if tc.assetID == "" {
				tc.assetID = capTestAsset
			}
			s := capTestServer(tc.reader, tc.snap)
			s.VerifiedCurrencies = cat
			s.MinMarketCapVolumeUSD = tc.floor
			d := capTestRunAsset(t, s, tc.assetID, false, tc.between)
			nameLateCapRefusal(&d)
			got := want{strOrEmpty(d.MarketCapUSD), strOrEmpty(d.FDVUSD), d.MarketCapLowLiquidity, d.MarketCapDecimalsMismatch, d.MarketCapLowerBound}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	const asset = "FOO-GAFOO"
	precise := observedSupply(map[string]string{asset: "10000000000"})
	for _, tc := range []struct {
		name string
		row  func(*AssetDetail)
		s    *Server
		want want
	}{
		{"listing dust floor", func(r *AssetDetail) { v := "1"; r.VolumeUSD24h = &v }, &Server{Options: Options{MinMarketCapVolumeUSD: 1000}}, want{lowLq: true}},
		{"listing turnover ceiling", func(r *AssetDetail) { v := "1"; r.VolumeUSD24h = &v }, &Server{Options: Options{MaxMarketCapVolumeRatio: 10}}, want{lowLq: true}},
		{"listing ticker collision", func(r *AssetDetail) { r.UnverifiedTickerCollision = true }, &Server{}, want{}},
		{"listing decimals mismatch", func(r *AssetDetail) { r.MarketCapDecimalsMismatch = true }, &Server{}, want{mismatch: true}},
		{"listing thin price", func(r *AssetDetail) { r.ThinMarket = true }, &Server{}, want{}},
		{"listing cap served", func(*AssetDetail) {}, &Server{}, want{cap: "1000.00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price := "1.00"
			row := AssetDetail{AssetID: asset, Code: "FOO", Decimals: 7, PriceUSD: &price}
			tc.row(&row)
			tc.s.fillRowMarketCap(context.Background(), &row, nil, precise, nil, nil, map[string]int{asset: 1})
			tc.s.fillDeclaredPegPricesInListing(context.Background(), []AssetDetail{row})
			got := want{strOrEmpty(row.MarketCapUSD), strOrEmpty(row.FDVUSD), row.MarketCapLowLiquidity, row.MarketCapDecimalsMismatch, row.MarketCapLowerBound}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
