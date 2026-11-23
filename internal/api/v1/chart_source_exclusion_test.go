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
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// inceptionKeyedReader is pairKeyedHistoryReader with the unbounded since-inception read served from the same fixture.
type inceptionKeyedReader struct{ pairKeyedHistoryReader }

func (r *inceptionKeyedReader) HistoryPoints(_ context.Context, p canonical.Pair, _ string, _ int) ([]v1.HistoryPoint, error) {
	return r.byPair[p.Base.String()+"/"+p.Quote.String()], nil
}

type chartExclResp struct {
	status   int
	cache    string
	raw      string
	points   []int64
	withheld string
}

func chartExclGet(t *testing.T, srv *v1.Server, path string) chartExclResp {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var body struct {
		Data struct {
			Points []struct {
				T string `json:"t"`
			} `json:"points"`
		} `json:"data"`
		Flags struct {
			FillsWithheldReason string `json:"fills_withheld_reason"`
		} `json:"flags"`
	}
	_ = json.Unmarshal(raw, &body)
	// as_of is the wall clock, the one field two identical answers differ in.
	var whole map[string]json.RawMessage
	_ = json.Unmarshal(raw, &whole)
	delete(whole, "as_of")
	raw, _ = json.Marshal(whole)
	out := chartExclResp{status: resp.StatusCode, cache: resp.Header.Get("Cache-Control"), raw: string(raw), withheld: body.Flags.FillsWithheldReason}
	for _, p := range body.Data.Points {
		ts, err := time.Parse(time.RFC3339, p.T)
		if err != nil {
			t.Fatalf("point t %q: %v", p.T, err)
		}
		out.points = append(out.points, ts.Unix())
	}
	return out
}

func equalUnix(got []int64, want ...time.Time) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i].Unix() {
			return false
		}
	}
	return true
}

func chartExclServer(reader v1.HistoryReader, fx v1.FXHistoryReader, rows []timescale.SourceExclusion, readErr error) *v1.Server {
	opts := v1.Options{History: reader, FXHistory: fx, Clock: t0FixtureClock}
	if rows != nil || readErr != nil {
		opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: rows, err: readErr}, pricingguard.SourceExclusionGateOptions{})
	}
	return v1.New(opts)
}

// TestChartSeriesWithholdsExcludedBuckets: on /v1/chart (vwap and twap) and /v1/history/since-inception a bucket is judged on
// its recorded sources over its own span. Only a bucket the excluded source fed inside the window is removed (a gap, never a
// zero) and the response says so; a bucket outside the window or fed by another source is served, and with nothing covered
// the body is byte-identical to a gate-less server's.
func TestChartSeriesWithholdsExcludedBuckets(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	b0, b1, b2, b3 := t0, t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	pts := []v1.HistoryPoint{
		{Bucket: b0, VWAP: "0.10", Sources: []string{"aquarius"}},
		{Bucket: b1, VWAP: "0.90", Sources: []string{"soroswap"}},
		{Bucket: b2, VWAP: "0.11", Sources: []string{"aquarius"}},
		{Bucket: b3, VWAP: "0.12", Sources: []string{"soroswap"}},
	}
	reader := func() *inceptionKeyedReader {
		return &inceptionKeyedReader{pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{"native/fiat:USD": pts}}}
	}
	// Covers b1 and b2, not b0 or b3.
	window := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: b1.Add(10 * time.Minute), Until: b2.Add(10 * time.Minute)}}
	elsewhere := []timescale.SourceExclusion{{ID: 2, Source: "soroswap", From: b3.Add(24 * time.Hour)}}

	for _, path := range []string{
		"/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h",
		"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1h",
	} {
		got := chartExclGet(t, chartExclServer(reader(), nil, window, nil), path)
		if got.status != http.StatusOK || !equalUnix(got.points, b0, b2, b3) || got.withheld != "source_excluded" || got.cache != "no-store" {
			t.Fatalf("%s excluded = %d %v withheld=%q cache=%q, want 200 [b0 b2 b3] source_excluded no-store\n%s",
				path, got.status, got.points, got.withheld, got.cache, got.raw)
		}
		base := chartExclGet(t, chartExclServer(reader(), nil, nil, nil), path)
		clean := chartExclGet(t, chartExclServer(reader(), nil, elsewhere, nil), path)
		if clean.raw != base.raw || clean.cache != base.cache || clean.withheld != "" {
			t.Fatalf("%s untouched body differs from a gate-less server's:\n got %s\nwant %s", path, clean.raw, base.raw)
		}
		if u := chartExclGet(t, chartExclServer(reader(), nil, nil, errors.New("table unreadable")), path); u.status != http.StatusNotFound {
			t.Fatalf("%s with the exclusion set unread = %d, want 404 withheld", path, u.status)
		}
	}

	// TWAP buckets record no sources: any exclusion naming the asset over a bucket's span withholds it.
	twap := []v1.HistoryPoint{{Bucket: b0, VWAP: "0.10"}, {Bucket: b1, VWAP: "0.11"}, {Bucket: b3, VWAP: "0.12"}}
	tr := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{"native/fiat:USD": twap}}
	named := []timescale.SourceExclusion{{ID: 3, Source: "soroswap", Base: "native", Quote: "fiat:USD", From: b1, Until: b2}}
	const twapPath = "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h&price_type=twap"
	if got := chartExclGet(t, chartExclServer(tr, nil, named, nil), twapPath); !equalUnix(got.points, b0, b3) || got.withheld != "source_excluded" {
		t.Fatalf("twap = %v withheld=%q, want [b0 b3] source_excluded\n%s", got.points, got.withheld, got.raw)
	}
	other := []timescale.SourceExclusion{{ID: 4, Source: "soroswap", Base: "crypto:BTC", Quote: "fiat:USD", From: b1, Until: b2}}
	if got := chartExclGet(t, chartExclServer(tr, nil, other, nil), twapPath); len(got.points) != 3 || got.withheld != "" {
		t.Fatalf("twap, another asset excluded = %v withheld=%q, want all 3 points unmarked", got.points, got.withheld)
	}
}

// TestChartXLMCrossJudgesLegsUnderTheServedPair: a series derived through XLM is served as the requested pair, so an exclusion
// stored there withholds a crossed bucket whose leg the excluded source fed; the other bucket still crosses.
func TestChartXLMCrossJudgesLegsUnderTheServedPair(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	d1 := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	d2 := d1.AddDate(0, 0, 1)
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		usdc.String() + "/native": {{Bucket: d1, VWAP: "3.3333333333", Sources: []string{"soroswap"}}, {Bucket: d2, VWAP: "3.3333333333", Sources: []string{"soroswap"}}},
		"crypto:XLM/fiat:USD":     {{Bucket: d1, VWAP: "0.27", Sources: []string{"binance"}}, {Bucket: d2, VWAP: "0.27", Sources: []string{"binance"}}},
	}}
	excl := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", Base: usdc.String(), Quote: "fiat:USD", From: d2}}
	srv := v1.New(v1.Options{
		History: reader, USDPeggedClassics: []canonical.Asset{usdc},
		Exclusions: pricingguard.NewSourceExclusionGate(exclTable{rows: excl}, pricingguard.SourceExclusionGateOptions{}),
	})
	got := chartExclGet(t, srv, "/v1/chart?asset="+usdc.String()+"&quote=fiat:USD&timeframe=1mo&granularity=1d")
	if got.status != http.StatusOK || !equalUnix(got.points, d1) || got.withheld != "source_excluded" {
		t.Fatalf("cross = %d %v withheld=%q, want [d1] source_excluded\n%s", got.status, got.points, got.withheld, got.raw)
	}
}

// TestChartFiatWithholdsExcludedFXDays: an fx_quotes day is judged on its vendor; a withheld leg day drops the derived cross
// day, and the other days are served.
func TestChartFiatWithholdsExcludedFXDays(t *testing.T) {
	d0 := time.Unix(1_770_000_000, 0).UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	d1, d2 := d0.AddDate(0, 0, 1), d0.AddDate(0, 0, 2)
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d0, RateUSDText: "0.9", InverseUSDText: "1.1", Source: "ecb"},
		{Bucket: d1, RateUSDText: "0.9", InverseUSDText: "1.1", Source: "vendorx"},
		{Bucket: d2, RateUSDText: "0.9", InverseUSDText: "1.1", Source: "ecb"},
	}}
	excl := []timescale.SourceExclusion{{ID: 1, Source: "vendorx", From: d1, Until: d2}}
	for _, path := range []string{
		"/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1mo",
		"/v1/chart?asset=fiat:EUR&quote=fiat:JPY&timeframe=1mo",
	} {
		got := chartExclGet(t, chartExclServer(&stubHistoryReader{}, fx, excl, nil), path)
		if got.status != http.StatusOK || !equalUnix(got.points, d0, d2) || got.withheld != "source_excluded" {
			t.Fatalf("%s = %d %v withheld=%q, want [d0 d2] source_excluded\n%s", path, got.status, got.points, got.withheld, got.raw)
		}
		if base, clean := chartExclGet(t, chartExclServer(&stubHistoryReader{}, fx, nil, nil), path),
			chartExclGet(t, chartExclServer(&stubHistoryReader{}, fx, []timescale.SourceExclusion{{ID: 2, Source: "other", From: d0}}, nil), path); base.raw != clean.raw {
			t.Fatalf("%s untouched body differs:\n got %s\nwant %s", path, clean.raw, base.raw)
		}
	}
}
