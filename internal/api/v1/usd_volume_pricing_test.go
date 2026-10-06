package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubUsdVolumePricingReader struct {
	rows []timescale.UsdVolumePricingRow
	err  error
}

func (s stubUsdVolumePricingReader) UsdVolumePricingStats(context.Context, time.Time, time.Time, []string) ([]timescale.UsdVolumePricingRow, error) {
	return s.rows, s.err
}

func coverageWithPricing(t *testing.T, snaps []timescale.CompletenessSnapshot, cache *v1.UsdVolumePricingCache) (v1.CoverageVerdictsView, string) {
	t.Helper()
	srv := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}, UsdVolumePricing: cache})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/coverage")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.CoverageVerdictsView `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data, string(body)
}

func completeSnap(now time.Time) []timescale.CompletenessSnapshot {
	return []timescale.CompletenessSnapshot{{
		Source: "blend", Genesis: 1, Tip: 100, Watermark: 100, CoveragePct: 1,
		Complete: true, LakeComplete: true, SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
		ComputedAt: now, ProjectionEvidencedAt: now,
	}}
}

// A source far below its bar must not move the headline: the axis measures
// valuation, not capture.
func TestCoverage_usdVolumePricingBelowBarLeavesHeadlineUnchanged(t *testing.T) {
	cache := v1.NewUsdVolumePricingCache(stubUsdVolumePricingReader{rows: []timescale.UsdVolumePricingRow{
		{Source: "binance", Trades: 1000, Priced: 500, Unpriced: 500},
		{Source: "coinbase", Trades: 10, Priced: 10},
		{Source: "sdex", Trades: 100, Priced: 90, Unpriced: 8, Unroutable: 2},
		{Source: "kraken"},
	}}, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	d, _ := coverageWithPricing(t, completeSnap(now), cache)
	if d.CompleteSources != 1 || d.TotalSources != 1 || d.LakeCompleteSources != 1 || !d.Sources[0].Complete {
		t.Fatalf("headline moved: complete=%d total=%d lake=%d", d.CompleteSources, d.TotalSources, d.LakeCompleteSources)
	}
	p := d.UsdVolumePricing
	if p == nil || !p.LowerBound || p.Excluded == "" {
		t.Fatalf("want lower_bound with named exclusion, got %+v", p)
	}
	by := map[string]v1.UsdVolumePricingSource{}
	for _, s := range p.Sources {
		by[s.Source] = s
	}
	if b := by["binance"]; b.MeetsBar == nil || *b.MeetsBar || b.PricedRatio != "0.500000" || b.Class != "external" {
		t.Errorf("binance = %+v", b)
	}
	if c := by["coinbase"]; c.MeetsBar == nil || !*c.MeetsBar {
		t.Errorf("coinbase = %+v", c)
	}
	// on-chain: ratio excludes unroutable (90/98), no verdict yet.
	if s := by["sdex"]; s.MeetsBar != nil || s.PricedRatio != "0.918367" || s.Class != "onchain" {
		t.Errorf("sdex = %+v", s)
	}
	// empty window: no ratio, no verdict, no divide by zero.
	if k := by["kraken"]; k.PricedRatio != "" || k.MeetsBar != nil {
		t.Errorf("kraken = %+v", k)
	}
}

func TestCoverage_usdVolumePricingFullyPricedIsNotLowerBound(t *testing.T) {
	cache := v1.NewUsdVolumePricingCache(stubUsdVolumePricingReader{rows: []timescale.UsdVolumePricingRow{
		{Source: "binance", Trades: 5, Priced: 5},
	}}, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, _ := coverageWithPricing(t, completeSnap(time.Now().UTC()), cache)
	if d.UsdVolumePricing == nil || d.UsdVolumePricing.LowerBound || d.UsdVolumePricing.Excluded != "" {
		t.Fatalf("got %+v", d.UsdVolumePricing)
	}
}

func TestCoverage_usdVolumePricingNullKeyPresentWhenCacheEmpty(t *testing.T) {
	failing := v1.NewUsdVolumePricingCache(stubUsdVolumePricingReader{err: errors.New("boom")}, nil)
	if err := failing.Refresh(context.Background()); err == nil {
		t.Fatal("want refresh error")
	}
	for name, cache := range map[string]*v1.UsdVolumePricingCache{"nil": nil, "never-refreshed": failing} {
		d, body := coverageWithPricing(t, completeSnap(time.Now().UTC()), cache)
		var raw struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		if v, ok := raw.Data["usd_volume_pricing"]; !ok || string(v) != "null" {
			t.Errorf("%s: usd_volume_pricing = %q (present=%v), want null", name, v, ok)
		}
		if d.UsdVolumePricing != nil {
			t.Errorf("%s: want nil view", name)
		}
	}
}
