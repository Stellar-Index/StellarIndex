// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			nameLateCapRefusal(&tc.d)
			if got := wireCapReason(t, tc.d); got != tc.want {
				t.Errorf("market_cap_withheld_reason = %q, want %q", got, tc.want)
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
		{"decimals mismatch", func(r *AssetDetail) { r.MarketCapDecimalsMismatch = true }, &Server{}, "decimals_mismatch"},
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
