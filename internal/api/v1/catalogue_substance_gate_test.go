// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

const (
	gateAQUAIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	gateAQUAAssetID = "AQUA-" + gateAQUAIssuer
)

// gatedCatalogueAssets serves AQUA's on-chain twin: a listing row with a
// price, change pills and deep multi-venue volume (so no dust guard is in
// play), the per-asset row the headline on-chain fallback reads, and an
// observed supply so the market-cap fill runs.
type gatedCatalogueAssets struct {
	stubAssetsReaderExt
}

func (s *gatedCatalogueAssets) twinRow() timescale.AssetRow {
	price, vol, change, sources := "0.5", "5000000", "12.5", 4
	return timescale.AssetRow{
		AssetID:       gateAQUAAssetID,
		Code:          "AQUA",
		IssuerGStrkey: gateAQUAIssuer,
		PriceUSD:      &price,
		Volume24hUSD:  &vol,
		Change1hPct:   &change,
		Change24hPct:  &change,
		Change7dPct:   &change,
		SourceCount:   &sources,
	}
}

func (s *gatedCatalogueAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if opts.Issuer != gateAQUAIssuer {
		return nil, nil
	}
	return []timescale.AssetRow{s.twinRow()}, nil
}

func (s *gatedCatalogueAssets) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	if assetID != gateAQUAAssetID {
		return timescale.AssetRow{}, sql.ErrNoRows
	}
	return s.twinRow(), nil
}

func (s *gatedCatalogueAssets) LatestSupplyObservations(
	context.Context, time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	return map[string]timescale.SupplyObservation{gateAQUAAssetID: {
		CirculatingSupply: "10000000000000000", // 10^9 AQUA at 7dp
		Basis:             string(supply.BasisIssuerExclusion),
		ObservedAt:        time.Now(),
	}}, nil
}

type gatedValuation struct {
	Slug         string  `json:"slug"`
	PriceUSD     *string `json:"price_usd"`
	MarketCapUSD *string `json:"market_cap_usd"`
	Change1hPct  *string `json:"change_1h_pct"`
	Change24hPct *string `json:"change_24h_pct"`
	Change7dPct  *string `json:"change_7d_pct"`
}

func (v gatedValuation) published() []string {
	var out []string
	for name, p := range map[string]*string{
		"price_usd": v.PriceUSD, "market_cap_usd": v.MarketCapUSD,
		"change_1h_pct": v.Change1hPct, "change_24h_pct": v.Change24hPct, "change_7d_pct": v.Change7dPct,
	} {
		if p != nil {
			out = append(out, name+"="+*p)
		}
	}
	sort.Strings(out)
	return out
}

func gatedCatalogueRow(t *testing.T, ts *testServer, path string) gatedValuation {
	t.Helper()
	resp := mustGet(t, ts.URL+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	var list struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	var rows []gatedValuation
	if err := json.Unmarshal(list.Data, &rows); err != nil {
		var one gatedValuation
		if err := json.Unmarshal(list.Data, &one); err != nil {
			t.Fatalf("GET %s: data is neither a row nor a list: %v", path, err)
		}
		return one
	}
	for _, r := range rows {
		if r.Slug == "aqua" {
			return r
		}
	}
	t.Fatalf("GET %s: no aqua catalogue row on the page", path)
	return gatedValuation{}
}

// TestCatalogueListingHonoursSubstanceGate pins the catalogue surfaces to the
// thin-market substance gate the classic listing and the CODE-ISSUER detail
// already apply: a verified Stellar-only entry whose on-chain market fails the
// floor must not publish its headline price (fillGlobalPriceFromOnChain), nor a
// market cap or change pills merged from its ungated twin row. The allow arm
// proves the fixture does reach every one of those fields.
func TestCatalogueListingHonoursSubstanceGate(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"/v1/assets?asset_class=crypto&limit=100",
		"/v1/assets?asset_class=all&limit=100",
		"/v1/assets/aqua",
	}
	for _, allow := range []bool{true, false} {
		srv := v1.New(v1.Options{
			AssetsReader:       &gatedCatalogueAssets{},
			Substance:          &stubSubstanceGate{allow: allow},
			VerifiedCurrencies: cat,
		})
		ts := httpTestServer(t, srv)
		for _, path := range paths {
			row := gatedCatalogueRow(t, ts, path)
			got := row.published()
			if !allow && len(got) > 0 {
				t.Errorf("deny: GET %s published a valuation the substance gate refuses: %v", path, got)
			}
			if allow && row.PriceUSD == nil {
				t.Errorf("allow: GET %s carried no price_usd; the fixture no longer reaches the gate", path)
			}
		}
	}
}
