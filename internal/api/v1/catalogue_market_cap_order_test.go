// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

const (
	rankUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	rankEURCIssuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
)

// rankedTwinAssets serves on-chain twins for two stablecoin catalogue
// entries. USDC precedes EURC in the catalogue's seed order; EURC is given
// the larger supply, so a market-cap ranking must put it first.
type rankedTwinAssets struct {
	stubAssetsReaderExt
}

// rankedTwins maps issuer → (asset_id, supply at 7dp). Both are priced at
// 1.00 on deep multi-venue volume, so the cap is the supply and no dust
// guard is in play.
var rankedTwins = map[string][2]string{
	rankUSDCIssuer: {"USDC-" + rankUSDCIssuer, "10000000000000"},  // 1,000,000 → $1.00M
	rankEURCIssuer: {"EURC-" + rankEURCIssuer, "900000000000000"}, // 90,000,000 → $90.00M
}

func rankedTwinRow(issuer string) (timescale.AssetRow, bool) {
	tw, ok := rankedTwins[issuer]
	if !ok {
		return timescale.AssetRow{}, false
	}
	price, vol, sources := "1.00", "500000000", 4
	return timescale.AssetRow{
		AssetID:       tw[0],
		IssuerGStrkey: issuer,
		PriceUSD:      &price,
		Volume24hUSD:  &vol,
		SourceCount:   &sources,
	}, true
}

func (s *rankedTwinAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	row, ok := rankedTwinRow(opts.Issuer)
	if !ok {
		return nil, nil
	}
	return []timescale.AssetRow{row}, nil
}

func (s *rankedTwinAssets) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	for issuer, tw := range rankedTwins {
		if tw[0] == assetID {
			row, _ := rankedTwinRow(issuer)
			return row, nil
		}
	}
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (s *rankedTwinAssets) LatestSupplyObservations(
	context.Context, time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	out := map[string]timescale.SupplyObservation{}
	for _, tw := range rankedTwins {
		out[tw[0]] = timescale.SupplyObservation{
			CirculatingSupply: tw[1],
			Basis:             string(supply.BasisIssuerExclusion),
			ObservedAt:        time.Now(),
		}
	}
	return out, nil
}

type rankedRow struct {
	Slug         string  `json:"slug"`
	MarketCapUSD *string `json:"market_cap_usd"`
}

func rankedPage(t *testing.T, ts *testServer, path string) []rankedRow {
	t.Helper()
	resp := mustGet(t, ts.URL+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	var env struct {
		Data []rankedRow `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return env.Data
}

// TestCatalogueListingRanksOnTheFilledMarketCap pins the asset_class
// listing's market-cap order to the caps it publishes. The ranking used to
// run on caps that are nil for every Stellar-issued row (only fiat rows get a
// catalogue-level cap) and before the page was sliced and filled, so rows
// came back in seed order and a large asset seeded late was unreachable on a
// short page.
func TestCatalogueListingRanksOnTheFilledMarketCap(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       &rankedTwinAssets{},
		VerifiedCurrencies: cat,
	}))

	full := rankedPage(t, ts, "/v1/assets?asset_class=stablecoin&limit=100")
	if len(full) < 2 {
		t.Fatalf("want every stablecoin catalogue row, got %d", len(full))
	}
	for i, want := range []struct{ slug, cap string }{{"eurc", "90000000.00"}, {"usdc", "1000000.00"}} {
		got := full[i]
		if got.Slug != want.slug || got.MarketCapUSD == nil || *got.MarketCapUSD != want.cap {
			t.Fatalf("row %d = %s cap=%v, want %s cap=%s — the listing is described as market-cap "+
				"ordered and must rank on the cap it serves; page=%+v", i, got.Slug, deref(got.MarketCapUSD),
				want.slug, want.cap, full)
		}
	}
	for _, r := range full[2:] {
		if r.MarketCapUSD != nil {
			t.Errorf("row %s carries cap %s below the capped rows", r.Slug, *r.MarketCapUSD)
		}
	}

	// The rank is decided before the slice, so the largest asset heads
	// page 1 however short the page is.
	first := rankedPage(t, ts, "/v1/assets?asset_class=stablecoin&limit=1")
	if len(first) != 1 || first[0].Slug != "eurc" {
		t.Fatalf("limit=1 page = %+v, want [eurc] (the largest market cap in the class)", first)
	}
}
