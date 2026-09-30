// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
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

// tickingTwinAssets is rankedTwinAssets whose caps can swap order between
// requests, standing in for an FX or price tick between two page reads.
type tickingTwinAssets struct {
	rankedTwinAssets
	swapped atomic.Bool
}

func (s *tickingTwinAssets) LatestSupplyObservations(
	ctx context.Context, window time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	out, err := s.rankedTwinAssets.LatestSupplyObservations(ctx, window)
	if s.swapped.Load() {
		usdc, eurc := rankedTwins[rankUSDCIssuer][0], rankedTwins[rankEURCIssuer][0]
		out[usdc], out[eurc] = out[eurc], out[usdc]
	}
	return out, err
}

// TestCataloguePagesSurviveARerank walks the class listing one row per page
// with the caps re-ordering after page 1. Every row must be served exactly
// once: the cursor has to name what was served, not a position in a list
// that is re-ranked on every request.
func TestCataloguePagesSurviveARerank(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	assets := &tickingTwinAssets{}
	ts := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       assets,
		VerifiedCurrencies: cat,
	}))

	want := len(rankedPage(t, ts, "/v1/assets?asset_class=stablecoin&limit=100"))
	order := walkCataloguePages(t, ts, "/v1/assets?asset_class=stablecoin&limit=1", &assets.swapped)
	assertEachServedOnceEURCFirst(t, order, want)
}

// TestUnifiedCataloguePagesSurviveARerank is the same walk over the unified
// listing's catalogue phase, whose `catalogue:` cursor must name the served
// rows too.
func TestUnifiedCataloguePagesSurviveARerank(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	baseline := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       &rankedTwinAssets{},
		VerifiedCurrencies: cat,
	}))
	var never atomic.Bool
	want := len(walkCataloguePages(t, baseline, "/v1/assets?asset_class=all&limit=1", &never))

	assets := &tickingTwinAssets{}
	ts := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       assets,
		VerifiedCurrencies: cat,
	}))
	order := walkCataloguePages(t, ts, "/v1/assets?asset_class=all&limit=1", &assets.swapped)
	assertEachServedOnceEURCFirst(t, order, want)
}

// walkCataloguePages follows pagination.next from path until the listing
// ends or leaves the catalogue phase, setting tick after page 1.
func walkCataloguePages(t *testing.T, ts *testServer, path string, tick *atomic.Bool) []string {
	t.Helper()
	var order []string
	cursor := ""
	for page := 0; page < 500; page++ {
		resp := mustGet(t, ts.URL+path+"&cursor="+url.QueryEscape(cursor))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: status %d", page, resp.StatusCode)
		}
		var env struct {
			Data       []rankedRow `json:"data"`
			Pagination *struct {
				Next string `json:"next"`
			} `json:"pagination"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, r := range env.Data {
			order = append(order, r.Slug)
		}
		tick.Store(true)
		if env.Pagination == nil || env.Pagination.Next == "" || strings.HasPrefix(env.Pagination.Next, "classic:") {
			return order
		}
		cursor = env.Pagination.Next
	}
	t.Fatalf("walk of %s did not end: %v", path, order)
	return nil
}

func assertEachServedOnceEURCFirst(t *testing.T, order []string, want int) {
	t.Helper()
	seen := map[string]int{}
	for _, slug := range order {
		seen[slug]++
	}
	if len(order) != want || len(seen) != want {
		t.Fatalf("walked %d rows (%d distinct), want each of %d once: %v", len(order), len(seen), want, order)
	}
	if order[0] != "eurc" || order[1] != "usdc" {
		t.Errorf("order = %v, want eurc (largest on page 1) then usdc (largest of the rest after the tick)", order)
	}
}

func TestCatalogueListingRejectsAForeignCursor(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       &rankedTwinAssets{},
		VerifiedCurrencies: cat,
	}))
	for _, cursor := range []string{"2", "not-a-cursor", "W10"} { // W10 = base64url("[]")
		resp := mustGet(t, ts.URL+"/v1/assets?asset_class=stablecoin&limit=1&cursor="+cursor)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cursor %q: status %d, want 400", cursor, resp.StatusCode)
		}
	}
	for _, cursor := range []string{"catalogue:2", "catalogue:abc", "catalogue:W10"} {
		resp := mustGet(t, ts.URL+"/v1/assets?asset_class=all&limit=1&cursor="+cursor)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cursor %q: status %d, want 400", cursor, resp.StatusCode)
		}
	}
}
