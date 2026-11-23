package v1_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type globalExclBody struct {
	Data  json.RawMessage `json:"data"`
	Flags v1.Flags        `json:"flags"`
}

// globalExclGet decodes path's data into data; degraded is read off Cache-Control, the only place a Degraded flag reaches the wire.
func globalExclGet(t *testing.T, srv *v1.Server, path string, data any) (stale, degraded bool) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s, want 200", path, resp.StatusCode, body)
	}
	var env globalExclBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: decode %v: %s", path, err, body)
	}
	if err := json.Unmarshal(env.Data, data); err != nil {
		t.Fatalf("%s: decode data %v: %s", path, err, body)
	}
	return env.Flags.Stale, resp.Header.Get("Cache-Control") == "no-store"
}

// TestGlobalAssetFXSourceExclusionWithholdsFiatRate: the fiat headline and every market cap built on it (the slug view and the
// verified listing) are judged on the FX feed that published the rate. An excluded feed's rate is withheld with its reason and
// is not replaced by the PriceReader fallback; an unreadable exclusion set withholds as a transient body.
func TestGlobalAssetFXSourceExclusionWithholdsFiatRate(t *testing.T) {
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: "1.17", Source: "ecb"},
	}}
	// A clean fallback the withheld FX reading must not be swapped for.
	prices := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
		"fiat:EUR/fiat:USD": {AssetID: "fiat:EUR", Quote: "fiat:USD", Price: "1.20000000000000", PriceType: "vwap"},
	}}
	gate := func(e exclTable) v1.PriceExclusionGate {
		return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
	}
	from := time.Now().Add(-72 * time.Hour)
	newSrv := func(g v1.PriceExclusionGate) *v1.Server {
		return v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t), FXHistory: fx, Prices: prices, Exclusions: g})
	}
	euroCap := func(rows []v1.VerifiedCurrencyListItem, slug string) string {
		for _, r := range rows {
			if r.Slug == slug {
				return r.MarketCapUSD
			}
		}
		t.Fatalf("verified listing has no %s row", slug)
		return ""
	}

	cases := []struct {
		name       string
		gate       v1.PriceExclusionGate
		wantPrice  string
		wantReason v1.PriceWithheldReason
		wantStale  bool
	}{
		{name: "no gate", wantPrice: "1.17"},
		{name: "other feed excluded", wantPrice: "1.17", gate: gate(exclTable{rows: []timescale.SourceExclusion{
			{ID: 1, Source: "massive", Base: "fiat:EUR", Quote: "fiat:USD", From: from},
		}})},
		{name: "publishing feed excluded", wantReason: v1.PriceWithheldSourceExcluded, gate: gate(exclTable{rows: []timescale.SourceExclusion{
			{ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: from},
		}})},
		{name: "exclusions unread", wantReason: v1.PriceWithheldUnattributed, wantStale: true, gate: gate(exclTable{err: errors.New("db down")})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSrv(tc.gate)
			var view v1.GlobalAssetView
			stale, degraded := globalExclGet(t, srv, "/v1/external/assets/euro", &view)
			if tc.wantPrice != "" {
				if view.PriceUSD == nil || *view.PriceUSD != tc.wantPrice || view.MarketCapUSD == nil || view.PriceWithheldReason != "" {
					t.Errorf("view price=%v cap=%v reason=%q, want %s with a market cap", view.PriceUSD, view.MarketCapUSD,
						view.PriceWithheldReason, tc.wantPrice)
				}
			} else if view.PriceUSD != nil || view.MarketCapUSD != nil || view.PriceSources != nil || view.PriceWithheldReason != tc.wantReason {
				t.Errorf("view price=%v cap=%v sources=%v reason=%q, want withheld as %q", view.PriceUSD, view.MarketCapUSD,
					view.PriceSources, view.PriceWithheldReason, tc.wantReason)
			}
			// A withheld rate is otherwise the same null as a missing one, so it is degraded (no-store) whatever the reason.
			if withheld := tc.wantReason != ""; stale != tc.wantStale || degraded != withheld {
				t.Errorf("view stale=%v degraded=%v, want stale=%v degraded=%v", stale, degraded, tc.wantStale, withheld)
			}

			// The listing's way-out judge also withholds this row under the other feed's exclusion (fail closed), so that case
			// is not asserted here.
			var listed []map[string]any
			_, degraded = globalExclGet(t, srv, "/v1/external/assets?asset_class=fiat", &listed)
			if tc.gate == nil || tc.wantReason != "" {
				for _, row := range listed {
					if row["slug"] == "euro" && (row["market_cap_usd"] != nil) != (tc.wantPrice != "") {
						t.Errorf("external listing euro market_cap_usd = %v, want served only when the rate is", row["market_cap_usd"])
					}
				}
				if withheld := tc.wantReason != ""; degraded != withheld {
					t.Errorf("external listing degraded=%v, want %v", degraded, withheld)
				}
			}

			var rows []v1.VerifiedCurrencyListItem
			stale, degraded = globalExclGet(t, srv, "/v1/assets/verified", &rows)
			if got := euroCap(rows, "euro"); (got != "") != (tc.wantPrice != "") {
				t.Errorf("verified euro market_cap_usd = %q, want served only when the rate is", got)
			}
			if tc.wantReason == v1.PriceWithheldSourceExcluded && euroCap(rows, "chinese-yuan") == "" {
				t.Error("verified chinese-yuan market_cap_usd withheld by an exclusion scoped to EUR")
			}
			if withheld := tc.wantReason != ""; degraded != withheld || stale != tc.wantStale {
				t.Errorf("verified degraded=%v stale=%v, want degraded=%v stale=%v", degraded, stale, withheld, tc.wantStale)
			}
		})
	}
}
