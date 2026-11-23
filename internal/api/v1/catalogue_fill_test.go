// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listingOnlyAssets serves change fields ONLY via ListAssetsExt — the
// production shape (the per-asset reader's row carries nil changes),
// which a live debug showed when enrichment merged nothing.
type listingOnlyAssets struct {
	AssetsReader
}

func (s *listingOnlyAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	// Mimic the REAL SQL semantics that broke the first two attempts:
	// Q is a substring match over code/slug/issuer COLUMN VALUES — a
	// full asset id can never match. Only the exact Issuer filter
	// returns rows.
	if opts.Issuer != "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
		return nil, nil
	}
	ch, vol := "1.23", "42.00"
	return []timescale.AssetRow{{
		AssetID:      "USDC-" + opts.Issuer,
		Code:         "USDC",
		Change24hPct: &ch,
		Volume24hUSD: &vol,
	}}, nil
}

func (s *listingOnlyAssets) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: assetID}, nil // nil changes — production shape
}

// TestCatalogueStatsUseListingReader pins the catalogue enrichment to the
// listing reader: catalogue rows must gain the twin's change/volume.
func TestCatalogueStatsUseListingReader(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Options: Options{AssetsReader: &listingOnlyAssets{}, VerifiedCurrencies: cat}}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	s.fillCatalogueStatsForPage(req.Context(), page, assetListFilters{})
	if page[0].Change24hPct == nil || *page[0].Change24hPct != "1.23" {
		t.Fatalf("catalogue row did not absorb the twin's change: %+v", page[0].Change24hPct)
	}
	if page[0].VolumeUSD24h == nil {
		t.Fatal("volume not merged")
	}
}

// failingSupplyAssets is listingOnlyAssets whose supply observer read fails, counted.
type failingSupplyAssets struct {
	listingOnlyAssets
	supplyReads atomic.Int32
}

func (f *failingSupplyAssets) LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error) {
	f.supplyReads.Add(1)
	return nil, errors.New("pool timeout")
}

// pricedTwinAssets is listingOnlyAssets whose twin carries an on-chain price (nil for none) and a precise supply.
type pricedTwinAssets struct {
	listingOnlyAssets
	price *string
}

func (p *pricedTwinAssets) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	rows, err := p.listingOnlyAssets.ListAssetsExt(ctx, opts)
	for i := range rows {
		rows[i].PriceUSD = p.price
	}
	return rows, err
}

func (p *pricedTwinAssets) LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error) {
	return map[string]timescale.SupplyObservation{
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": {CirculatingSupply: "1000000000", Basis: "issuer_exclusion"},
	}, nil
}

// A catalogue row's cap is its own served price times its own supply: never the twin's on-chain price beside the row's global
// price. With no on-chain price the supply still lands, but the row's whole-token price values no cap (price_basis), as on the
// asset detail for the same issuance.
func TestCatalogueStats_CapIsTheRowsOwnPriceTimesSupply(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	onChain := "0.5"
	for name, twinPrice := range map[string]*string{"twin priced": &onChain, "twin unpriced": nil} {
		t.Run(name, func(t *testing.T) {
			s := &Server{Options: Options{AssetsReader: &pricedTwinAssets{price: twinPrice}, VerifiedCurrencies: cat}}
			rowPrice := "2"
			page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc", PriceUSD: &rowPrice}}
			s.fillCatalogueStatsForPage(context.Background(), page, assetListFilters{typ: "classic"})
			if page[0].CirculatingSupply == nil || *page[0].CirculatingSupply != "1000000000" {
				t.Fatalf("circulating_supply = %v, want the twin's 1000000000", page[0].CirculatingSupply)
			}
			if twinPrice == nil {
				if page[0].MarketCapUSD != nil || page[0].MarketCapWithheldReason != MarketCapWithheldPriceBasis {
					t.Errorf("market_cap_usd = %v reason %q, want null with price_basis", page[0].MarketCapUSD, page[0].MarketCapWithheldReason)
				}
			} else if page[0].MarketCapUSD == nil || !decimalEqual(*page[0].MarketCapUSD, "200") {
				// 1000000000 stroops = 100 USDC at the row's own $2.
				t.Errorf("market_cap_usd = %v, want 200 (row price 2 x supply 100)", page[0].MarketCapUSD)
			}
			if page[0].PriceUSD == nil || *page[0].PriceUSD != "2" {
				t.Errorf("row price changed to %v", page[0].PriceUSD)
			}
		})
	}
}

// The twin's gate outcome rules the catalogue row's cap: a refused or unmeasured on-chain price backs no cap at the row's price.
func TestCatalogueStats_TwinGateOutcomeRulesTheCap(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	onChain, rowPrice := "0.5", "2"
	cases := map[string]struct {
		gate           PriceSubstanceGate
		wantUnmeasured bool
		wantLowLiq     bool
	}{
		"unmeasured twin":        {gate: &unmeasuredGate{}, wantUnmeasured: true},
		"measured-withheld twin": {gate: &stubListingGate{}, wantLowLiq: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &Server{Options: Options{AssetsReader: &pricedTwinAssets{price: &onChain}, VerifiedCurrencies: cat, Substance: tc.gate}}
			page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc", PriceUSD: &rowPrice}}
			unmeasured := s.fillCatalogueStatsForPage(context.Background(), page, assetListFilters{typ: "classic"})
			if page[0].MarketCapUSD != nil {
				t.Errorf("market_cap_usd = %s, want null: the twin's price did not clear the gate", *page[0].MarketCapUSD)
			}
			if page[0].CirculatingSupply == nil || *page[0].CirculatingSupply != "1000000000" {
				t.Errorf("circulating_supply = %v, want the twin's 1000000000", page[0].CirculatingSupply)
			}
			if unmeasured != tc.wantUnmeasured {
				t.Errorf("page unmeasured = %v, want %v", unmeasured, tc.wantUnmeasured)
			}
			if page[0].MarketCapLowLiquidity != tc.wantLowLiq {
				t.Errorf("market_cap_low_liquidity = %v, want %v", page[0].MarketCapLowLiquidity, tc.wantLowLiq)
			}
			if page[0].PriceUSD == nil || *page[0].PriceUSD != rowPrice {
				t.Errorf("row price changed to %v", page[0].PriceUSD)
			}
		})
	}
}

// With no row price the twin's on-chain price must not stand in: no cap, the supply still lands.
func TestCatalogueStats_NoRowPriceNoCap(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	onChain := "0.5"
	s := &Server{Options: Options{AssetsReader: &pricedTwinAssets{price: &onChain}, VerifiedCurrencies: cat}}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	s.fillCatalogueStatsForPage(context.Background(), page, assetListFilters{typ: "classic"})
	if page[0].MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %s, want null with no row price", *page[0].MarketCapUSD)
	}
	if page[0].CirculatingSupply == nil || *page[0].CirculatingSupply != "1000000000" {
		t.Errorf("circulating_supply = %v, want 1000000000", page[0].CirculatingSupply)
	}
}

// The valuations after the cap fill rule on the twin's own on-chain price, so it must be back on the twin.
func TestFillTwinCapAtRowPrice_RestoresTheTwinsOwnPrice(t *testing.T) {
	onChain, rowPrice := "0.5", "2"
	reader := &pricedTwinAssets{price: &onChain}
	s := &Server{Options: Options{AssetsReader: reader}}
	precise, _ := reader.LatestSupplyObservations(context.Background(), 0)
	twin := []AssetDetail{{AssetID: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Type: "classic", Decimals: 7, PriceUSD: &onChain}}
	s.fillTwinCapAtRowPrice(context.Background(), twin, &rowPrice, nil, precise)
	if twin[0].PriceUSD == nil || *twin[0].PriceUSD != onChain {
		t.Errorf("twin price = %v after the cap fill, want its own %s", twin[0].PriceUSD, onChain)
	}
	if twin[0].MarketCapUSD == nil || !decimalEqual(*twin[0].MarketCapUSD, "200") {
		t.Errorf("market_cap_usd = %v, want 200 at the row's price", twin[0].MarketCapUSD)
	}
}

// An unpriced, ungated row on the main listing path still gets its supply: supply is a fact about the token.
func TestFillMarketCapsFromSupply_UnpricedRowGetsSupply(t *testing.T) {
	s := &Server{Options: Options{AssetsReader: &pricedTwinAssets{}}}
	rows := []AssetDetail{{AssetID: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Type: "classic", Decimals: 7}}
	s.fillMarketCapsFromSupply(context.Background(), rows, nil)
	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != "1000000000" {
		t.Errorf("circulating_supply = %v, want 1000000000", rows[0].CirculatingSupply)
	}
	if rows[0].MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %s, want null with no price", *rows[0].MarketCapUSD)
	}
}

// A twin with no on-chain price is not valued at the row's per-whole-token price, so no scale drift is claimed for it; a twin
// with its own price keeps the lockstep refusal.
func TestFillTwinCapAtRowPrice_DecimalsDriftNeedsTheTwinsOwnPrice(t *testing.T) {
	const contract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	onChain, rowPrice := "0.5", "2"
	precise := map[string]timescale.SupplyObservation{contract: {CirculatingSupply: "100000000", Basis: "issuer_exclusion"}}
	for name, tc := range map[string]struct {
		own          *string
		wantMismatch bool
		wantCap      string
	}{
		"twin unpriced": {own: nil},
		"twin priced":   {own: &onChain, wantMismatch: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{Options: Options{AssetsReader: &listingOnlyAssets{}, TokenDecimals: fixedTokenDecimals(6)}}
			twin := []AssetDetail{{AssetID: contract, Type: "soroban", Decimals: 7, PriceUSD: tc.own}}
			s.fillTwinCapAtRowPrice(context.Background(), twin, &rowPrice, nil, precise)
			if twin[0].MarketCapDecimalsMismatch != tc.wantMismatch {
				t.Errorf("market_cap_decimals_mismatch = %v, want %v", twin[0].MarketCapDecimalsMismatch, tc.wantMismatch)
			}
			got := twin[0].MarketCapUSD
			if tc.wantCap == "" {
				if got != nil {
					t.Errorf("market_cap_usd = %s, want null", *got)
				}
			} else if got == nil || !decimalEqual(*got, tc.wantCap) {
				// 100000000 at 6 decimals = 100 tokens at the row's $2.
				t.Errorf("market_cap_usd = %v, want %s", got, tc.wantCap)
			}
		})
	}
}

// decimalEqual compares two decimal strings by value.
func decimalEqual(a, b string) bool {
	ar, ok1 := new(big.Rat).SetString(a)
	br, ok2 := new(big.Rat).SetString(b)
	return ok1 && ok2 && ar.Cmp(br) == 0
}

// TestCatalogueStatsReadPreciseSupplyOncePerPage: a failing observer read marks the page and is not repeated per entry.
func TestCatalogueStatsReadPreciseSupplyOncePerPage(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	reader := &failingSupplyAssets{}
	s := unreadTestServer(Options{AssetsReader: reader, VerifiedCurrencies: cat})
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}, {Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	if !s.fillCatalogueStatsForPage(context.Background(), page, assetListFilters{}) {
		t.Error("supply read failed: page reported measured")
	}
	if n := reader.supplyReads.Load(); n != 1 {
		t.Errorf("supply observer read %d times for a two-entry page, want 1", n)
	}
}
