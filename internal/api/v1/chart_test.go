package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubFXHistoryReader implements v1.FXHistoryReader for chart-fiat tests.
type stubFXHistoryReader struct {
	points []v1.FXQuotePoint
	err    error
}

func (s *stubFXHistoryReader) ListFXHistory(_ context.Context, _ string, _, _ time.Time) ([]v1.FXQuotePoint, error) {
	return s.points, s.err
}

// TestChart_Fiat_GranularitySnappedToDaily covers CA2-A02-correct-2:
// fx_quotes is a one-row-per-UTC-day series, so a fiat:fiat chart must
// report granularity=1d (never the requested sub-daily grain) and must
// not raise truncated/discontinuous against a daily grid it was never
// measured on.
func TestChart_Fiat_GranularitySnappedToDaily(t *testing.T) {
	from := time.Now().UTC().Add(-24 * time.Hour)
	d1 := from.Add(2 * time.Hour) // 2h after `from`: > 15m grace, < 24h grace
	d2 := d1.Add(24 * time.Hour)  // contiguous daily bucket
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d1, RateUSDText: "1.08", InverseUSDText: "0.92592592592592592593"},
		{Bucket: d2, RateUSDText: "1.09", InverseUSDText: "0.91743119266055045872"},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=24h&granularity=15m")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Granularity != "1d" {
		t.Errorf("Granularity = %q, want %q (snapped to the grid fx_quotes actually serves)",
			env.Data.Granularity, "1d")
	}
	if env.Data.Truncated {
		t.Errorf("Truncated = true, want false: first bucket is 2h after `from`, well inside "+
			"a daily series' 24h grace (data_starts_at=%v)", env.Data.DataStartsAt)
	}
	if env.Data.Discontinuous {
		t.Errorf("Discontinuous = true, want false: the two points are one contiguous daily "+
			"bucket apart (gap %v -> %v)", env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
}

// tickerFXHistoryReader serves a distinct daily series per ticker so
// cross-fiat tests can hand each USD leg its own rates.
type tickerFXHistoryReader struct {
	byTicker map[string][]v1.FXQuotePoint
}

func (s *tickerFXHistoryReader) ListFXHistory(_ context.Context, ticker string, _, _ time.Time) ([]v1.FXQuotePoint, error) {
	return s.byTicker[ticker], nil
}

func TestChart_Fiat_CrossPair_TriangulatesViaUSD(t *testing.T) {
	// EUR/JPY (neither side USD): price = rate_usd[JPY] / rate_usd[EUR]
	// per shared day; days missing either leg are skipped.
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)
	fx := &tickerFXHistoryReader{byTicker: map[string][]v1.FXQuotePoint{
		"EUR": {
			{Bucket: d1, RateUSDText: "0.925", InverseUSDText: "1.08108108108108108108"},
			{Bucket: d2, RateUSDText: "0.930", InverseUSDText: "1.07526881720430107527"},
			{Bucket: d3, RateUSDText: "0.920", InverseUSDText: "1.08695652173913043478"}, // JPY leg absent — must be skipped
		},
		"JPY": {
			{Bucket: d1, RateUSDText: "155.00", InverseUSDText: "0.00645161290322580645"},
			{Bucket: d2, RateUSDText: "156.00", InverseUSDText: "0.00641025641025641026"},
		},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:EUR&quote=fiat:JPY&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags struct {
			Triangulated bool `json:"triangulated"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 (d3 has no JPY leg)", len(env.Data.Points))
	}
	// 155.00 / 0.925 = 167.567567… — assert the stable leading digits
	// (the 10th decimal wobbles with the float64 representation of
	// the 0.925 leg, which is inherent to the reader's float fields).
	if got := env.Data.Points[0].P; !strings.HasPrefix(got, "167.5675675") {
		t.Errorf("point[0].P = %q, want 167.5675675…", got)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true for a cross-fiat series")
	}
}

func TestChart_Fiat_CrossPair_NoSharedDays_EmptySeries(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	fx := &tickerFXHistoryReader{byTicker: map[string][]v1.FXQuotePoint{
		"EUR": {{Bucket: d1, RateUSDText: "0.925", InverseUSDText: "1.08108108108108108108"}},
		"JPY": {{Bucket: d2, RateUSDText: "155.00", InverseUSDText: "0.00645161290322580645"}},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:EUR&quote=fiat:JPY&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if len(env.Data.Points) != 0 {
		t.Errorf("no shared buckets should yield an empty series, got %d", len(env.Data.Points))
	}
}

// TestChart_TWAP_ServesTimeWeightedSeries — price_type=twap now serves
// a series from the twap_1h / twap_1d CAGGs (migration 0081)
// instead of a 400. The default 24h timeframe's 15m grain
// snaps onto the 1h TWAP CAGG, and the response reports the grain
// actually served + price_type=twap.
func TestChart_TWAP_ServesTimeWeightedSeries(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC().Truncate(time.Hour)
	v := "1234.5"
	reader := &stubHistoryReader{
		twapPoints: []v1.HistoryPoint{
			{Bucket: t0, VWAP: "0.4200", VolumeUSD: &v},
			{Bucket: t0.Add(time.Hour), VWAP: "0.4300"},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=twap")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.PriceType != "twap" {
		t.Errorf("price_type = %q, want twap", env.Data.PriceType)
	}
	// 24h default → 15m default grain → snapped to the 1h TWAP CAGG.
	if env.Data.Granularity != "1h" {
		t.Errorf("granularity = %q, want 1h (snapped)", env.Data.Granularity)
	}
	if reader.lastCall.granularity != "1h" {
		t.Errorf("reader saw granularity=%q, want snapped 1h", reader.lastCall.granularity)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(env.Data.Points))
	}
	if env.Data.Points[0].P != "0.4200" {
		t.Errorf("point[0].p = %q, want 0.4200 (twap value pass-through)", env.Data.Points[0].P)
	}
}

// TestChart_TWAP_GranularitySnapping pins the sub-daily→1h / daily+→1d
// snap so the twap surface stays on its two CAGGs.
func TestChart_TWAP_GranularitySnapping(t *testing.T) {
	for _, tc := range []struct {
		reqGran  string
		wantGran string
	}{
		{"1m", "1h"},
		{"15m", "1h"},
		{"1h", "1h"},
		{"4h", "1h"},
		{"1d", "1d"},
		{"1w", "1d"},
		{"1mo", "1d"},
	} {
		reader := &stubHistoryReader{twapPoints: []v1.HistoryPoint{
			{Bucket: time.Unix(1_770_000_000, 0).UTC(), VWAP: "1.0"},
		}}
		srv := v1.New(v1.Options{History: reader})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=twap&granularity="+tc.reqGran)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("gran=%s status=%d want 200", tc.reqGran, resp.StatusCode)
		}
		var env struct {
			Data v1.ChartSeries `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data.Granularity != tc.wantGran {
			t.Errorf("gran=%s → served %q, want %q", tc.reqGran, env.Data.Granularity, tc.wantGran)
		}
		if reader.lastCall.granularity != tc.wantGran {
			t.Errorf("gran=%s → reader saw %q, want %q", tc.reqGran, reader.lastCall.granularity, tc.wantGran)
		}
	}
}

// TestChart_TWAP_StablecoinFallback — the twap chart path reuses the
// shared X/fiat:USD → X/<USD-pegged classic> fallback: an empty literal
// twap series for native/fiat:USD falls back to the proxied peg pair's
// twap buckets and flags triangulated.
func TestChart_TWAP_StablecoinFallback(t *testing.T) {
	usdc, err := canonical.NewClassicAsset(
		"USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("build USDC: %v", err)
	}
	native := canonical.NativeAsset()
	pegPair, err := canonical.NewPair(native, usdc)
	if err != nil {
		t.Fatalf("build native/USDC pair: %v", err)
	}
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegPair.Base.String() + "/" + pegPair.Quote.String(): {
			{Bucket: time.Unix(1_770_000_000, 0).UTC(), VWAP: "0.4100"},
		},
	}}
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=twap")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags struct {
			Triangulated bool `json:"triangulated"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1 (from proxied peg pair)", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true (stablecoin proxy fired)")
	}
}

// TestChart_DefaultsTimeframeAndGranularity covers two defaults at
// once: timeframe=24h and granularity=15m (per ADR-0020 table).
func TestChart_DefaultsTimeframeAndGranularity(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	v := "100"
	reader := &stubHistoryReader{
		points: []v1.HistoryPoint{
			{Bucket: t0, VWAP: "0.50", VolumeUSD: &v},
			{Bucket: t0.Add(15 * time.Minute), VWAP: "0.51"},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Timeframe != "24h" {
		t.Errorf("timeframe default = %q, want 24h", env.Data.Timeframe)
	}
	if env.Data.Granularity != "15m" {
		t.Errorf("granularity default = %q, want 15m (per ADR-0020 table)", env.Data.Granularity)
	}
	if env.Data.PriceType != "vwap" {
		t.Errorf("price_type = %q, want vwap", env.Data.PriceType)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(env.Data.Points))
	}
	if reader.lastCall.granularity != "15m" {
		t.Errorf("reader saw granularity=%q, want default-resolved 15m", reader.lastCall.granularity)
	}
	// 24h timeframe → from must be ~24h before now (zero would
	// indicate the timeframe→window mapping wasn't applied).
	if reader.lastCall.from.IsZero() {
		t.Error("reader saw zero from — timeframe window not applied")
	}
	delta := time.Since(reader.lastCall.from) - 24*time.Hour
	if delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("from window = %v from now, want ~24h", time.Since(reader.lastCall.from))
	}
}

// pairKeyedHistoryReader returns different points per pair. Used
// by the stablecoin-fallback test below where the literal pair has
// no data and the proxy retry against X/USDC must succeed.
type pairKeyedHistoryReader struct {
	byPair map[string][]v1.HistoryPoint
	calls  []string // ordered list of pair keys queried
}

func (r *pairKeyedHistoryReader) HistoryPointsInRange(_ context.Context, p canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	key := p.Base.String() + "/" + p.Quote.String()
	r.calls = append(r.calls, key)
	return r.byPair[key], nil
}

// TWAPPointsInRange mirrors HistoryPointsInRange against the same
// per-pair fixture, so the twap chart path (and its stablecoin
// fallback) can be exercised through this reader too.
func (r *pairKeyedHistoryReader) TWAPPointsInRange(_ context.Context, p canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	key := p.Base.String() + "/" + p.Quote.String()
	r.calls = append(r.calls, key)
	return r.byPair[key], nil
}

// Other HistoryReader methods are unused by the chart handler but
// must exist for interface satisfaction.
func (r *pairKeyedHistoryReader) HistoryPoints(_ context.Context, _ canonical.Pair, _ string, _ int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (r *pairKeyedHistoryReader) TradesInRange(_ context.Context, _ canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	return nil, nil
}

func (r *pairKeyedHistoryReader) TradesInRangeAfter(_ context.Context, _ canonical.Pair, _, _, _ time.Time, _ uint32, _, _ string, _ uint32, _ int) ([]canonical.Trade, error) {
	return nil, nil
}

func (r *pairKeyedHistoryReader) LatestTradePerSource(_ context.Context, _ canonical.Pair, _ string) ([]canonical.Trade, error) {
	return nil, nil
}

func (r *pairKeyedHistoryReader) OHLCSeries(_ context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
	return nil, nil
}

// TestChart_StablecoinFallback exercises the X/fiat:USD →
// X/<USD-pegged classic> retry. /v1/chart for native/fiat:USD
// with no literal points but USDC trades available should return
// the USDC points and tag the envelope flags.triangulated=true.
func TestChart_StablecoinFallback(t *testing.T) {
	usdc, err := canonical.NewClassicAsset(
		"USDC",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{
		byPair: map[string][]v1.HistoryPoint{
			"native/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": {
				{Bucket: t0, VWAP: "0.16"},
				{Bucket: t0.Add(time.Hour), VWAP: "0.161"},
			},
		},
	}
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 (from USDC fallback)", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true on stablecoin-proxy fallback")
	}
	// Read order: the literal pair, then XLM's other canonical spellings
	// against the SAME quote (an alias form is the same asset, not a
	// proxy), and only once those come back empty the USDC proxy.
	if len(reader.calls) < 2 {
		t.Fatalf("reader saw %d calls, want at least 2 (literal + fallback)", len(reader.calls))
	}
	if reader.calls[0] != "native/fiat:USD" {
		t.Errorf("first call = %q, want native/fiat:USD", reader.calls[0])
	}
	const proxyPair = "native/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aliasAt, proxyAt := -1, -1
	for i, c := range reader.calls {
		switch c {
		case "crypto:XLM/fiat:USD":
			aliasAt = i
		case proxyPair:
			proxyAt = i
		}
	}
	if aliasAt < 0 {
		t.Errorf("crypto:XLM/fiat:USD never read; calls=%v", reader.calls)
	}
	if proxyAt < 0 {
		t.Fatalf("proxy pair native/USDC-… never read; calls=%v", reader.calls)
	}
	if aliasAt > proxyAt {
		t.Errorf("proxied the quote before trying the crypto:XLM spelling; calls=%v", reader.calls)
	}
}

// TestChart_NativeReadsCryptoXLMAlias is the chart-side half of the
// alias-blind regression: the literal-keyed CAGG read left
// `?asset=native` unable to see the series stored under `crypto:XLM`.
// chartStablecoinFallback did not cover it — it crosses the base aliases
// with PROXY quotes only and skips the requested quote, so
// crypto:XLM/fiat:USD was the one combination never read.
//
// An alias form is the same asset in another canonical spelling, so the
// response must NOT be stamped triangulated and must echo the id the
// client asked for.
func TestChart_NativeReadsCryptoXLMAlias(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{
		byPair: map[string][]v1.HistoryPoint{
			"crypto:XLM/fiat:USD": {
				{Bucket: t0, VWAP: "0.16"},
				{Bucket: t0.Add(time.Hour), VWAP: "0.161"},
			},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — ?asset=native must reach the crypto:XLM series", len(env.Data.Points))
	}
	if env.Flags.Triangulated {
		t.Error("flags.triangulated = true; an alias spelling is the same asset, not a proxy")
	}
	if env.Data.AssetID != "native" {
		t.Errorf("asset_id = %q, want native (echo the requested form)", env.Data.AssetID)
	}
	if reader.calls[0] != "native/fiat:USD" {
		t.Errorf("first call = %q, want native/fiat:USD (the literal form leads)", reader.calls[0])
	}
}

// TestChart_StablecoinFallback_CryptoBacker exercises the
// broadened fiat-proxy fallback: it also reaches the
// abstract stablecoin backers (crypto:USDT/USDC/…) crossed with the
// XLM base aliases — so a native/fiat:USD chart whose only USD depth is
// the CEX-sourced crypto:XLM/crypto:USDT series (binance) is found even
// with NO operator classic pegs configured. The old classic-pegs-only
// fallback returned an empty series here.
func TestChart_StablecoinFallback_CryptoBacker(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{
		byPair: map[string][]v1.HistoryPoint{
			// Binance stamps XLMUSDT as crypto:XLM/crypto:USDT.
			"crypto:XLM/crypto:USDT": {
				{Bucket: t0, VWAP: "0.1650"},
				{Bucket: t0.Add(time.Hour), VWAP: "0.1655"},
			},
		},
	}
	// No USDPeggedClassics — the crypto-backer path must carry it alone.
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 (from crypto:XLM/crypto:USDT backer)", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true on stablecoin-backer fallback")
	}
	// The literal pair is tried first, and the backer that carries the
	// answer is the crypto:USDT quote under the crypto:XLM base alias.
	if reader.calls[0] != "native/fiat:USD" {
		t.Errorf("first call = %q, want native/fiat:USD (literal)", reader.calls[0])
	}
	// This does not assert the backer was the LAST call, which would hold only
	// if the walk stopped at the first source pair holding any
	// bucket at all. That short-circuit is the defect
	// [Server.chartBucketMerge] removes — the walk reads every source
	// so a later one can fill a bucket an earlier one left empty — so the
	// last call is simply the last enumerated pair and says nothing. What
	// the test pins is that the backer is READ and that
	// the served values are ITS values, and both are asserted directly.
	read := false
	for _, c := range reader.calls {
		if c == "crypto:XLM/crypto:USDT" {
			read = true
		}
	}
	if !read {
		t.Errorf("crypto:XLM/crypto:USDT never read; calls=%v", reader.calls)
	}
	if got := []string{env.Data.Points[0].P, env.Data.Points[1].P}; got[0] != "0.1650" || got[1] != "0.1655" {
		t.Errorf("points = %v, want the backer's own values [0.1650 0.1655]", got)
	}
}

// TestChart_TruncatedFlagOnRetentionShortfall — when the requested
// timeframe extends before the earliest available data, the
// envelope flips Truncated=true and surfaces both DataStartsAt and
// RequestedFrom so consumers can render a "history begins ..." hint
// instead of guessing whether the deployment is data-thin or the
// asset is genuinely flat.
func TestChart_TruncatedFlagOnRetentionShortfall(t *testing.T) {
	// 7 days of 1d points, but request `timeframe=1y` (=365d window).
	now := time.Now().UTC().Truncate(24 * time.Hour)
	pts := make([]v1.HistoryPoint, 0, 7)
	for i := 6; i >= 0; i-- {
		pts = append(pts, v1.HistoryPoint{Bucket: now.Add(-time.Duration(i) * 24 * time.Hour), VWAP: "0.16"})
	}
	reader := &stubHistoryReader{points: pts}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if !env.Data.Truncated {
		t.Error("Truncated = false, want true (1y asked, only 7 days returned)")
	}
	if env.Data.DataStartsAt == nil {
		t.Fatal("DataStartsAt = nil on truncated response")
	}
	if env.Data.RequestedFrom == nil {
		t.Fatal("RequestedFrom = nil on truncated response")
	}
	if !env.Data.DataStartsAt.Time().Equal(pts[0].Bucket) {
		t.Errorf("DataStartsAt = %v, want %v", env.Data.DataStartsAt, pts[0].Bucket)
	}
	// RequestedFrom should be ~365d before now.
	delta := time.Since(env.Data.RequestedFrom.Time()) - 365*24*time.Hour
	if delta < -10*time.Second || delta > 10*time.Second {
		t.Errorf("RequestedFrom = %v ago, want ~365d", time.Since(env.Data.RequestedFrom.Time()))
	}
}

// A timeframe=all read that fills the 50k row cap holds the OLDEST slice;
// the response must say so, and a bounded or under-cap read must not.
func TestChart_TimeframeAllRowCapTruncated(t *testing.T) {
	const historyMaxPoints = 50_000 // internal/api/v1/history.go
	t0 := time.Now().UTC().Add(-historyMaxPoints * time.Minute).Truncate(time.Minute)
	full := make([]v1.HistoryPoint, historyMaxPoints)
	for i := range full {
		full[i] = v1.HistoryPoint{Bucket: t0.Add(time.Duration(i) * time.Minute), VWAP: "1.0"}
	}
	cases := []struct {
		name   string
		query  string
		points []v1.HistoryPoint
		want   bool
	}{
		{"all at cap", "timeframe=all&granularity=1m", full, true},
		{"all under cap", "timeframe=all&granularity=1m", full[:10], false},
		{"bounded at cap", "timeframe=24h&granularity=1m", full, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{History: &stubHistoryReader{points: tc.points}})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&"+tc.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			var env struct {
				Data v1.ChartSeries `json:"data"`
			}
			mustDecode(t, resp, &env)
			if env.Data.RowCapTruncated != tc.want {
				t.Fatalf("row_cap_truncated = %v, want %v", env.Data.RowCapTruncated, tc.want)
			}
			if tc.want {
				if env.Data.DataEndsAt == nil || !time.Time(*env.Data.DataEndsAt).Equal(full[len(full)-1].Bucket) {
					t.Errorf("data_ends_at = %v, want %v", env.Data.DataEndsAt, full[len(full)-1].Bucket)
				}
			} else if env.Data.DataEndsAt != nil {
				t.Errorf("data_ends_at = %v, want nil", env.Data.DataEndsAt)
			}
		})
	}
}

func chartRowCapSeries(start time.Time, n int, step time.Duration) []v1.HistoryPoint {
	out := make([]v1.HistoryPoint, n)
	for i := range out {
		out[i] = v1.HistoryPoint{Bucket: start.Add(time.Duration(i) * step), VWAP: "1.0"}
	}
	return out
}

func getChartSeries(t *testing.T, reader v1.HistoryReader, query string) v1.ChartSeries {
	t.Helper()
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data
}

// The cap is judged per source read, not on the merged output: a capped
// source flags the series even when an uncapped one extends it, and a
// union that merely totals 50k does not.
func TestChart_RowCapTruncated_PerSourceRead(t *testing.T) {
	const capN = 50_000 // historyMaxPoints
	t0 := time.Now().UTC().Add(-2 * capN * time.Minute).Truncate(time.Minute)
	capped := chartRowCapSeries(t0, capN, time.Minute)
	later := chartRowCapSeries(t0.Add(capN*time.Minute), 1000, time.Minute)

	s := getChartSeries(t, &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		"native/fiat:USD":     capped,
		"crypto:XLM/fiat:USD": later,
	}}, "timeframe=all&granularity=1m")
	if !s.RowCapTruncated {
		t.Fatal("row_cap_truncated = false; one source read hit the cap")
	}
	if s.DataEndsAt == nil || !time.Time(*s.DataEndsAt).Equal(capped[capN-1].Bucket) {
		t.Errorf("data_ends_at = %v, want the capped source's end %v", s.DataEndsAt, capped[capN-1].Bucket)
	}

	half := chartRowCapSeries(t0, capN/2, time.Minute)
	rest := chartRowCapSeries(t0.Add(capN/2*time.Minute), capN/2, time.Minute)
	s = getChartSeries(t, &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		"native/fiat:USD":     half,
		"crypto:XLM/fiat:USD": rest,
	}}, "timeframe=all&granularity=1m")
	if len(s.Points) != capN {
		t.Fatalf("union has %d points, want %d", len(s.Points), capN)
	}
	if s.RowCapTruncated || s.DataEndsAt != nil {
		t.Errorf("uncapped sources whose union is %d flagged: %v %v", capN, s.RowCapTruncated, s.DataEndsAt)
	}
}

func TestChart_TWAP_RowCapTruncated(t *testing.T) {
	const capN = 50_000 // historyMaxPoints
	t0 := time.Now().UTC().Add(-capN * time.Hour).Truncate(time.Hour)
	full := chartRowCapSeries(t0, capN, time.Hour)
	s := getChartSeries(t, &stubHistoryReader{twapPoints: full}, "price_type=twap&timeframe=all&granularity=1h")
	if !s.RowCapTruncated || s.DataEndsAt == nil || !time.Time(*s.DataEndsAt).Equal(full[capN-1].Bucket) {
		t.Fatalf("twap at cap: truncated=%v ends=%v", s.RowCapTruncated, s.DataEndsAt)
	}
	s = getChartSeries(t, &stubHistoryReader{twapPoints: full[:10]}, "price_type=twap&timeframe=all&granularity=1h")
	if s.RowCapTruncated {
		t.Error("twap under cap flagged")
	}
}

func TestChart_MarketCap_FiatCNY_ComputesFromM2(t *testing.T) {
	d1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d1, RateUSDText: "7.18", InverseUSDText: "0.13927576601671309192"},
		{Bucket: d2, RateUSDText: "7.20", InverseUSDText: "0.13888888888888888889"},
	}}
	srv := v1.New(v1.Options{
		History:            &stubHistoryReader{},
		FXHistory:          fx,
		VerifiedCurrencies: newTestCatalogue(t),
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:CNY&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.PriceType != "market_cap" {
		t.Errorf("price_type: got %q want market_cap", env.Data.PriceType)
	}
	if got := len(env.Data.Points); got != 2 {
		t.Fatalf("got %d points, want 2", got)
	}
	// First point: M2 (CNY 302T per seed) × 1/7.18 ≈ 42.06T USD. Just
	// verify the result is in the expected magnitude — exact figure
	// depends on the catalogue value.
	first := env.Data.Points[0].P
	if len(first) < 3 || first == "0.00" {
		t.Errorf("first market_cap point looks empty: %q", first)
	}
}

func TestChart_MarketCap_Crypto_Computed(t *testing.T) {
	// On-chain base: market_cap = daily USD price × daily circulating
	// supply (supply_1d CAGG). 10^10 stroops /1e7 = 1000 XLM.
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	hist := &stubHistoryReader{points: []v1.HistoryPoint{
		{Bucket: d(1), VWAP: "0.10"},
		{Bucket: d(2), VWAP: "0.20"},
	}}
	sup := &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
		{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)}, // 1000 XLM
	}}
	srv := v1.New(v1.Options{History: hist, Supply: sup, VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.PriceType != "market_cap" {
		t.Errorf("price_type: got %q want market_cap", env.Data.PriceType)
	}
	if got := len(env.Data.Points); got != 2 {
		t.Fatalf("got %d points, want 2: %+v", got, env.Data.Points)
	}
	if env.Data.Points[0].P != "100.00" || env.Data.Points[1].P != "200.00" {
		t.Errorf("market_cap points = %q, %q; want 100.00, 200.00 (0.10×1000, 0.20×1000 forward-filled)",
			env.Data.Points[0].P, env.Data.Points[1].P)
	}
}

// /v1/chart accepts `base=` as alias
// for `asset=` so URLs from /v1/twap don't 400 on first try.
func TestChart_BaseParamAcceptedAsAssetAlias(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?base=native&timeframe=24h")
	if resp.StatusCode == http.StatusBadRequest {
		t.Errorf("base= alias rejected (400); want it accepted as asset= alias")
	}
}

func getChartOpts(t *testing.T, opts v1.Options, query string) v1.ChartSeries {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, v1.New(opts)).URL+"/v1/chart?"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var body struct {
		Data v1.ChartSeries `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data
}

func TestChart_Fiat_RateDirection(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d1, RateUSDText: "7.18", InverseUSDText: "0.13927576601671309192"},
		{Bucket: d2, RateUSDText: "7.20", InverseUSDText: "0.13888888888888888889"},
	}}
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"USD to CNY serves the rate", "asset=fiat:USD&quote=fiat:CNY", []string{"7.1800000000", "7.2000000000"}},
		{"CNY to USD serves the inverse", "asset=fiat:CNY&quote=fiat:USD", []string{"0.1392757660", "0.1388888888"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := getChartOpts(t, v1.Options{History: &stubHistoryReader{}, FXHistory: fx}, tc.query+"&timeframe=1y&granularity=1d")
			if len(s.Points) != len(tc.want) {
				t.Fatalf("got %d points, want %d", len(s.Points), len(tc.want))
			}
			for i, w := range tc.want {
				if s.Points[i].P != w {
					t.Errorf("point[%d].P = %q, want %q", i, s.Points[i].P, w)
				}
			}
		})
	}
}

func TestChart_Status(t *testing.T) {
	hist := func(*testing.T) v1.Options { return v1.Options{History: &stubHistoryReader{}} }
	withCatalogue := func(t *testing.T) v1.Options {
		return v1.Options{History: &stubHistoryReader{}, VerifiedCurrencies: newTestCatalogue(t)}
	}
	for _, tc := range []struct {
		name, query string
		opts        func(*testing.T) v1.Options
		want        int
	}{
		{"reader nil", "asset=native", func(*testing.T) v1.Options { return v1.Options{} }, http.StatusServiceUnavailable},
		{"fiat without FX reader serves empty", "asset=fiat:CNY&quote=fiat:USD&timeframe=1y&granularity=1d", hist, http.StatusOK},
		{"missing asset", "", hist, http.StatusBadRequest},
		{"unknown timeframe", "asset=native&timeframe=2y", hist, http.StatusBadRequest},
		{"unknown price_type", "asset=native&price_type=mean", hist, http.StatusBadRequest},
		{
			"unknown granularity", "asset=native&granularity=2h",
			func(*testing.T) v1.Options {
				return v1.Options{History: &stubHistoryReader{pointsErr: v1.ErrUnknownGranularity}}
			}, http.StatusBadRequest,
		},
		{"asset equals quote", "asset=native&quote=native", hist, http.StatusBadRequest},
		{"both asset and base", "asset=native&base=native", hist, http.StatusBadRequest},
		{
			"market_cap needs supply reader", "asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d",
			withCatalogue, http.StatusServiceUnavailable,
		},
		{"market_cap quote must be USD", "asset=fiat:CNY&quote=fiat:EUR&price_type=market_cap", withCatalogue, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, httpTestServer(t, v1.New(tc.opts(t))).URL+"/v1/chart?"+tc.query)
			if resp.StatusCode != tc.want {
				t.Errorf("status=%d want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestChart_TimeframeGranularityWindow(t *testing.T) {
	for _, tc := range []struct {
		query, wantGran string
		wantZeroFrom    bool
	}{
		{"timeframe=1h", "1m", false},
		{"timeframe=24h", "15m", false},
		{"timeframe=1w", "1h", false},
		{"timeframe=1mo", "4h", false},
		{"timeframe=1y", "1d", false},
		{"timeframe=all", "1d", true},
		{"timeframe=1h&granularity=15m", "15m", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
			getChartOpts(t, v1.Options{History: reader}, "asset=native&"+tc.query)
			if reader.lastCall.granularity != tc.wantGran {
				t.Errorf("granularity=%q want %q", reader.lastCall.granularity, tc.wantGran)
			}
			if reader.lastCall.from.IsZero() != tc.wantZeroFrom {
				t.Errorf("from=%v, zero want %v", reader.lastCall.from, tc.wantZeroFrom)
			}
		})
	}
}

// A window with data reaching its start is not truncated, and timeframe=all
// is never truncated by definition.
func TestChart_NotTruncated(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, timeframe string
		points          []v1.HistoryPoint
	}{
		{"data reaches window start", "24h", []v1.HistoryPoint{
			{Bucket: now.Add(-25 * time.Hour), VWAP: "0.16"},
			{Bucket: now.Add(-1 * time.Hour), VWAP: "0.17"},
		}},
		{"timeframe all", "all", []v1.HistoryPoint{{Bucket: now.Add(-1 * time.Hour), VWAP: "0.16"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := getChartSeries(t, &stubHistoryReader{points: tc.points}, "timeframe="+tc.timeframe)
			if s.Truncated {
				t.Error("Truncated = true, want false")
			}
			if s.DataStartsAt != nil {
				t.Errorf("DataStartsAt = %v, want nil when not truncated", s.DataStartsAt)
			}
		})
	}
}

// The direct fiat chart leg (one USD-quoted series, no cross) renders
// through the same magnitude-relative decimal renderer as every other
// price surface: a hyperinflation-shaped rate below 1e-10 USD must not
// collapse to "0.0000000000", and a normal rate keeps its ten places.
func TestChart_Fiat_DirectLeg_TinyRateSurvives(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d1, RateUSDText: "2500000000000", InverseUSDText: "0.0000000000004"},
		{Bucket: d2, RateUSDText: "7.18", InverseUSDText: "0.13927576601671309192"},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:CNY&quote=fiat:USD&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(env.Data.Points); got != 2 {
		t.Fatalf("got %d points, want 2", got)
	}
	if got, want := env.Data.Points[0].P, "0.0000000000004000000000000"; got != want {
		t.Errorf("tiny inverse rate P = %q, want %q", got, want)
	}
	if back, ok := new(big.Rat).SetString(env.Data.Points[0].P); !ok || back.Sign() <= 0 {
		t.Errorf("tiny inverse rate %q reparses non-positive", env.Data.Points[0].P)
	}
	if got, want := env.Data.Points[1].P, "0.1392757660"; got != want {
		t.Errorf("normal inverse rate P = %q, want %q", got, want)
	}
}

// Every fiat chart leg serves the fx_quotes NUMERIC text exactly. A rate
// of 2^53+1 has no float64 representation; a float hop renders it as
// ...992 or ...994 while claiming ten exact decimal places.
func TestChart_Fiat_RatesAbove2to53StayExact(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	const huge = "9007199254740993" // 2^53 + 1
	const want = huge + ".0000000000"
	fx := &tickerFXHistoryReader{byTicker: map[string][]v1.FXQuotePoint{
		"EUR": {{Bucket: d1, RateUSDText: "1", InverseUSDText: "1"}},
		"JPY": {{Bucket: d1, RateUSDText: huge, InverseUSDText: huge}},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	for _, q := range []string{
		"asset=fiat:USD&quote=fiat:JPY", // rate_usd leg
		"asset=fiat:JPY&quote=fiat:USD", // inverse_usd leg
		"asset=fiat:EUR&quote=fiat:JPY", // cross: rate_usd[JPY] / rate_usd[EUR]
	} {
		resp := mustGet(t, ts.URL+"/v1/chart?"+q+"&timeframe=1y&granularity=1d")
		var env struct {
			Data v1.ChartSeries `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("%s: decode: %v", q, err)
		}
		if resp.StatusCode != http.StatusOK || len(env.Data.Points) != 1 {
			t.Fatalf("%s: status=%d points=%d, want 200 and 1", q, resp.StatusCode, len(env.Data.Points))
		}
		if got := env.Data.Points[0].P; got != want {
			t.Errorf("%s: P = %q, want %q", q, got, want)
		}
	}
}

// A request whose grid outruns the response cap is served at the
// finest grain that fits, and the response says so — while a request
// that fits is untouched at every grain.
//
// Both halves are asserted at once because either alone is a
// half-check: an echo test that never exercises a fitting pair would
// pass on a handler that coarsened everything, and a fitting-pair test
// alone would pass on that behaviour.
//
// `granularity` on the wire is the signal, and it is asserted TOGETHER
// with the grain the reader was called at: a response that echoed
// `15m` while reading `1m` would be a new lie in the field this change
// exists to make true, and the reverse (reading 15m, echoing 1m) is
// the bug itself.
func TestChart_CoarsensAGranularityTheWindowCannotCarry(t *testing.T) {
	for _, tc := range []chartGranularityFitCase{
		{
			timeframe: "1y", requested: "1m", served: "15m",
			why: "365d of minutes is 525,600 grid points against a 50,000-point cap",
		},
		{
			timeframe: "1y", requested: "15m", served: "15m",
			why: "35,040 points — fits, so it is served as asked",
		},
		{
			timeframe: "1mo", requested: "1m", served: "1m",
			why: "30d of minutes is 43,200 points — under the cap, untouched",
		},
		{
			timeframe: "1w", requested: "1m", served: "1m",
			why: "10,080 points",
		},
		{
			timeframe: "24h", requested: "1m", served: "1m",
			why: "1,440 points",
		},
		{
			timeframe: "1h", requested: "1m", served: "1m",
			why: "60 points",
		},
		{
			timeframe: "all", requested: "1m", served: "1m",
			why: "no requested width — the point count is a property of the data, not the request",
		},
		{
			timeframe: "1y", requested: "1d", served: "1d",
			why: "365 points; coarsening must never touch a grain that fits",
		},
	} {
		t.Run(tc.timeframe+"_"+tc.requested, func(t *testing.T) {
			reader := &stubHistoryReader{points: []v1.HistoryPoint{
				{Bucket: time.Unix(1_770_000_000, 0).UTC(), VWAP: "0.4200"},
			}}
			srv := v1.New(v1.Options{History: reader})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+
				"/v1/chart?asset=native&quote=fiat:USD&timeframe="+tc.timeframe+
				"&granularity="+tc.requested)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want 200", resp.StatusCode)
			}
			var env struct {
				Data v1.ChartSeries `json:"data"`
			}
			mustDecode(t, resp, &env)

			if env.Data.Granularity != tc.served {
				t.Errorf("granularity on the wire = %q, want %q (%s)",
					env.Data.Granularity, tc.served, tc.why)
			}
			if reader.lastCall.granularity != tc.served {
				t.Errorf("reader was called at %q, want %q (%s)",
					reader.lastCall.granularity, tc.served, tc.why)
			}
			// The requested timeframe is what the caller asked for and
			// is echoed unchanged — coarsening narrows the RESOLUTION,
			// never the window.
			if env.Data.Timeframe != tc.timeframe {
				t.Errorf("timeframe = %q, want %q", env.Data.Timeframe, tc.timeframe)
			}
		})
	}
}

// A bad `?granularity=` still 400s with the served-set enumeration:
// the fit rule must not swallow an unknown grain by coarsening it onto
// a real one.
func TestChart_UnknownGranularityStill400sAtEveryTimeframe(t *testing.T) {
	for _, tf := range []string{"1h", "24h", "1w", "1mo", "1y", "all"} {
		reader := &stubHistoryReader{pointsErr: v1.ErrUnknownGranularity}
		srv := v1.New(v1.Options{History: reader})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe="+tf+"&granularity=2h")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("timeframe=%s granularity=2h → status=%d, want 400", tf, resp.StatusCode)
		}
		// The 400 must come from an UNCOARSENED "2h" reaching the reader
		// — a fit rule that coarsened an unknown grain onto a real one
		// (e.g. "15m") before the reader ever sees it would still 400
		// here for the wrong reason and serve 200 on a live backend.
		if reader.lastCall.granularity != "2h" {
			t.Errorf("timeframe=%s: reader was called at %q, want unfitted %q",
				tf, reader.lastCall.granularity, "2h")
		}
	}
}

// A holed alias series must still reach the proxy walk: the walk was gated on
// the series being empty. Without the fix: 15 points and a 25-day gap.
func TestChart_HoledAliasSeriesStillReachesTheProxyWalk(t *testing.T) {
	store := holedFlagshipStore()
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — 15 from the CEX spelling and the 25 the pool holds "+
			"between them; a holed series is not an empty one", got)
	}
	days := chartBucketDays(t, env)
	for i := 1; i < len(days); i++ {
		if gap := days[i].Sub(days[i-1]); gap != 24*time.Hour {
			t.Fatalf("gap of %s between %s and %s — the merged series must be contiguous",
				gap, days[i-1].Format(time.RFC3339), days[i].Format(time.RFC3339))
		}
	}
	// Each bucket carries its own source's mark; the pool displaces none of
	// the days the CEX spelling answered.
	byDay := chartPriceByDay(env)
	for _, n := range holedCEXDays() {
		if got := byDay[holedDay(n)]; got != holedCEXPrice {
			t.Errorf("day -%d = %q, want the CEX spelling's own mark %q", n, got, holedCEXPrice)
		}
	}
	for _, n := range holedPoolDays() {
		if got := byDay[holedDay(n)]; got != holedPoolPrice {
			t.Errorf("day -%d = %q, want the pool's own mark %q", n, got, holedPoolPrice)
		}
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; 25 of the 40 buckets were served through the peg's SAC form")
	}
	// A filled hole reports a continuous series.
	if env.Data.Discontinuous || env.Data.GapStartsAt != nil || env.Data.GapEndsAt != nil {
		t.Errorf("discontinuous = %v (gap %v → %v) over 40 contiguous daily points",
			env.Data.Discontinuous, env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	// A holed window must keep walking the proxies.
	if len(store.pointCalls) < 4 {
		t.Errorf("reader saw only %d calls: %v — the proxy walk must run on a holed window",
			len(store.pointCalls), store.pointCalls)
	}
}

// The chart and /v1/ohlc read one store and must serve the same buckets, in
// both directions: converging the chart onto /v1/ohlc's read would silently
// empty assets only the chart reaches (the declared peg's own dollar series).
func TestChart_AgreesWithOHLCOnThePopulationItServes(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	chartDays := chartBucketDays(t, getChart(t, ts.URL+holedChartQ))

	from := holedDay(41).Format(time.RFC3339)
	to := holedDay(0).Format(time.RFC3339)
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1d&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ohlc status = %d, want 200", resp.StatusCode)
	}
	var ohlc struct {
		Data struct {
			Intervals []v1.OHLCSeriesBar `json:"intervals"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ohlc); err != nil {
		t.Fatal(err)
	}
	ohlcDays := make([]time.Time, 0, len(ohlc.Data.Intervals))
	for _, b := range ohlc.Data.Intervals {
		ohlcDays = append(ohlcDays, b.T.Time().UTC())
	}

	for _, d := range chartDays {
		if !slices.ContainsFunc(ohlcDays, d.Equal) {
			t.Errorf("/v1/chart serves %s and /v1/ohlc does not", d.Format(time.RFC3339))
		}
	}
	for _, d := range ohlcDays {
		if !slices.ContainsFunc(chartDays, d.Equal) {
			t.Errorf("/v1/ohlc serves %s and /v1/chart does not — the chart is hiding a bucket "+
				"its sibling reads from the same rows", d.Format(time.RFC3339))
		}
	}
	if len(chartDays) != 40 || len(ohlcDays) != 40 {
		t.Fatalf("chart=%d bars, ohlc=%d bars, want 40 each", len(chartDays), len(ohlcDays))
	}
}

// A held-back SAC spelling fills an unanswered bucket and may not re-price an
// answered one. RED if the merge takes the last writer or blends: the served
// mark becomes the pool's 13.09 instead of the CEX spelling's 0.16.
func TestChart_ThinPoolNeverDisplacesAnAnsweredBucket(t *testing.T) {
	day := holedDay(3)
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              {day: holedCEXPrice},
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {day: holedThinPoolPrice},
	}))

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got != 1 {
		t.Fatalf("points = %d, want 1 — both sources hold the same single bucket", got)
	}
	if got := env.Data.Points[0].P; got != holedCEXPrice {
		t.Errorf("served %q, want the established spelling's own mark %q", got, holedCEXPrice)
	}
	if env.Flags.Triangulated {
		t.Error("flags.triangulated = true; no served bucket came from a proxy quote")
	}
}

// A bucket depends only on itself, never on what else the window contains.
// The sources sit on opposite sides of the 1mo boundary: over 1y the CEX
// spelling answers somewhere, over 1mo nowhere, so a per-response first-hit
// would serve the pool's days in the narrow window and suppress them in the
// wide one.
func TestChart_ABucketRendersTheSameInEveryWindow(t *testing.T) {
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              holedDays(holedCEXPrice, 40, 31),
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: holedDays(holedPoolPrice, 5, 1),
	}))

	wide := chartPriceByDay(getChart(t, ts.URL+holedChartQ))
	narrow := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1mo&granularity=1d")

	if len(narrow.Data.Points) == 0 {
		t.Fatal("the 1mo window served nothing at all")
	}
	for _, p := range narrow.Data.Points {
		day := p.T.Time().UTC().Format(time.RFC3339)
		want, ok := wide[p.T.Time().UTC()]
		if !ok {
			t.Errorf("%s served in the 1mo window and absent from the 1y window", day)
		} else if p.P != want {
			t.Errorf("%s = %q in the 1mo window and %q in the 1y window", day, p.P, want)
		}
	}
}

// A series with a hole says so on the wire. `timeframe=all` never raises
// `truncated`, so without this signal a five-year hole has no marker.
func TestChart_DiscontinuousSignal(t *testing.T) {
	ts := holedServer(t, holedCEXOnlyStore())

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1d")
	requireHoleDeclared(t, len(env.Data.Points), env.Data.Discontinuous, env.Data.GapStartsAt, env.Data.GapEndsAt)
	if env.Data.Truncated {
		t.Error("truncated = true; `timeframe=all` never raises it, which is why the gap signal exists")
	}
}

// Series carried by a proxy route keep every bucket and say they were
// triangulated. The declared peg's own dollar series has no observed market
// under any spelling, so the chart derives it through XLM (/v1/ohlc serves
// nothing for it); the merge must leave that route last, whole-series, and
// reached only when nothing observed answered. An asset whose whole series
// comes from a proxy quote (AQUA, yXLM on production) keeps every bucket.
func TestChart_ProxyRoutedSeriesSurviveTheMerge(t *testing.T) {
	for _, tc := range []struct {
		name       string
		asset      string
		series     map[string]map[time.Time]string
		wantPoints int
		wantPrice  string // every point, when set
	}{
		{
			"derived XLM cross", pegAliasUSDCClassic,
			map[string]map[time.Time]string{
				pegAliasUSDCClassic + "/native": {holedDay(3): "6.2500000000", holedDay(2): "6.2500000000"},
				"crypto:XLM/fiat:USD":           {holedDay(3): holedCEXPrice, holedDay(2): holedCEXPrice},
			},
			2,
			// 6.25 XLM per USDC x $0.16 per XLM = $1.00.
			"1.0000000000",
		},
		{
			"proxy-only asset", pegAliasAquaClassic,
			map[string]map[time.Time]string{
				pegAliasAquaClassic + "/" + pegAliasUSDCSAC: holedDays("0.0041000000", 5, 1),
			},
			5, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := holedServer(t, newChartOHLCStore(tc.series))

			got := getChart(t, ts.URL+"/v1/chart?asset="+tc.asset+"&quote=fiat:USD&timeframe=1y&granularity=1d")
			if n := len(got.Data.Points); n != tc.wantPoints {
				t.Fatalf("points = %d, want %d — the proxy route is the only way to this series", n, tc.wantPoints)
			}
			if !got.Flags.Triangulated {
				t.Error("flags.triangulated = false on a series served through a proxy route")
			}
			for _, p := range got.Data.Points {
				if tc.wantPrice != "" && p.P != tc.wantPrice {
					t.Errorf("derived point = %q, want %s", p.P, tc.wantPrice)
				}
			}
		})
	}
}

// Measured regression: at `timeframe=1y&granularity=1m` one constituent takes
// 8.112s and three others sum to 5.867s. A walk reading all 24 inside the 8s
// ceiling turned the 200 into a 503 with every merged bucket discarded.
func TestChart_SlowProxyCannotCostTheSeries(t *testing.T) {
	slow := &latencyStore{
		chartOHLCStore: holedFlagshipStore(),
		delay: map[string]time.Duration{
			"crypto:XLM/fiat:USD":                              300 * time.Millisecond,
			"native/" + pegAliasUSDCClassic:                    8112 * time.Millisecond,
			"crypto:XLM/" + pegAliasUSDCClassic:                2 * time.Second,
			canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: 2 * time.Second,
			"crypto:XLM/crypto:USDT":                           2 * time.Second,
		},
	}
	ts := holedServer(t, slow)

	started := time.Now()
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1m")
	elapsed := time.Since(started)

	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds — a slow proxy may not "+
			"cost the series it was consulted to complete", got, len(holedCEXDays()))
	}
	if elapsed > 6*time.Second {
		t.Errorf("handler took %s — the walk must stop on its own budget, well inside the 8s ceiling", elapsed)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false; a walk that stopped short must say the series may be incomplete")
	}
}

// A source consulted to fill a series may never destroy one: a proxy error
// degrades (stale) rather than discarding 40 merged buckets as a 503.
func TestChart_ProxyReadErrorDegradesRatherThanFails(t *testing.T) {
	ts := holedServer(t, &latencyStore{
		chartOHLCStore: holedFlagshipStore(),
		fail:           map[string]error{"native/" + pegAliasUSDCClassic: context.DeadlineExceeded},
	})

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds", got, len(holedCEXDays()))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a walk cut short by a read error")
	}
}

// An alias spelling's read error is the response: a `?granularity=` the store
// rejects stays a 400, not an empty 200 (and nothing merged means nothing to
// degrade to).
func TestChart_AliasReadErrorIsStillTheAnswer(t *testing.T) {
	ts := holedServer(t, &latencyStore{
		chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{}),
		fail:           map[string]error{"native/fiat:USD": v1.ErrUnknownGranularity},
	})

	resp := mustGet(t, ts.URL+holedChartQ)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an alias spelling's error is the answer", resp.StatusCode)
	}
}

// `timeframe=all` on `prices_1mo`: buckets are calendar months, so a fixed
// 30-day threshold reports a hole in a contiguous year while a skipped month
// must still be reported.
func TestChart_MonthlyGranularityGapSignal(t *testing.T) {
	month := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	var year []time.Time
	for m := time.January; m <= time.December; m++ {
		year = append(year, month(2025, m))
	}
	for _, tc := range []struct {
		name       string
		months     []time.Time
		wantPoints int
		wantGap    bool
		gapStart   time.Time
		gapEnd     time.Time
	}{
		{"contiguous calendar months", year, 12, false, time.Time{}, time.Time{}},
		{"skipped March to May", []time.Time{
			month(2025, time.January), month(2025, time.February), month(2025, time.June), month(2025, time.July),
		}, 4, true, month(2025, time.February), month(2025, time.June)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := map[time.Time]string{}
			for _, m := range tc.months {
				series[m] = holedCEXPrice
			}
			ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": series}))

			env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1mo")
			if got := len(env.Data.Points); got != tc.wantPoints {
				t.Fatalf("points = %d, want %d", got, tc.wantPoints)
			}
			if env.Data.Discontinuous != tc.wantGap {
				t.Fatalf("discontinuous = %v, want %v (gap %v → %v) — a 31-day month is not a hole",
					env.Data.Discontinuous, tc.wantGap, env.Data.GapStartsAt, env.Data.GapEndsAt)
			}
			if !tc.wantGap {
				return
			}
			if got := env.Data.GapStartsAt.Time().UTC(); !got.Equal(tc.gapStart) {
				t.Errorf("gap_starts_at = %s, want %s", got.Format("2006-01-02"), tc.gapStart.Format("2006-01-02"))
			}
			if got := env.Data.GapEndsAt.Time().UTC(); !got.Equal(tc.gapEnd) {
				t.Errorf("gap_ends_at = %s, want %s", got.Format("2006-01-02"), tc.gapEnd.Format("2006-01-02"))
			}
		})
	}
}

// Common path, not an edge: 59 of the 60 largest assets report
// flags.triangulated=true, so every proxy read runs on an empty merge. A proxy
// that fails before the series is found must be skipped, not answered with
// (RED with the error split on merge emptiness: 503, zero points).
func TestChart_FailingEarlyProxyStillFindsTheSeries(t *testing.T) {
	pool := holedDays(holedPoolPrice, 5, 1)
	for _, tc := range []struct {
		name string
		fail map[string]error
	}{
		{"first proxy pair errors", map[string]error{
			"native/" + pegAliasUSDCClassic: errors.New("prices_1d unavailable"),
		}},
		{"first proxy pair times out", map[string]error{
			"native/" + pegAliasUSDCClassic: context.DeadlineExceeded,
		}},
		{"a backer pair times out", map[string]error{
			"native/crypto:USDT": context.DeadlineExceeded,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := holedServer(t, &latencyStore{
				chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{
					canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
				}),
				fail: tc.fail,
			})

			env := getChart(t, ts.URL+holedChartQ)
			if got := len(env.Data.Points); got != 5 {
				t.Fatalf("points = %d, want 5 — a failing proxy must be skipped", got)
			}
			if !env.Flags.Triangulated {
				t.Error("flags.triangulated = false on a series carried entirely by a proxy quote")
			}
			if !env.Flags.Stale {
				t.Error("flags.stale = false; a source was skipped, so the series may be short")
			}
		})
	}
}

// [chartWindow.covered] has two conditions: no readable grid point before the
// earliest hit, and as many distinct buckets as the grid has points. A wrong
// `true` silently truncates a money series with no stale flag, so both branches
// of the count are pinned: a covered window stops after one read without
// flags.stale; one interior bucket missing (held by a proxy) keeps walking.
// RED under a bare `return ok`: the missing day is never filled.
func TestChart_CoveredWindowStopsTheWalkOnlyWhenCovered(t *testing.T) {
	const missing = 100
	for _, tc := range []struct {
		name      string
		book      map[time.Time]string // under native/fiat:USD
		pool      map[time.Time]string // under the peg's SAC form
		wantCalls int                  // exact when >0; otherwise at least 2
	}{
		{"window covered after one read", holedDays(holedCEXPrice, 366, 0), nil, 1},
		{"every readable bucket held", holedDays(holedCEXPrice, 365, 1), nil, 1},
		{
			"one interior bucket missing", holedDays(holedCEXPrice, 365, 1, missing),
			map[time.Time]string{holedDay(missing): holedPoolPrice},
			0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := map[string]map[time.Time]string{"native/fiat:USD": tc.book}
			if tc.pool != nil {
				series[canonical.XLMSacContractID+"/"+pegAliasUSDCSAC] = tc.pool
			}
			store := newChartOHLCStore(series)
			ts := holedServer(t, store)

			env := getChart(t, ts.URL+holedChartQ)
			if env.Data.Discontinuous {
				t.Fatalf("series is not contiguous (gap %v → %v)", env.Data.GapStartsAt, env.Data.GapEndsAt)
			}
			if tc.wantCalls > 0 {
				if got := len(store.pointCalls); got != tc.wantCalls {
					t.Errorf("reader saw %d calls, want %d — a fully covered window has nothing left to fill: %v",
						got, tc.wantCalls, store.pointCalls)
				}
				if env.Flags.Stale {
					t.Error("flags.stale = true; stopping because the window is COVERED is not a degradation")
				}
				return
			}
			byDay := chartPriceByDay(env)
			if got, ok := byDay[holedDay(missing)]; !ok || got != holedPoolPrice {
				t.Fatalf("day -%d = %q (present=%v), want the proxy's %q — the window is not covered "+
					"while a bucket inside it is missing (calls=%d)",
					missing, got, ok, holedPoolPrice, len(store.pointCalls))
			}
			if len(store.pointCalls) < 2 {
				t.Errorf("reader saw %d call(s): %v — a covered-window stop fired on an UNCOVERED window",
					len(store.pointCalls), store.pointCalls)
			}
		})
	}
}

// A window with a bucket no source traded in can never be covered, so every
// remaining source is read. Each read must cost only the buckets still missing,
// so the walk ends by exhausting its list without flags.stale, and a later
// source that holds a missing bucket is still merged. The scattered case is
// RED while reads are capped by range count rather than missing width.
func TestChart_UnfillableHoleDoesNotRunTheWalkToBudget(t *testing.T) {
	var scattered []int
	for n := 12; n <= 360; n += 12 {
		scattered = append(scattered, n)
	}
	for _, tc := range []struct {
		name   string
		holes  []int
		filled []int // holes the last proxy (the peg's SAC pool) holds
		// Rows returned by every read after the first; with more than one
		// span, the dense proxy and the pool each also return probe rows.
		laterRows int
	}{
		{"one hole no source holds", []int{100}, nil, 0},
		{"three holes, a later source fills one", []int{40, 100, 300}, []int{100}, 1 + 2},
		// The two one-day gaps fit in the six missing days and are bridged,
		// so the dense proxy also returns days 21 and 61, which the book holds.
		{"adjacent hole runs", []int{20, 22, 60, 62, 100, 200}, []int{60, 200}, 2 + 2 + 2},
		// Eleven-day gaps: only two fit in the 30 (then 28) missing days,
		// so the dense proxy returns those 22 held days and the pool its two.
		{"thirty holes spread across the year", scattered, []int{120, 240}, 22 + 2 + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := map[time.Time]string{}
			for _, n := range tc.filled {
				pool[holedDay(n)] = holedPoolPrice
			}
			series := map[string]map[time.Time]string{
				"native/fiat:USD":                                  holedDays(holedCEXPrice, 365, 1, tc.holes...),
				"native/" + pegAliasUSDCClassic:                    holedDays(holedThinPoolPrice, 365, 1, tc.holes...),
				canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
			}
			store := &spanCostStore{
				chartOHLCStore: newChartOHLCStore(series),
				missing:        time.Duration(len(tc.holes)) * 24 * time.Hour,
				perSource:      map[string]*spanCost{},
			}
			ts := holedServer(t, store)

			env := getChart(t, ts.URL+holedChartQ)
			if env.Flags.Stale {
				t.Errorf("flags.stale = true after %d reads; nothing cut the walk short — a hole "+
					"no source holds is not a degradation", store.reads)
			}
			byDay := chartPriceByDay(env)
			for _, n := range tc.holes {
				got, served := byDay[holedDay(n)]
				switch {
				case slices.Contains(tc.filled, n) && got != holedPoolPrice:
					t.Errorf("day -%d = %q, want the later source's %q", n, got, holedPoolPrice)
				case !slices.Contains(tc.filled, n) && served:
					t.Errorf("day -%d served, but no source holds it", n)
				}
			}
			if store.laterRows != tc.laterRows {
				t.Errorf("reads after the first returned %d rows, want %d — a later source must be "+
					"read over the missing buckets only", store.laterRows, tc.laterRows)
			}
			for src, c := range store.perSource {
				if c.reads > 1+len(tc.holes) || c.width > 2*store.missing {
					t.Errorf("%s: %d reads over %v, want at most %d reads over %v — a later "+
						"source's cost must be bounded by the missing buckets",
						src, c.reads, c.width, 1+len(tc.holes), 2*store.missing)
				}
				if _, holds := series[src]; !holds && c.reads > 1 {
					t.Errorf("%s holds nothing but was read %d times, want 1", src, c.reads)
				}
			}
		})
	}
}

// TestChart_MarketCap_RequestDeadlineIs503NotEmptySeries pins the wire
// answer when the blanket request deadline fires inside
// /v1/chart?price_type=market_cap.
//
// The market-cap legs must not map ANY read error, deadline included,
// to emptyMarketCapSeries at HTTP 200 with Flags{} — no stale marker, no
// error. An empty series is syntactically valid, so a caller renders
// "market cap $0" for an asset holding real supply and cannot tell that
// from a genuine no-data window: the same wrong-answer-with-full-
// confidence failure as the bodyless 200 on
// /v1/lending/pools/{pool}/reserves, and just as invisible to any
// 5xx-based availability signal.
//
// RequestTimeout is set below every per-handler budget so the deadline
// the reader observes is the blanket one — the case no per-handler
// arithmetic can reach.
func TestChart_MarketCap_RequestDeadlineIs503NotEmptySeries(t *testing.T) {
	srv := v1.New(v1.Options{
		History: stallingMarketCapHistory{&stubHistoryReader{}},
		Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: time.Now().UTC().Truncate(24 * time.Hour), Circulating: big.NewInt(1_000_0000000)},
		}},
		VerifiedCurrencies: newTestCatalogue(t),
		RequestTimeout:     150 * time.Millisecond,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	body, _ := readAll(resp)
	if resp.StatusCode == http.StatusOK {
		var env struct {
			Data  v1.ChartSeries `json:"data"`
			Flags v1.Flags       `json:"flags"`
		}
		_ = json.Unmarshal([]byte(body), &env)
		t.Fatalf("status = 200 with %d points and stale=%v — a blown request deadline must never be "+
			"answered with a market-cap series a caller will read as fact",
			len(env.Data.Points), env.Flags.Stale)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a request deadline is retryable capacity): %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "chart-timeout") {
		t.Errorf("expected the `chart-timeout` problem type in the body, got: %s", body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestChart_MarketCap_ReadFailureFlagsStale covers the non-deadline half
// of the same degrade: a plain read failure still serves the empty series
// (the shape callers depend on), but flags.stale now says the series is
// empty because the read failed — not because the asset has no market cap.
func TestChart_MarketCap_ReadFailureFlagsStale(t *testing.T) {
	srv := v1.New(v1.Options{
		History:            &stubHistoryReader{pointsErr: errChartReadBroke},
		Supply:             &stubSupplyLooker{},
		VerifiedCurrencies: newTestCatalogue(t),
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the empty-series degrade is the documented shape)", resp.StatusCode)
	}
	var env struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Points) != 0 {
		t.Fatalf("points = %d, want 0", len(env.Data.Points))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a market-cap series emptied by a READ FAILURE — unflagged, " +
			"it claims the asset has no market cap, which is a different fact")
	}
}

// The fiat and fiat-cross legs shared the market-cap leg's shape exactly:
// a blown ListFXHistory budget fell through to an empty series at 200 with
// no flag. Same fix, same proof.
func TestChart_Fiat_RequestDeadlineIs503NotEmptySeries(t *testing.T) {
	srv := v1.New(v1.Options{
		History:            &stubHistoryReader{},
		FXHistory:          stallingFXHistory{},
		VerifiedCurrencies: newTestCatalogue(t),
		RequestTimeout:     150 * time.Millisecond,
	})
	ts := httpTestServer(t, srv)

	for _, q := range []string{
		"/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1y&granularity=1d",
		"/v1/chart?asset=fiat:EUR&quote=fiat:GBP&timeframe=1y&granularity=1d",
	} {
		resp := mustGet(t, ts.URL+q)
		body, _ := readAll(resp)
		if resp.StatusCode == http.StatusOK {
			var env struct {
				Data  v1.ChartSeries `json:"data"`
				Flags v1.Flags       `json:"flags"`
			}
			_ = json.Unmarshal([]byte(body), &env)
			t.Fatalf("%s: status = 200 with %d points and stale=%v — a blown fx read deadline must not be served as an empty series a caller reads as fact",
				q, len(env.Data.Points), env.Flags.Stale)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503: %s", q, resp.StatusCode, body)
		}
		if !strings.Contains(body, "chart-timeout") {
			t.Errorf("%s: expected the chart-timeout problem type, got: %s", q, body)
		}
	}
}

// TestChart_EmptySeriesCarriesCoverageFrom — /v1/chart's window always
// ends at now, so it can never sit BELOW the floor; the floor itself is
// the answer there. An empty 24h chart with `coverage_from: 2018-07-01`
// says "quiet"; the same chart with the field absent says "nothing is
// held for this pair".
func TestChart_EmptySeriesCarriesCoverageFrom(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := emptyHistoryServer(t, probe)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=crypto:XLM&quote=fiat:USD&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, resp, &env)
	if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Fatalf("coverage_from = %v, want %s", env.CoverageFrom, xlmCoverageFloor)
	}
	if env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = true on a window ending at now")
	}
}

// TestChart_FiatQuoteFloorIsTheProxySet — /v1/chart serves a fiat quote
// from its proxy list after the literal pair, so an empty 24h chart for
// AQUA/fiat:USD with only AQUA/USDC buckets says "quiet since the peg's
// floor", not "nothing held".
func TestChart_FiatQuoteFloorIsTheProxySet(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		probeKey(aqua, usdc): pegFloor2021,
	}}
	ts := fiatCoverageServer(t, probe)

	resp := mustGet(t, ts.URL+"/v1/chart?asset="+aquaClassicID+"&quote=fiat:USD&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, resp, &env)
	assertCoverage(t, env, &pegFloor2021, false)
}

// TestChart_XLMCrossFloorIsTheLaterLeg — the chart's last route derives
// a fiat series through XLM, bucket by bucket, so it exists only where
// BOTH legs do: its floor is the LATER leg's. Folding XLM's own fiat
// floor (2018) into the set as if it were a direct constituent would
// hand every asset with any XLM market a floor from before that asset
// existed. And when the asset leg holds nothing, the cross cannot be
// served at all, and XLM's floor must not surface through it.
func TestChart_XLMCrossFloorIsTheLaterLeg(t *testing.T) {
	t.Parallel()
	aqua := mustParseAsset(t, aquaClassicID)
	native := canonical.NativeAsset()
	usd := mustParseAsset(t, "fiat:USD")
	assetLegFloor := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	get := func(t *testing.T, probe *coverageFloorProbe) coverageMeta {
		t.Helper()
		ts := fiatCoverageServer(t, probe)
		resp := mustGet(t, ts.URL+"/v1/chart?asset="+aquaClassicID+"&quote=fiat:USD&timeframe=24h")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var env coverageMeta
		mustDecode(t, resp, &env)
		return env
	}

	t.Run("both legs held: the later leg is the floor", func(t *testing.T) {
		env := get(t, &coverageFloorProbe{byPair: map[string]time.Time{
			probeKey(aqua, native): assetLegFloor,
			probeKey(native, usd):  xlmCoverageFloor,
		}})
		assertCoverage(t, env, &assetLegFloor, false)
	})
	t.Run("asset leg absent: the cross has no floor", func(t *testing.T) {
		env := get(t, &coverageFloorProbe{byPair: map[string]time.Time{
			probeKey(native, usd): xlmCoverageFloor,
		}})
		assertCoverage(t, env, nil, false)
	})
}

func TestChart_DegradedFailureExitsAreNoStore(t *testing.T) {
	fxFail := &stubFXHistoryReader{err: errors.New("fx_quotes: broke")}
	cases := []struct {
		name, path string
		opts       v1.Options
	}{
		{
			"fiat fx failure", "/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{}, FXHistory: fxFail},
		},
		{
			"fiat-cross fx failure", "/v1/chart?asset=fiat:EUR&quote=fiat:GBP&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{}, FXHistory: fxFail},
		},
		{
			"market cap price read failure", "/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{pointsErr: errChartReadBroke}, Supply: &stubSupplyLooker{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.VerifiedCurrencies = newTestCatalogue(t)
			ts := httpTestServer(t, v1.New(tc.opts))
			assertNoStore(t, mustGet(t, ts.URL+tc.path))
		})
	}
}

// TestChart_NonstandardDecimals_NormalizesPriceNotVolumeUSD pins the
// /v1/chart contract (this endpoint is
// not declined; the raw prices_<gran> ratio is corrected:
//
//   - each point's `p`: raw CAGG ratio × K (10^(9−7) = 100 here).
//   - each point's `v_usd`: UNCHANGED — prices_<gran>.volume_usd is
//     Σ(usd_volume) (migration 0002), already USD-denominated at
//     trade-valuation time and invariant to the pair's decimals split.
func TestChart_NonstandardDecimals_NormalizesPriceNotVolumeUSD(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	vusd := "1234.56"
	reader := &stubHistoryReader{points: []v1.HistoryPoint{{
		Bucket: t0, VWAP: "41.32", VolumeUSD: &vusd,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/chart?asset="+flaggedAsset+"&quote="+classicUSDC+"&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"p":"4132.0000000000"`) {
		t.Errorf("chart body missing normalized point price 4132.0000000000: %s", body)
	}
	if !strings.Contains(body, `"v_usd":"1234.56"`) {
		t.Errorf("chart v_usd must be untouched (already USD-anchored): %s", body)
	}
}

// TestChart_NonstandardDecimals_7dpByteIdentical — wiring the cache must
// not reformat an unflagged pair's points.
func TestChart_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{points: []v1.HistoryPoint{{Bucket: t0, VWAP: "0.1242"}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote="+classicUSDC+"&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"p":"0.1242"`) {
		t.Errorf("7dp chart points must be byte-identical; body: %s", body)
	}
}

// TestChart_StablecoinFallback_ReachesPegSACTwin pins the USD-proxy walk
// on the PEG's own spellings, not just the classic one the operator typed
// into `[trades].usd_pegged_classic_assets`.
//
// A declared peg is an asset, not a spelling. Soroban AMMs (Aquarius,
// Phoenix, Soroswap) trade the SAC wrapper, so a pool's USD leg is stored
// quoted in the USDC SAC and never in USDC-GA5Z… — and the walk, bound to
// the classic form alone, read the one spelling the depth is not under.
// Measured on r1: a Soroban-traded asset returned 0 chart
// points against fiat:USD while the identical window quoted in the USDC
// SAC returned 39.
//
// RED without the fix: 0 points — the only pair holding the series
// (SAC base × SAC peg) is never read.
func TestChart_StablecoinFallback_ReachesPegSACTwin(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		// The Soroban pool's series: both legs in SAC form.
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
			{Bucket: t0.Add(time.Hour), VWAP: "0.0042"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the USD proxy must reach the peg's SAC form", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg, not the requested quote")
	}
	if env.Data.AssetID != pegAliasAquaClassic {
		t.Errorf("asset_id = %q, want %q (echo the requested form)", env.Data.AssetID, pegAliasAquaClassic)
	}
	// SAC LAST is a money-safety ordering, not a style choice: the walk
	// takes the first form that answers, so classic depth must be read
	// before a thin Soroban pool. Held to ONE base spelling so the
	// comparison isolates the peg's alias order — two reads that differ
	// on both sides would order themselves by the base loop alone.
	classicAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCClassic)
	sacAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCSAC)
	if classicAt < 0 || sacAt < 0 {
		t.Fatalf("walk missed a combination: classic-peg at %d, SAC-peg at %d (calls=%v)", classicAt, sacAt, reader.calls)
	}
	if classicAt > sacAt {
		t.Errorf("classic peg read at %d, after its SAC form at %d — SAC must be last", classicAt, sacAt)
	}
}

// TestChart_StablecoinFallback_EveryClassicPegBeforeAnySAC pins the
// ordering across the PEG dimension with the base held fixed: with two
// declared pegs, every peg's classic form is read before any peg's SAC
// form. A per-peg interleave ([pegA classic, pegA SAC, pegB classic])
// would read peg A's thin Soroban pool before peg B's deep classic book
// and re-price a series the classic-only walk already served from that
// book — the one outcome widening the walk must never produce.
//
// RED with a per-peg interleave: the thin pool (0.0041) is served in
// place of the deep book (0.0099).
func TestChart_StablecoinFallback_EveryClassicPegBeforeAnySAC(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		// Thin Soroban pool, quoted in the FIRST peg's SAC.
		pegAliasAquaClassic + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
		},
		// Deep classic book, quoted in the SECOND peg's classic form.
		pegAliasAquaClassic + "/" + pegAliasPYUSDClassic: {
			{Bucket: t0, VWAP: "0.0099"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc, pyusd}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(env.Data.Points))
	}
	if got := env.Data.Points[0].P; got != "0.0099" {
		t.Errorf("served %s — the second peg's classic book must be read before the first peg's SAC pool (0.0041)", got)
	}
	deepAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasPYUSDClassic)
	thinAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCSAC)
	if deepAt < 0 {
		t.Fatalf("second peg's classic form never read (calls=%v)", reader.calls)
	}
	if thinAt >= 0 && thinAt < deepAt {
		t.Errorf("first peg's SAC read at %d, before the second peg's classic form at %d (calls=%v)", thinAt, deepAt, reader.calls)
	}
}

// TestChart_StablecoinFallback_SkipsPegSpellingOfTheBase pins the other
// half of widening the walk: a base and a proxy quote that are the SAME
// asset in two spellings must not be read at all. Their pair has no rows
// by construction, and every combination shares the handler's one 8s
// deadline.
func TestChart_StablecoinFallback_SkipsPegSpellingOfTheBase(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	for _, call := range reader.calls {
		switch call {
		case pegAliasUSDCClassic + "/" + pegAliasUSDCSAC, pegAliasUSDCSAC + "/" + pegAliasUSDCClassic:
			t.Errorf("read %q — the two sides are one asset, never a market", call)
		}
	}
}

// TestChart_DeclaredPeg_FiatUSD_CrossesThroughXLM pins the numeraire's
// own dollar series. The declared USD peg has no USD-quoted buckets under
// any spelling — every USD series on chain is served by rewriting the
// quote to THIS asset — and the proxy walk cannot proxy the peg through
// itself, so USDC, the largest asset on the deployment, charted as an
// empty series beside a 24h volume in the tens of millions. Its dollar
// depth is the USDC/XLM book and the USDC-SAC/XLM-SAC pools; crossed
// with XLM's CEX-quoted dollar series bucket by bucket, that is the
// peg's actual traded dollar price.
//
// The fixture stores classic USDC's trades under its SAC identity
// (the Soroban pool: USDC-SAC quoted in the XLM SAC) and XLM's dollar
// series under `crypto:XLM`; the request names the classic id. Three
// pool buckets against two XLM buckets prove the join emits only buckets
// present on both legs. Prices are exact products rendered at 10
// digits: 5 × 0.2 and 5.05 × 0.198.
//
// RED without the cross: 0 points.
func TestChart_DeclaredPeg_FiatUSD_CrossesThroughXLM(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	vol0, vol1 := "12345.67", "890.12"
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
			{Bucket: t0, VWAP: "5", VolumeUSD: &vol0},
			{Bucket: t0.Add(time.Hour), VWAP: "5.05", VolumeUSD: &vol1},
			{Bucket: t0.Add(2 * time.Hour), VWAP: "5.1"},
		},
		"crypto:XLM/fiat:USD": {
			{Bucket: t0, VWAP: "0.2"},
			{Bucket: t0.Add(time.Hour), VWAP: "0.198"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the declared peg's USD series must be derived through XLM (calls=%v)", len(env.Data.Points), reader.calls)
	}
	if got := env.Data.Points[0].P; got != "1.0000000000" {
		t.Errorf("points[0].p = %s, want 1.0000000000 (5 × 0.2)", got)
	}
	if got := env.Data.Points[1].P; got != "0.9999000000" {
		t.Errorf("points[1].p = %s, want 0.9999000000 (5.05 × 0.198)", got)
	}
	if got := env.Data.Points[0].VUSD; got == nil || *got != vol0 {
		t.Errorf("points[0].v_usd = %v, want %s (the asset leg's own USD volume)", got, vol0)
	}
	if !env.Data.Points[1].T.Time().Equal(t0.Add(time.Hour)) {
		t.Errorf("points[1].t = %s, want %s", env.Data.Points[1].T, t0.Add(time.Hour))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; a series composed through XLM is derived, not traded")
	}
	if env.Data.AssetID != pegAliasUSDCClassic || env.Data.Quote != "fiat:USD" {
		t.Errorf("asset_id/quote = %q/%q, want %q/fiat:USD (echo the request)", env.Data.AssetID, env.Data.Quote, pegAliasUSDCClassic)
	}
}

// TestChart_FiatFallback_XLMCrossRunsLast pins where the cross sits in
// the chain: after every directly observed market. A pool quoted in the
// peg's SAC is a traded series and must be served as-is, and once it has
// answered the XLM legs are not read at all — a derived value never
// displaces an observed one, and never costs a read it cannot use.
//
// RED with the cross ahead of the proxy walk: the derived 0.0040000000
// is served in place of the pool's 0.0041.
func TestChart_FiatFallback_XLMCrossRunsLast(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
		},
		pegAliasAquaClassic + "/native": {
			{Bucket: t0, VWAP: "0.02"},
		},
		"crypto:XLM/fiat:USD": {
			{Bucket: t0, VWAP: "0.2"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(env.Data.Points))
	}
	if got := env.Data.Points[0].P; got != "0.0041" {
		t.Errorf("served %s — the observed pool (0.0041) must win over the XLM-derived value", got)
	}
	if at := callIndex(reader.calls, pegAliasAquaClassic+"/native"); at >= 0 {
		t.Errorf("XLM leg read at %d although a proxy already answered (calls=%v)", at, reader.calls)
	}
}

// TestChart_DeclaredPeg_ThroughXLM_LegReadFailureIsNotAnEmptySeries pins
// that a failed alias read on either leg of the derived through-XLM
// series is answered as the failure it is — 503 on a deadline, 500 on a
// store error — as the same failure on the requested pair's own read is,
// never as a cacheable, unflagged `200 points: []`.
//
// RED without the fix: 200 with 0 points and flags.stale=false.
func TestChart_DeclaredPeg_ThroughXLM_LegReadFailureIsNotAnEmptySeries(t *testing.T) {
	cases := []struct {
		name    string
		failFor func(base, quote string) error
		want    int
	}{
		{
			name: "asset leg deadline",
			failFor: func(base, quote string) error {
				if isUSDCSpelling(base) && isXLMSpelling(quote) {
					return context.DeadlineExceeded
				}
				return nil
			},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "pivot leg store error",
			failFor: func(base, quote string) error {
				if isXLMSpelling(base) && quote == "fiat:USD" {
					return errors.New("pq: connection reset")
				}
				return nil
			},
			want: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, throughXLMLegChartURL(t, true, tc.failFor))
			if resp.StatusCode != tc.want {
				var env chartEnvelope
				_ = json.NewDecoder(resp.Body).Decode(&env)
				t.Fatalf("status=%d, want %d (points=%d stale=%v) — a failed leg read was served as an empty series",
					resp.StatusCode, tc.want, len(env.Data.Points), env.Flags.Stale)
			}
		})
	}
}

// TestChart_DeclaredPeg_ThroughXLM_DegradedEmptyLegFlagsStale pins that
// a pivot leg left empty by failed PROXY reads (a degraded walk, not an
// answer-class error) marks the empty reply flags.stale=true: the empty
// series may be the failure's doing, not the market's.
//
// RED without the fix: flags.stale=false.
func TestChart_DeclaredPeg_ThroughXLM_DegradedEmptyLegFlagsStale(t *testing.T) {
	url := throughXLMLegChartURL(t, false, func(base, quote string) error {
		if isXLMSpelling(base) && isUSDCSpelling(quote) {
			return errors.New("pq: connection reset")
		}
		return nil
	})
	env := getChart(t, url)
	if len(env.Data.Points) != 0 {
		t.Fatalf("got %d points, want 0", len(env.Data.Points))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false; a cross left empty by a failed leg read must be flagged degraded")
	}
}
