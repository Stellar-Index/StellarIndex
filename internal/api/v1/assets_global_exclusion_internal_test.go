package v1

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// withheldTierReader refuses tier 1 with err while refusing is set, then serves a clean VWAP.
type withheldTierReader struct {
	err      error
	refusing atomic.Bool
}

func newWithheldTierReader(err error) *withheldTierReader {
	r := &withheldTierReader{err: err}
	r.refusing.Store(true)
	return r
}

func (r *withheldTierReader) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	if r.refusing.Load() {
		return "", time.Time{}, 0, nil, false, r.err
	}
	return "0.9998", time.Now().Add(-time.Minute), 100, []string{"kraken"}, true, nil
}

func (r *withheldTierReader) LatestAggregatorPrices(context.Context, canonical.Asset, canonical.Asset, []string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (r *withheldTierReader) LookupTriangulated(context.Context, canonical.Asset, canonical.Asset, time.Duration) (string, time.Time, bool, error) {
	return "", time.Time{}, false, nil
}

// TestGlobalAssetHeadline_ExcludedTwinIsNotServed: the twin's pooled on-chain price is judged against the exclusions on the
// twin's own markets and on the XLM/USD anchor, so the slug view and the catalogue row withhold what /v1/assets/{asset_id} withholds,
// with the reason and without the twin's change pills.
func TestGlobalAssetHeadline_ExcludedTwinIsNotServed(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	vc, ok := cat.LookupBySlug("usdc")
	if !ok || vc.StellarEntry() == nil {
		t.Fatal("embedded catalogue has no usdc Stellar issuance")
	}
	twin := vc.StellarEntry().AssetID
	from := time.Now().Add(-time.Hour)
	listing := "0.4321"
	for _, tc := range []struct {
		name string
		rows []timescale.SourceExclusion
		want PriceWithheldReason
	}{
		{name: "no exclusion"},
		{name: "unrelated pair excluded", rows: []timescale.SourceExclusion{{ID: 1, Source: "sdex", Base: "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", Quote: "fiat:USD", From: from}}},
		{name: "twin market excluded", rows: []timescale.SourceExclusion{{ID: 1, Source: "sdex", Base: twin, Quote: "native", From: from}}, want: PriceWithheldSourceExcluded},
		{name: "anchor excluded", rows: []timescale.SourceExclusion{{ID: 1, Source: "sdex", Base: "native", Quote: twin, From: from}}, want: PriceWithheldSourceExcluded},
	} {
		s := New(Options{
			VerifiedCurrencies: cat,
			GlobalPrice:        newWithheldTierReader(nil),
			AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
			Exclusions:         pricingguard.NewSourceExclusionGate(globalExclTable{rows: tc.rows}, pricingguard.SourceExclusionGateOptions{}),
		})
		view := s.buildGlobalAssetView(context.Background(), vc)
		page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
		s.fillCataloguePricesForPage(context.Background(), page)
		pct := "4.2"
		mergeTwinStats(&page[0], AssetDetail{Change24hPct: &pct})
		if (view.PriceUSD == nil) != (tc.want != "") || view.PriceWithheldReason != tc.want {
			t.Errorf("%s: slug view price_usd=%s reason=%q, want reason %q", tc.name, strOrEmpty(view.PriceUSD), view.PriceWithheldReason, tc.want)
		}
		if (page[0].PriceUSD == nil) != (tc.want != "") || page[0].PriceWithheldReason != tc.want || (page[0].Change24hPct != nil) != (tc.want == "") {
			t.Errorf("%s: catalogue row price_usd=%s reason=%q change_24h set=%v, want reason %q", tc.name, strOrEmpty(page[0].PriceUSD),
				page[0].PriceWithheldReason, page[0].Change24hPct != nil, tc.want)
		}
	}
}

// TestGlobalAssetHeadline_OnChainFallbackJudgedOverItsReadWindow: the per-asset reader's price may come from any bucket in its
// 7-day window, so an exclusion that ended days ago still withholds it on /v1/assets/{slug}; one that ended before the window
// does not.
func TestGlobalAssetHeadline_OnChainFallbackJudgedOverItsReadWindow(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	vc, ok := cat.LookupBySlug("usdc")
	if !ok || vc.StellarEntry() == nil {
		t.Fatal("embedded catalogue has no usdc Stellar issuance")
	}
	twin := vc.StellarEntry().AssetID
	listing := "0.4321"
	day := 24 * time.Hour
	for _, tc := range []struct {
		name     string
		from, to time.Duration
		withheld bool
	}{
		{name: "ended four days ago", from: 5 * day, to: 4 * day, withheld: true},
		{name: "ended before the window", from: 10 * day, to: 9 * day},
	} {
		rows := []timescale.SourceExclusion{{
			ID: 1, Source: "sdex", Base: twin, Quote: "native", From: time.Now().Add(-tc.from), Until: time.Now().Add(-tc.to),
		}}
		srv := New(Options{
			VerifiedCurrencies: cat,
			GlobalPrice:        newWithheldTierReader(nil),
			AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
			Exclusions:         pricingguard.NewSourceExclusionGate(globalExclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{}),
		})
		ts := httptest.NewServer(srv.Handler())
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		ts.Close()
		body := string(b)
		served := strings.Contains(body, `"price_usd":"0.4321"`)
		named := strings.Contains(body, `"price_withheld_reason":"source_excluded"`)
		if resp.StatusCode != http.StatusOK || served == tc.withheld || named != tc.withheld {
			t.Errorf("%s: %d %s, want withheld=%v", tc.name, resp.StatusCode, body, tc.withheld)
		}
	}
}

// refuseGlobalTierGate refuses only the off-chain crypto:TICKER/fiat:USD market, so every on-chain leg passes.
type refuseGlobalTierGate struct{}

func (refuseGlobalTierGate) Allowed(_ context.Context, base, _ canonical.Asset, _ string) bool {
	return !strings.HasPrefix(base.String(), "crypto:")
}

func (g refuseGlobalTierGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}

// TestGlobalAssetHeadline_SubstanceRefusalIsNamed: a headline tier the substance gate refuses is withheld with its reason,
// like a source-excluded one, rather than silently filled from the twin's on-chain listing price.
func TestGlobalAssetHeadline_SubstanceRefusalIsNamed(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	vc, ok := cat.LookupBySlug("usdc")
	if !ok {
		t.Fatal("embedded catalogue has no usdc")
	}
	listing := "0.4321"
	reader := newWithheldTierReader(nil)
	reader.refusing.Store(false)
	s := New(Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        reader,
		AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
		Substance:          refuseGlobalTierGate{},
	})
	view := s.buildGlobalAssetView(context.Background(), vc)
	if view.PriceUSD != nil || view.PriceWithheldReason != PriceWithheldSubstance {
		t.Fatalf("price_usd=%s reason=%q, want no price and reason %q", strOrEmpty(view.PriceUSD), view.PriceWithheldReason, PriceWithheldSubstance)
	}
}

// TestGlobalAssetHeadline_WithheldTierBlocksOnChainFallback:a headline tier refused by a source exclusion is not "no data",
// so the on-chain listing price (raw pooled prices_1m, which no exclusion judges) does not fill it; an unreadable exclusion
// set marks the body stale and is not replayed, and a plain miss still takes the listing price.
func TestGlobalAssetHeadline_WithheldTierBlocksOnChainFallback(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	listing := "0.4321"
	get := func(t *testing.T, reader aggregate.GlobalPriceReader) string {
		t.Helper()
		srv := New(Options{
			VerifiedCurrencies: cat,
			GlobalPrice:        reader,
			AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}

	if body := get(t, newWithheldTierReader(nil)); !strings.Contains(body, `"price_usd":"0.4321"`) ||
		strings.Contains(body, "price_withheld_reason") {
		t.Fatalf("plain miss: %s, want the on-chain listing price and no reason", body)
	}

	excluded := get(t, newWithheldTierReader(&aggregate.TierWithheldError{}))
	if strings.Contains(excluded, "price_usd") || !strings.Contains(excluded, `"price_withheld_reason":"source_excluded"`) ||
		strings.Contains(excluded, `"stale":true`) {
		t.Fatalf("excluded: %s, want no price, reason source_excluded, not stale", excluded)
	}

	unread := newWithheldTierReader(&aggregate.TierWithheldError{Unread: true})
	srv := New(Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        unread,
		AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	var bodies [2]string
	for i := range bodies {
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodies[i] = string(b)
		unread.refusing.Store(false)
	}
	if strings.Contains(bodies[0], "price_usd") || !strings.Contains(bodies[0], `"price_withheld_reason":"unattributed"`) ||
		!strings.Contains(bodies[0], `"stale":true`) {
		t.Fatalf("unread: %s, want no price, reason unattributed, stale", bodies[0])
	}
	if !strings.Contains(bodies[1], `"price_usd":"0.9998"`) || strings.Contains(bodies[1], "price_withheld_reason") {
		t.Fatalf("after the read recovers: %s, want the fresh tier-1 price, not a replayed refusal", bodies[1])
	}
}
