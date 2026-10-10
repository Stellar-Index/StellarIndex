package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubPriceReader implements v1.PriceReader.
type stubPriceReader struct {
	// Lookup keyed on "<base>/<quote>".
	snapshots map[string]v1.PriceSnapshot
	stale     map[string]bool
	sources   map[string][]string
	// recent is the per-pair RecentClosedSnapshots history, same key shape.
	// A missing key yields no observations, like the production reader.
	recent map[string][]v1.PriceSnapshot
	// err fails EVERY call; errByPair fails only the listed pairs, for
	// states where one pair is refused while a fallback leg still serves.
	err       error
	errByPair map[string]error

	// calls counts LatestPrice invocations (coalescing check).
	calls int32
	// startedCh is signaled on entry to every LatestPrice call and releaseCh
	// is read before it returns, holding N concurrent calls open at once.
	startedCh chan struct{}
	releaseCh chan struct{}
}

func (r *stubPriceReader) LatestPrice(_ context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	atomic.AddInt32(&r.calls, 1)
	if r.startedCh != nil {
		r.startedCh <- struct{}{}
	}
	if r.releaseCh != nil {
		<-r.releaseCh
	}
	if r.err != nil {
		return v1.PriceSnapshot{}, nil, false, r.err
	}
	key := a.String() + "/" + q.String()
	if err := r.errByPair[key]; err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}
	snap, ok := r.snapshots[key]
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return snap, r.sources[key], r.stale[key], nil
}

func (r *stubPriceReader) RecentClosedSnapshots(_ context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	if r.err != nil {
		return nil, r.err
	}
	key := a.String() + "/" + q.String()
	if err := r.errByPair[key]; err != nil {
		return nil, err
	}
	rows, ok := r.recent[key]
	if !ok {
		return []v1.PriceSnapshot{}, nil
	}
	if n < len(rows) {
		rows = rows[:n]
	}
	return rows, nil
}

const pathNativeUSD = "/v1/price?asset=native&quote=fiat:USD"

// usdcPeg is the declared classic USD peg the stablecoin proxy tests use.
var usdcPeg = mustClassicTest("USDC", testUSDCIssuer)

// priceGet serves path from a server built with opts.
func priceGet(t *testing.T, opts v1.Options, path string) (int, string) {
	t.Helper()
	return getBody(t, startHTTPTest(t, v1.New(opts).Handler()).URL+path)
}

// checkBody asserts every want is in body and no absent is.
func checkBody(t *testing.T, body string, want, absent []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q: %s", s, body)
		}
	}
	for _, s := range absent {
		if strings.Contains(body, s) {
			t.Errorf("body must not contain %q: %s", s, body)
		}
	}
}

// usdReader serves snap for native/fiat:USD with the given sources.
func usdReader(snap v1.PriceSnapshot, sources ...string) *stubPriceReader {
	r := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap}}
	if sources != nil {
		r.sources = map[string][]string{"native/fiat:USD": sources}
	}
	return r
}

func TestPrice_RequestErrors(t *testing.T) {
	notFound := func() *stubPriceReader { return &stubPriceReader{err: v1.ErrPriceNotFound} }
	for _, tc := range []struct {
		name    string
		opts    v1.Options
		path    string
		status  int
		want    string // substring the body must carry
		notWant string // substring it must not
	}{
		{name: "no reader", opts: v1.Options{}, path: pathNativeUSD, status: 503, want: "price-unavailable"},
		{name: "missing asset", opts: v1.Options{Prices: &stubPriceReader{}}, path: "/v1/price", status: 400},
		{name: "invalid asset", opts: v1.Options{Prices: &stubPriceReader{}}, path: "/v1/price?asset=garbage-format", status: 400},
		{name: "identity pair", opts: v1.Options{Prices: &stubPriceReader{}}, path: "/v1/price?asset=native&quote=native", status: 400, want: "identity-price"},
		{name: "not found", opts: v1.Options{Prices: notFound()}, path: pathNativeUSD, status: 404},
		{name: "internal error is not leaked", opts: v1.Options{Prices: &stubPriceReader{err: errors.New("db timeout")}}, path: pathNativeUSD, status: 500, notWant: "db timeout"},
		{
			name: "cache miss keeps 404",
			opts: v1.Options{Prices: notFound(), Triangulated: &stubTriangulatedPriceLooker{found: false}},
			path: pathNativeUSD, status: 404,
		},
		{
			name: "proxy: non-USD quote skips the USD peg",
			opts: v1.Options{Prices: notFound(), USDPeggedClassics: []canonical.Asset{usdcPeg}},
			path: "/v1/price?asset=native&quote=fiat:EUR", status: 404,
		},
		{
			name: "proxy: USDC/EUR is a cross-rate, not a $1 peg",
			opts: v1.Options{Prices: notFound()},
			path: "/v1/price?asset=crypto:USDC&quote=fiat:EUR", status: 404,
		},
		{
			name: "fiat cross fallback needs both sides fiat",
			opts: v1.Options{Prices: notFound(), Currencies: &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
				Currencies: []v1.CurrencyEntry{{Ticker: "EUR", RateUSD: 0.92}},
			}}},
			path: pathNativeUSD, status: 404,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := priceGet(t, tc.opts, tc.path)
			if status != tc.status {
				t.Errorf("status = %d, want %d: %s", status, tc.status, body)
			}
			var want, notWant []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if tc.notWant != "" {
				notWant = []string{tc.notWant}
			}
			checkBody(t, body, want, notWant)
		})
	}
}

func TestPrice_Served(t *testing.T) {
	happy := usdReader(v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.1242", PriceType: "last_trade",
		ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
	}, "sdex")
	stale := usdReader(v1.PriceSnapshot{Price: "0.1242", PriceType: "last_trade"})
	stale.stale = map[string]bool{"native/fiat:USD": true}
	for _, tc := range []struct {
		name   string
		reader *stubPriceReader
		path   string
		want   []string
	}{
		{"happy path", happy, pathNativeUSD, []string{`"price":"0.1242"`, `"price_type":"last_trade"`, `"stale":false`, `"sources":["sdex"]`}},
		{"stale flag", stale, pathNativeUSD, []string{`"stale":true`}},
		{"quote defaults to USD", usdReader(v1.PriceSnapshot{Price: "0.12"}), "/v1/price?asset=native", []string{`"price":"0.12"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := priceGet(t, v1.Options{Prices: tc.reader}, tc.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d: %s", status, body)
			}
			checkBody(t, body, tc.want, nil)
		})
	}
}

// stubTriangulatedPriceLooker implements v1.TriangulatedPriceLooker. Cache
// hits must be honoured whether or not the provenance marker is present:
// direct stablecoin-fiat-proxy rewrites carry none but still surface.
type stubTriangulatedPriceLooker struct {
	value          string
	isTriangulated bool
	found          bool
	err            error
}

func (s *stubTriangulatedPriceLooker) LookupTriangulatedVWAP(
	_ context.Context, _, _ canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{Value: s.value, Triangulated: s.isTriangulated, ObservedAt: time.Now().UTC()}, s.found, s.err
}

// Every priceFallback degradation MUST surface flags.stale=true: the
// fallback chain is itself the staleness signal.
func TestPrice_RedisVWAPFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		looker *stubTriangulatedPriceLooker
		want   []string
	}{
		{
			name: "direct rewrite has no triangulation marker", path: pathNativeUSD,
			looker: &stubTriangulatedPriceLooker{value: "0.1242", found: true},
			want:   []string{`"price":"0.1242"`, `"price_type":"vwap"`, `"triangulated":false`, `"stale":true`},
		},
		{
			name: "triangulated sets the flag", path: "/v1/price?asset=crypto:XLM&quote=fiat:EUR",
			looker: &stubTriangulatedPriceLooker{value: "0.5500", isTriangulated: true, found: true},
			want:   []string{`"price":"0.5500"`, `"triangulated":true`, `"stale":true`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := priceGet(t, v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}, Triangulated: tc.looker}, tc.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			checkBody(t, body, tc.want, nil)
		})
	}
}

// stubCompositeMetaLooker implements v1.TriangulatedPriceLooker and the
// optional v1.CompositeMetaLooker, which carries the router's quality meta.
type stubCompositeMetaLooker struct {
	value          string
	isTriangulated bool
	found          bool
	metaRaw        []byte
	metaFound      bool
	metaErr        error
}

func (s *stubCompositeMetaLooker) LookupTriangulatedVWAP(
	_ context.Context, _, _ canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{Value: s.value, Triangulated: s.isTriangulated, ObservedAt: time.Now().UTC()}, s.found, nil
}

func (s *stubCompositeMetaLooker) LookupCompositeMeta(
	_ context.Context, _, _ canonical.Asset, _ time.Duration,
) ([]byte, bool, error) {
	return s.metaRaw, s.metaFound, s.metaErr
}

// The aggregator's composite-quality meta (diverged, rerouted,
// pivot_unverified) surfaces as omitempty envelope flags.
func TestPrice_TriangulatedCompositeFlags(t *testing.T) {
	tri := func(meta string) *stubCompositeMetaLooker {
		return &stubCompositeMetaLooker{value: "0.5500", isTriangulated: true, found: true, metaRaw: []byte(meta), metaFound: meta != ""}
	}
	notFound := &stubPriceReader{err: v1.ErrPriceNotFound}
	for _, tc := range []struct {
		name   string
		reader *stubPriceReader
		looker *stubCompositeMetaLooker
		want   []string
		absent []string
	}{
		{
			name: "diverged and rerouted both surface", reader: notFound,
			looker: tri(`{"path_count":2,"combined_confidence":0.81,"low_confidence":false,"diverged":true,"rerouted":true}`),
			want:   []string{`"triangulated":true`, `"diverged":true`, `"rerouted":true`},
		},
		{
			name: "rerouted omitted when false", reader: notFound,
			looker: tri(`{"diverged":true,"rerouted":false}`),
			want:   []string{`"diverged":true`}, absent: []string{`"rerouted"`},
		},
		{
			name: "pivot_unverified surfaces", reader: notFound,
			looker: tri(`{"pivot_proxy_share":{"crypto:XLM/fiat:USD":1},"pivot_unverified":true}`),
			want:   []string{`"pivot_unverified":true`},
		},
		{
			name: "pivot_unverified omitted when false", reader: notFound,
			looker: tri(`{"pivot_proxy_share":{"crypto:XLM/fiat:USD":0.4}}`),
			absent: []string{`"pivot_unverified"`},
		},
		{
			name: "no meta leaves both flags unset", reader: notFound, looker: tri(""),
			want: []string{`"triangulated":true`}, absent: []string{`"diverged"`, `"rerouted"`},
		},
		{
			// A configured router target can also carry a genuine closed bucket;
			// the meta must surface whichever arm served.
			name: "direct serve still surfaces the meta",
			reader: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
				"crypto:XLM/fiat:EUR": {Price: "0.0900", PriceType: "last_trade"},
			}},
			looker: &stubCompositeMetaLooker{metaRaw: []byte(`{"diverged":true,"rerouted":true}`), metaFound: true},
			want:   []string{`"triangulated":false`, `"diverged":true`, `"rerouted":true`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := priceGet(t, v1.Options{Prices: tc.reader, Triangulated: tc.looker}, "/v1/price?asset=crypto:XLM&quote=fiat:EUR")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			checkBody(t, body, tc.want, tc.absent)
		})
	}
}

// With no operator-enabled proxy, the handler walks the declared classic
// pegs and serves the first one with a row, echoing the requested quote.
func TestPrice_StablecoinFiatProxy_FallsThroughToClassicPeg(t *testing.T) {
	pegKey := "native/" + usdcPeg.String()
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{pegKey: {
			AssetID: "native", Quote: usdcPeg.String(), Price: "0.1626", PriceType: "vwap",
			ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
		}},
		sources: map[string][]string{pegKey: {"sdex"}},
	}
	status, body := priceGet(t, v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdcPeg}}, pathNativeUSD)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	checkBody(t, body, []string{`"price":"0.1626"`, `"price_type":"vwap"`, `"quote":"fiat:USD"`, `"triangulated":true`, `"sources":["sdex"]`}, nil)
}

// A stablecoin priced in the fiat it tracks is a synthetic $1 peg, not a
// 404: the classic-issued peg via USDPeggedClassics, and the abstract
// crypto:<TICKER> form via the aggregate.FiatProxy arm with no operator config.
func TestPrice_StablecoinFiatProxy_SelfPegReturnsOne(t *testing.T) {
	for _, tc := range []struct {
		opts         v1.Options
		asset, quote string
	}{
		{v1.Options{USDPeggedClassics: []canonical.Asset{usdcPeg}}, usdcPeg.String(), "fiat:USD"},
		{v1.Options{}, "crypto:USDC", "fiat:USD"},
		{v1.Options{}, "crypto:USDT", "fiat:USD"},
		{v1.Options{}, "crypto:EURC", "fiat:EUR"},
	} {
		tc.opts.Prices = &stubPriceReader{err: v1.ErrPriceNotFound}
		status, body := priceGet(t, tc.opts, "/v1/price?asset="+tc.asset+"&quote="+tc.quote)
		if status != http.StatusOK {
			t.Fatalf("%s/%s: status = %d, want 200: %s", tc.asset, tc.quote, status, body)
		}
		var resp struct {
			Data v1.PriceSnapshot `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("%s/%s: decode: %v", tc.asset, tc.quote, err)
		}
		if resp.Data.Price != "1.000000000000" {
			t.Errorf("%s/%s: price = %q, want 1.000000000000", tc.asset, tc.quote, resp.Data.Price)
		}
		if resp.Data.PriceType != "peg" {
			t.Errorf("%s/%s: price_type = %q, want peg", tc.asset, tc.quote, resp.Data.PriceType)
		}
		if resp.Data.Quote != tc.quote {
			t.Errorf("%s/%s: quote = %q, want %s", tc.asset, tc.quote, resp.Data.Quote, tc.quote)
		}
	}
}

// stubDivergenceLooker is a minimal v1.DivergenceLooker. A zero window is
// "unrecorded".
type stubDivergenceLooker struct {
	firing  bool
	checked bool
	window  time.Duration
	err     error
	calls   int
}

func (s *stubDivergenceLooker) DivergenceFiringFor(_ context.Context, _, _ canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	s.calls++
	return s.firing, s.checked, s.window, s.err
}

// stubConfidenceLooker is a minimal v1.ConfidenceLooker.
type stubConfidenceLooker struct {
	score v1.PriceSnapshotConfidence
	found bool
	err   error
	calls int
}

func (s *stubConfidenceLooker) LookupConfidence(_ context.Context, _, _ canonical.Asset, _ time.Duration) (v1.PriceSnapshotConfidence, bool, error) {
	s.calls++
	return s.score, s.found, s.err
}

func TestPrice_Confidence(t *testing.T) {
	snap := v1.PriceSnapshot{Price: "0.07", PriceType: "vwap"}
	substituted := snap
	// The staple is looked up from a cache keyed only by (asset, quote,
	// window), so it would describe the current tick, not the older
	// last-known-good bucket a substituted snapshot actually serves.
	substituted.Substituted = true
	full := v1.PriceSnapshotConfidence{
		Confidence: 0.92,
		Factors:    v1.ConfidenceFactors{ZScore: 0.95, SourceCount: 0.95, Diversity: 1.0, Liquidity: 1.0, CrossOracle: 1.0, BaselineQuality: 1.0},
	}
	for _, tc := range []struct {
		name      string
		snap      v1.PriceSnapshot
		conf      *stubConfidenceLooker // nil: none wired
		wantCalls int                   // -1: not asserted
		want      []string
		absent    []string
	}{
		{
			"cached score reaches the wire", snap, &stubConfidenceLooker{score: full, found: true}, 1,
			[]string{`"confidence":0.92`, `"confidence_factors"`, `"baseline_quality":1`},
			nil,
		},
		{
			"substituted snapshot skips the staple", substituted, &stubConfidenceLooker{score: full, found: true}, 0,
			nil,
			[]string{`"confidence"`},
		},
		{
			"cache miss omits the fields", snap, &stubConfidenceLooker{}, -1,
			nil,
			[]string{`"confidence"`, `"confidence_factors"`},
		},
		{
			"lookup error is best effort", snap, &stubConfidenceLooker{err: errors.New("redis exploded")}, -1,
			[]string{`"price":"0.07"`},
			[]string{`"confidence"`},
		},
		{"no looker wired", snap, nil, -1, nil, []string{`"confidence"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Prices: usdReader(tc.snap)}
			if tc.conf != nil {
				opts.Confidence = tc.conf
			}
			status, body := priceGet(t, opts, pathNativeUSD)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			checkBody(t, body, tc.want, tc.absent)
			if tc.conf != nil && tc.wantCalls >= 0 && tc.conf.calls != tc.wantCalls {
				t.Errorf("looker calls = %d, want %d", tc.conf.calls, tc.wantCalls)
			}
		})
	}
}

func TestPrice_Divergence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		div       *stubDivergenceLooker // nil: none wired
		want      string
		wantCalls int
	}{
		// An evaluated (checked) firing verdict ends the alias walk.
		{"firing", &stubDivergenceLooker{firing: true, checked: true}, `"divergence_warning":true`, 1},
		{"clean", &stubDivergenceLooker{}, `"divergence_warning":false`, 1},
		{"no looker", nil, `"divergence_warning":false`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Prices: usdReader(v1.PriceSnapshot{Price: "0.07", PriceType: "vwap"})}
			if tc.div != nil {
				opts.Divergence = tc.div
			}
			_, body := priceGet(t, opts, pathNativeUSD)
			checkBody(t, body, []string{tc.want}, nil)
			if tc.div != nil && tc.div.calls != tc.wantCalls {
				t.Errorf("divergence lookup calls = %d, want %d", tc.div.calls, tc.wantCalls)
			}
		})
	}
}

// divVerdict is one cached divergence verdict.
type divVerdict = struct{ firing, checked bool }

// stubAliasDivergenceLooker answers per SPELLING, so a test can put the
// cached verdict under one member of an alias family and query another —
// the r1 shape, where the worker refreshes `crypto:XLM/fiat:USD` and
// nothing is ever written under `native`.
type stubAliasDivergenceLooker struct {
	// verdicts is keyed on the asset's wire form; an absent spelling is a
	// cache miss, as with the production adapter for an unindexed base.
	verdicts map[string]divVerdict
	// asked records every spelling consulted, in order; guarded because the
	// tip stream consults the looker from its producer goroutine.
	mu    sync.Mutex
	asked []string
	// window is every verdict's recorded aggregation window.
	window time.Duration
}

func (s *stubAliasDivergenceLooker) DivergenceFiringFor(_ context.Context, a, _ canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	s.mu.Lock()
	s.asked = append(s.asked, a.String())
	s.mu.Unlock()
	v := s.verdicts[a.String()]
	return v.firing, v.checked, s.window, nil
}

func (s *stubAliasDivergenceLooker) askedSpellings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// The verdict must describe the market the price was served from: it is
// asked for the served spelling first, and a sibling spelling's verdict is
// never borrowed to vouch for a price it never saw.
func TestPrice_DivergenceVerdictFollowsServedSpelling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		servedKey string
		verdicts  map[string]divVerdict
		want      []string
		asked     []string // nil: not asserted
	}{
		{
			name: "served crypto:XLM market's clean verdict", servedKey: "crypto:XLM/fiat:USD",
			verdicts: map[string]divVerdict{"crypto:XLM": {checked: true}},
			want:     []string{`"divergence_checked":true`, `"divergence_warning":false`},
			asked:    []string{"crypto:XLM"},
		},
		{
			name: "clean sibling verdict never reaches a native-served price", servedKey: "native/fiat:USD",
			verdicts: map[string]divVerdict{"crypto:XLM": {checked: true}},
			want:     []string{`"divergence_checked":false`, `"divergence_warning":false`},
			asked:    []string{"native"},
		},
		{
			name: "firing sibling verdict never reaches a native-served price", servedKey: "native/fiat:USD",
			verdicts: map[string]divVerdict{"crypto:XLM": {firing: true, checked: true}},
			want:     []string{`"divergence_checked":false`, `"divergence_warning":false`},
			asked:    []string{"native"},
		},
		{
			// A below-quorum record carries the last evaluated warning forward
			// (firing, unchecked); a sibling's fresh clean verdict must not replace it.
			name: "standing warning on the served spelling", servedKey: "native/fiat:USD",
			verdicts: map[string]divVerdict{"native": {firing: true}, "crypto:XLM": {checked: true}},
			want:     []string{`"divergence_checked":false`, `"divergence_warning":true`},
		},
		{
			name: "walk starts at the served alias, not the requested spelling", servedKey: "crypto:XLM/fiat:USD",
			verdicts: map[string]divVerdict{"native": {firing: true, checked: true}, "crypto:XLM": {checked: true}},
			want:     []string{`"divergence_warning":false`},
			asked:    []string{"crypto:XLM"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			div := &stubAliasDivergenceLooker{verdicts: tc.verdicts}
			reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
				tc.servedKey: {Price: "0.18726015145022901497", PriceType: "vwap"},
			}}
			status, body := priceGet(t, v1.Options{Prices: reader, Divergence: div}, pathNativeUSD)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			checkBody(t, body, tc.want, nil)
			if asked := div.askedSpellings(); tc.asked != nil && !reflect.DeepEqual(asked, tc.asked) {
				t.Errorf("spellings asked = %v, want %v", asked, tc.asked)
			}
		})
	}
}

// failingDivergenceLooker fails every lookup and counts the attempts —
// the shape of a store that is down for every spelling at once.
type failingDivergenceLooker struct{ calls atomic.Int32 }

func (f *failingDivergenceLooker) DivergenceFiringFor(context.Context, canonical.Asset, canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	f.calls.Add(1)
	return false, false, 0, errors.New("redis exploded")
}

// The spellings share one backing store, so the walk ends at the first
// error: an outage costs one round-trip per request, not one per alias. The
// price still flows with the flags left false.
func TestPrice_DivergenceLookupErrorStopsTheWalk(t *testing.T) {
	div := &failingDivergenceLooker{}
	status, body := priceGet(t, v1.Options{Prices: usdReader(v1.PriceSnapshot{Price: "0.07", PriceType: "vwap"}), Divergence: div}, pathNativeUSD)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — divergence error must NOT fail the price call: %s", status, body)
	}
	checkBody(t, body, []string{`"divergence_checked":false`, `"divergence_warning":false`}, nil)
	if n := div.calls.Load(); n != 1 {
		t.Errorf("lookups on a failing store = %d, want 1 (walk stops at the first error, not each of the %d spellings)",
			n, len(canonical.AssetAliases(canonical.NativeAsset())))
	}
}

// VWAP1m is the CAGG-served counterpart to LastTradeToSnapshot: ObservedAt
// is the END of the 1-minute window and the NUMERIC text passes through.
func TestVWAP1mToSnapshot(t *testing.T) {
	bucketStart := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	got := v1.VWAP1mToSnapshot("native", "fiat:USD", "0.123456789", bucketStart)

	if got.AssetID != "native" || got.Quote != "fiat:USD" {
		t.Errorf("pair = %q/%q, want native/fiat:USD", got.AssetID, got.Quote)
	}
	if got.Price != "0.123456789" {
		t.Errorf("Price = %q, want pass-through of NUMERIC text 0.123456789", got.Price)
	}
	if got.PriceType != "vwap" {
		t.Errorf("PriceType = %q, want vwap", got.PriceType)
	}
	if got.WindowSeconds != 60 {
		t.Errorf("WindowSeconds = %d, want 60", got.WindowSeconds)
	}
	if want := bucketStart.Add(60 * time.Second); !got.ObservedAt.Time().Equal(want) {
		t.Errorf("ObservedAt = %v, want %v (END of window, not start)", got.ObservedAt, want)
	}
}

func TestLastTradeToSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name             string
		base, quote      int64
		decimals         int
		wantPrice        string
		wantNotPriceable bool
	}{
		// 12_420_000 / 1_000_000_000 scaled to 7 decimals = 0.0124200.
		{name: "7 decimals", base: 1_000_000_000, quote: 12_420_000, decimals: 7, wantPrice: "0.0124200"},
		{name: "zero decimals", base: 1_000, quote: 12_420, decimals: 0, wantPrice: "12"},
		// A zero-leg trade has no price: the snapshot is refused, never "0".
		{name: "zero quote", base: 5_000_000_000, decimals: 7, wantNotPriceable: true},
		{name: "zero base", quote: 7_000_000, decimals: 7, wantNotPriceable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := canonical.Trade{
				Source: "sdex", Ledger: 52_430_001, Timestamp: time.Unix(1745000000, 0).UTC(),
				TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
				Pair:        mustPair(canonical.NativeAsset(), usdcPeg),
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.base)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quote)),
			}
			snap, ok := v1.LastTradeToSnapshot(tr, tc.decimals)
			if ok == tc.wantNotPriceable {
				t.Fatalf("priceable = %v (snapshot %+v), want %v", ok, snap, !tc.wantNotPriceable)
			}
			if tc.wantNotPriceable {
				return
			}
			if snap.AssetID != "native" || snap.PriceType != "last_trade" {
				t.Errorf("asset/price_type = %q/%q, want native/last_trade", snap.AssetID, snap.PriceType)
			}
			if snap.Price != tc.wantPrice {
				t.Errorf("price = %q, want %s", snap.Price, tc.wantPrice)
			}
			if !snap.ObservedAt.Time().Equal(tr.Timestamp) {
				t.Errorf("timestamp lost")
			}
		})
	}
}

// stubFrozenLooker implements v1.FrozenLooker.
type stubFrozenLooker struct {
	frozen bool
	err    error
	calls  int
}

func (s *stubFrozenLooker) FrozenForPair(_ context.Context, _, _ canonical.Asset) (bool, error) {
	s.calls++
	return s.frozen, s.err
}

// A frozen response IS the held last-known-good, which is by definition
// single-sourced (anomaly.ActionFreeze), however many sources the bucket has.
func TestPrice_FrozenSetsBothFlags(t *testing.T) {
	reader := usdReader(v1.PriceSnapshot{Price: "0.07", PriceType: "vwap"}, "sdex", "soroswap", "binance")
	frz := &stubFrozenLooker{frozen: true}
	// The fixture must hold a last-known-good: a freeze with no VWAP cache is
	// a shape production cannot take and would certify the raw 0.07 being
	// served under frozen=true.
	held := &stubTriangulatedPriceLooker{value: "0.0655", found: true}

	_, body := priceGet(t, v1.Options{Prices: reader, Freeze: frz, Triangulated: held}, pathNativeUSD)
	checkBody(t, body, []string{`"frozen":true`, `"single_source":true`, `"price":"0.0655"`}, []string{`"0.07"`})
	if frz.calls != 1 {
		t.Errorf("freeze lookup calls = %d, want 1", frz.calls)
	}
}

// When not frozen, single_source mirrors len(sources)==1 (omitempty when
// false), with or without a FrozenLooker wired.
func TestPrice_NotFrozenSingleSourceFromSourceCount(t *testing.T) {
	one := v1.PriceSnapshot{Price: "0.07", PriceType: "last_trade"}
	for _, tc := range []struct {
		name    string
		sources []string
		freeze  *stubFrozenLooker // nil: none wired
		want    []string
		absent  []string
	}{
		{"one source", []string{"sdex"}, &stubFrozenLooker{}, []string{`"single_source":true`}, []string{`"frozen":true`}},
		{"two sources", []string{"sdex", "soroswap"}, &stubFrozenLooker{}, nil, []string{`"single_source":true`, `"frozen":true`}},
		{"no looker, one source", []string{"sdex"}, nil, []string{`"single_source":true`}, []string{`"frozen":true`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Prices: usdReader(one, tc.sources...)}
			if tc.freeze != nil {
				opts.Freeze = tc.freeze
			}
			_, body := priceGet(t, opts, pathNativeUSD)
			checkBody(t, body, tc.want, tc.absent)
		})
	}
}

// A freeze lookup error means the status is UNKNOWN, not "confirmed not
// frozen": the price still flows, but single_source must not be derived
// from the source count and frozen_checked must reflect the failed read.
func TestPrice_FreezeErrorIsBestEffortAndUnasserted(t *testing.T) {
	reader := usdReader(v1.PriceSnapshot{Price: "0.07", PriceType: "vwap"}, "sdex")
	before := testutil.ToFloat64(obs.APIFreezeLookupFailuresTotal)
	frz := &stubFrozenLooker{err: errors.New("redis exploded")}

	status, body := priceGet(t, v1.Options{Prices: reader, Freeze: frz}, pathNativeUSD)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — freeze lookup error must NOT fail the price call", status)
	}
	checkBody(t, body, nil, []string{`"frozen":true`, `"single_source":true`, `"frozen_checked":true`})
	if got := testutil.ToFloat64(obs.APIFreezeLookupFailuresTotal) - before; frz.calls == 0 || got != float64(frz.calls) {
		t.Errorf("freeze lookup failure counter delta = %v, want %d (one per failed lookup)", got, frz.calls)
	}
}

// batchFreezeLooker freezes any pair whose base.Code matches frozenForBase.
type batchFreezeLooker struct {
	frozenForBase string
}

func (b *batchFreezeLooker) FrozenForPair(_ context.Context, asset, _ canonical.Asset) (bool, error) {
	return asset.Code == b.frozenForBase, nil
}

func mustPair(base, quote canonical.Asset) canonical.Pair {
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		panic(err)
	}
	return p
}

func mustClassicTest(code, issuer string) canonical.Asset {
	a, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		panic(err)
	}
	return a
}

// stubCurrenciesReader is the test seam for the fiat-cross-rate fallback;
// a nil snap is the "warming up" branch.
type stubCurrenciesReader struct {
	snap *v1.CurrenciesSnapshot
}

func (s *stubCurrenciesReader) Latest() *v1.CurrenciesSnapshot { return s.snap }

// With both sides fiat and no direct trades (the steady state on Stellar),
// the handler synthesises a cross rate from the forex snapshot and flags it
// triangulated. EUR rate_usd=0.92 means 1 EUR = 1/0.92 = ~1.0869565 USD.
func TestPrice_FiatCrossRate_EURUSD(t *testing.T) {
	now := time.Now().UTC()
	currencies := &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies:  []v1.CurrencyEntry{{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: now}},
		PublishedAt: now,
	}}
	fixings := fixingsOf(hourlyFixing("EUR", "0.92", now.Add(-timescale.FXFixingLag).Truncate(time.Hour)))

	status, body := priceGet(t, v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}, Currencies: currencies, FXFixings: fixings},
		"/v1/price?asset=fiat:EUR&quote=fiat:USD")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fiat cross-rate fallback): %s", status, body)
	}
	// The digits past 1.086 depend on strconv.FormatFloat's shortest round-trip form.
	checkBody(t, body, []string{`"asset_id":"fiat:EUR"`, `"quote":"fiat:USD"`, `"price_type":"vwap"`, `"triangulated":true`, `"price":"1.086`}, nil)
}

// SDEX writes `native` and CEX writes `crypto:XLM`; the public surface must
// serve whichever spelling holds the VWAP, preferring a fresh one.
func TestPrice_XLMAlias(t *testing.T) {
	xlm := v1.PriceSnapshot{AssetID: "crypto:XLM", Quote: "fiat:USD", Price: "0.1500", PriceType: "vwap", ObservedAt: v1.WireTime(time.Now().UTC())}
	native := v1.PriceSnapshot{AssetID: "native", Quote: "fiat:USD", Price: "0.1500", PriceType: "vwap", ObservedAt: v1.WireTime(time.Now().UTC())}
	staleNative := v1.PriceSnapshot{AssetID: "native", Quote: "fiat:USD", Price: "0.1000", PriceType: "vwap", ObservedAt: v1.WireTime(time.Now().Add(-48 * time.Hour).UTC())}
	for _, tc := range []struct {
		name      string
		snapshots map[string]v1.PriceSnapshot
		stale     map[string]bool
		path      string
	}{
		{"native falls through to crypto:XLM", map[string]v1.PriceSnapshot{"crypto:XLM/fiat:USD": xlm}, nil, pathNativeUSD},
		{"crypto:XLM falls through to native", map[string]v1.PriceSnapshot{"native/fiat:USD": native}, nil, "/v1/price?asset=crypto:XLM&quote=fiat:USD"},
		{
			"fresh alias beats stale literal",
			map[string]v1.PriceSnapshot{"native/fiat:USD": staleNative, "crypto:XLM/fiat:USD": xlm},
			map[string]bool{"native/fiat:USD": true},
			pathNativeUSD,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := priceGet(t, v1.Options{Prices: &stubPriceReader{snapshots: tc.snapshots, stale: tc.stale}}, tc.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			checkBody(t, body, []string{`"price":"0.1500"`}, []string{`"stale":true`, `"triangulated":true`})
		})
	}
}

// gatingStubPriceReader also implements the optional proxyPairGate
// (RecentClosedVWAP1mExists) the stablecoin proxy consults to skip empty
// pegs before the unbounded last-trade walk. latestCalls records which
// pairs reached LatestPrice, proving a gated-out peg is never walked.
type gatingStubPriceReader struct {
	stubPriceReader
	exists      map[string]bool
	latestCalls map[string]int
}

func (r *gatingStubPriceReader) RecentClosedVWAP1mExists(_ context.Context, base, quote canonical.Asset) (bool, error) {
	return r.exists[base.String()+"/"+quote.String()], nil
}

func (r *gatingStubPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if r.latestCalls != nil {
		r.latestCalls[a.String()+"/"+q.String()]++
	}
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// A peg with no recent closed VWAP bucket must be skipped BEFORE
// LatestPrice, which on a classic-peg quote falls into an unbounded
// last-trade walk. Empty peg first: it is never walked and the live one serves.
func TestPrice_StablecoinProxy_GateSkipsEmptyPeg(t *testing.T) {
	emptyPeg := mustClassicTest("USDT", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	liveKey := "native/" + usdcPeg.String()
	reader := &gatingStubPriceReader{
		stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{liveKey: {
				AssetID: "native", Quote: usdcPeg.String(), Price: "0.1626", PriceType: "vwap",
				ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			}},
			sources: map[string][]string{liveKey: {"sdex"}},
		},
		exists:      map[string]bool{liveKey: true},
		latestCalls: map[string]int{},
	}
	status, body := priceGet(t, v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{emptyPeg, usdcPeg}}, pathNativeUSD)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (live peg should triangulate): %s", status, body)
	}
	checkBody(t, body, []string{`"price":"0.1626"`, `"quote":"fiat:USD"`, `"triangulated":true`}, nil)
	if n := reader.latestCalls["native/"+emptyPeg.String()]; n != 0 {
		t.Errorf("empty peg walked %d times, want 0 (gate must skip it)", n)
	}
	if n := reader.latestCalls[liveKey]; n != 1 {
		t.Errorf("live peg walked %d times, want 1", n)
	}
}

// With EVERY peg gated out the proxy misses without walking any pair, and
// with no other fallback wired the handler 404s.
func TestPrice_StablecoinProxy_GateAllEmpty_FastMiss(t *testing.T) {
	reader := &gatingStubPriceReader{
		stubPriceReader: stubPriceReader{err: v1.ErrPriceNotFound},
		exists:          map[string]bool{},
		latestCalls:     map[string]int{},
	}
	status, body := priceGet(t, v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdcPeg}}, pathNativeUSD)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (all pegs gated out): %s", status, body)
	}
	// The primary alias read legitimately walks native/fiat:USD; the
	// load-bearing check is that the classic-peg pair never is.
	if n := reader.latestCalls["native/"+usdcPeg.String()]; n != 0 {
		t.Errorf("peg pair walked %d times, want 0 (gate must skip it)", n)
	}
}

// A burst of concurrent requests for the SAME pair must collapse onto one
// upstream LatestPrice call.
func TestPrice_ConcurrentRequests_Coalesced(t *testing.T) {
	const n = 8
	reader := usdReader(v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.1242", PriceType: "last_trade",
		ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
	}, "sdex")
	reader.startedCh = make(chan struct{}, n)
	reader.releaseCh = make(chan struct{})
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := mustGet(t, ts.URL+pathNativeUSD); resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		}()
	}

	// A coalesced handler lets one call reach the reader; an uncoalesced one
	// lets all n through almost at once. Either settles well inside the window.
	deadline := time.After(200 * time.Millisecond)
drain:
	for {
		select {
		case <-reader.startedCh:
		case <-deadline:
			break drain
		}
	}
	close(reader.releaseCh)
	wg.Wait()

	if got := atomic.LoadInt32(&reader.calls); got != 1 {
		t.Errorf("LatestPrice calls = %d, want 1 (n=%d identical concurrent requests should coalesce)", got, n)
	}
}

// pairKeyedCompositeLooker keys both the VWAP cache and the composite
// meta on the LITERAL (base, quote) pair, as Redis does, so a read under
// the wrong spelling misses rather than passing by accident.
type pairKeyedCompositeLooker struct {
	vwaps map[string]v1.CachedVWAP
	metas map[string][]byte
}

func (l *pairKeyedCompositeLooker) LookupTriangulatedVWAP(
	_ context.Context, base, quote canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	v, ok := l.vwaps[base.String()+"/"+quote.String()]
	return v, ok, nil
}

func (l *pairKeyedCompositeLooker) LookupCompositeMeta(
	_ context.Context, base, quote canonical.Asset, _ time.Duration,
) ([]byte, bool, error) {
	m, ok := l.metas[base.String()+"/"+quote.String()]
	return m, ok, nil
}

// The aggregator publishes the GBP composite under crypto:XLM/fiat:GBP
// only, so on a closed-bucket miss ?asset=native must reach that same
// composite (value, triangulated flag, router meta) rather than a
// request-time FX cross. Everything but the echoed asset_id must match.
func TestPrice_FallbackCompositeIsSpellingIndependent(t *testing.T) {
	observed := time.Date(2026, 9, 18, 8, 55, 0, 0, time.UTC)
	looker := &pairKeyedCompositeLooker{
		vwaps: map[string]v1.CachedVWAP{
			"crypto:XLM/fiat:GBP": {Value: "0.140889883632", Triangulated: true, ObservedAt: observed},
		},
		metas: map[string][]byte{
			"crypto:XLM/fiat:GBP": []byte(`{"path_count":1,"combined_confidence":0.9,"low_confidence":false,"diverged":true,"rerouted":false}`),
		},
	}
	opts := v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}, Triangulated: looker}

	type response struct {
		Data    map[string]any `json:"data"`
		Flags   map[string]any `json:"flags"`
		Sources []string       `json:"sources"`
	}
	get := func(asset string) response {
		t.Helper()
		status, body := priceGet(t, opts, "/v1/price?asset="+asset+"&quote=fiat:GBP")
		if status != http.StatusOK {
			t.Fatalf("asset=%s: status = %d, want 200: %s", asset, status, body)
		}
		var resp response
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("asset=%s: decode: %v: %s", asset, err, body)
		}
		if got := resp.Data["asset_id"]; got != asset {
			t.Fatalf("asset=%s: asset_id = %v, want the requested spelling echoed", asset, got)
		}
		delete(resp.Data, "asset_id")
		return resp
	}

	composite := get("crypto:XLM")
	native := get("native")
	if composite.Data["price"] != "0.140889883632" {
		t.Fatalf("crypto:XLM price = %v, want the cached composite", composite.Data["price"])
	}
	if composite.Flags["triangulated"] != true || composite.Flags["diverged"] != true {
		t.Fatalf("crypto:XLM flags = %v, want triangulated and diverged from the composite meta", composite.Flags)
	}
	if !reflect.DeepEqual(native.Data, composite.Data) {
		t.Errorf("native data = %v\nwant (crypto:XLM) %v", native.Data, composite.Data)
	}
	if !reflect.DeepEqual(native.Flags, composite.Flags) {
		t.Errorf("native flags = %v\nwant (crypto:XLM) %v", native.Flags, composite.Flags)
	}
	if !reflect.DeepEqual(native.Sources, composite.Sources) {
		t.Errorf("native sources = %v, want (crypto:XLM) %v", native.Sources, composite.Sources)
	}
}

// TestPrice_NonstandardDecimals_NormalizesFlaggedBaseLeg proves /v1/price
// normalizes a confirmed non-7-decimal base leg — the closed-1m-
// bucket read (the last /v1/price path still declining after v0.12.0)
// now serves the AdjustPrice-corrected value. The stub snapshot carries
// the RAW CAGG ratio 41.32 (the runbook's real CC2RB… incident value,
// decimals()=9 vs USDC's 7): K = 10^(9−7) = 100 → true price 4132.
func TestPrice_NonstandardDecimals_NormalizesFlaggedBaseLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID:       flaggedAsset,
			Quote:         "fiat:USD",
			Price:         "41.32",
			PriceType:     "vwap",
			ObservedAt:    v1.WireTime(time.Unix(1745000000, 0).UTC()),
			WindowSeconds: 60,
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{
		Prices:              reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (closed-bucket read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("body missing normalized price 4132.0000000000: %s", body)
	}
}

// TestPrice_NonstandardDecimals_NormalizesFlaggedQuoteLeg proves the
// quote leg scales the other way: K = 10^(7−9) = 1/100.
func TestPrice_NonstandardDecimals_NormalizesFlaggedQuoteLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := "native/" + flaggedAsset
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID:       "native",
			Quote:         flaggedAsset,
			Price:         "41.32",
			PriceType:     "vwap",
			ObservedAt:    v1.WireTime(time.Unix(1745000000, 0).UTC()),
			WindowSeconds: 60,
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{
		Prices:              reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (closed-bucket read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.4132000000"`) {
		t.Errorf("body missing normalized price 0.4132000000: %s", body)
	}
}

// TestPrice_NonstandardDecimals_UnflaggedPairServesNormally proves the
// guard is NOT a false-positive trap: with the cache wired but the
// requested pair clean, /v1/price serves exactly as it would with no
// guard configured at all.
func TestPrice_NonstandardDecimals_UnflaggedPairServesNormally(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	snap := v1.PriceSnapshot{
		AssetID:    "native",
		Quote:      "fiat:USD",
		Price:      "0.1242",
		PriceType:  "last_trade",
		ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap},
		sources:   map[string][]string{"native/fiat:USD": {"sdex"}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unflagged pair must serve normally)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1242"`) {
		t.Errorf("body missing expected price: %s", body)
	}
}

// TestPrice_NonstandardDecimals_NoCacheWired_ServesNormally proves a
// deployment that never wires NonstandardDecimals (the pre-guard shape,
// and every deployment until the cache is configured) is unaffected —
// declineIfNonstandardDecimals must be a pure no-op when s.NonstandardDecimals
// is nil.
func TestPrice_NonstandardDecimals_NoCacheWired_ServesNormally(t *testing.T) {
	snap := v1.PriceSnapshot{AssetID: "native", Quote: "fiat:USD", Price: "0.5", PriceType: "last_trade"}
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap}}
	srv := v1.New(v1.Options{Prices: reader}) // NonstandardDecimals left nil
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPrice_StablecoinProxy_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	usdc, err := canonical.ParseAsset(classicUSDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	// Literal flagged/fiat:USD is ABSENT; the proxy walk rewrites to
	// flagged/<USDC-classic>, whose raw VWAP is 41.32. tryStablecoinFiatProxy
	// must scale it by K=100 → 4132 before serving.
	key := flaggedAsset + "/" + usdc.String()
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: usdc.String(), Price: "41.32",
				PriceType: "vwap", ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		USDPeggedClassics:   []canonical.Asset{usdc},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("stablecoin-proxy tier not normalized (want 4132.0000000000): %s", body)
	}
}

// TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross: the SAC
// spelling serves the same XLM cross as the classic id, everything but the
// asset_id echo identical.
func TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross(t *testing.T) {
	pegLegAt := time.Unix(1745000000, 0).UTC()
	pivotAt := pegLegAt.Add(-3 * time.Minute)
	reader := xlmCrossReader(pegLegAt, pivotAt)
	base := pegServer(t, reader)

	classic := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	sac := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCSAC))

	if sac.Data.Price != "0.9500000000" {
		t.Errorf("SAC price = %q, want 0.9500000000 — the observed XLM cross the classic id serves", sac.Data.Price)
	}
	if sac.Data.PriceType != "vwap" || sac.Data.WindowSeconds != 60 {
		t.Errorf("SAC snapshot = %s/%d, want vwap/60", sac.Data.PriceType, sac.Data.WindowSeconds)
	}
	if !sac.Data.ObservedAt.Time().Equal(pivotAt) {
		t.Errorf("SAC observed_at = %s, want the older leg's %s", sac.Data.ObservedAt, pivotAt)
	}
	if sac.Data.AssetID != pegAliasUSDCSAC || sac.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", sac.Data.AssetID, sac.Data.Quote, pegAliasUSDCSAC)
	}
	want := classic.Data
	want.AssetID = pegAliasUSDCSAC
	if !reflect.DeepEqual(sac.Data, want) {
		t.Errorf("SAC snapshot differs from the classic spelling's beyond asset_id:\n sac     = %+v\n classic = %+v", sac.Data, classic.Data)
	}
	if !reflect.DeepEqual(sac.Flags, classic.Flags) {
		t.Errorf("flags differ between spellings: sac=%+v classic=%+v", sac.Flags, classic.Flags)
	}
	if !sac.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	body, _ := readAll(mustGet(t, pegPriceURL(base, pegAliasUSDCSAC)))
	if !strings.Contains(body, `"sources":["bitstamp","coinbase","sdex"]`) {
		t.Errorf("sources must credit both legs' venues, sorted: %s", body)
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool: the
// leg walks ONE spelling at a time and reaches the SAC form only when the
// classic form found nothing. A dormant (stale) classic book still answers;
// a fresh thin pool must not displace it, and is never read.
func TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), true)
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want 0.9500000000 — the dormant SDEX book, not the fresh pool's 2.0000000000",
			env.Data.Price)
	}
	body, _ := readAll(mustGet(t, pegPriceURL(base, pegAliasUSDCClassic)))
	if !strings.Contains(body, `"sources":["coinbase","sdex"]`) {
		t.Errorf("sources must be the book's venues and the pivot's, without the pool: %s", body)
	}
	assertSACPoolUnread(t, reader.pairsAsked(), "although its classic form answered")
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst: spellings
// are walked classic first, SAC last, not literal-first. With both books
// fresh and disagreeing, either spelling serves the deep book's value.
func TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), false)
	base := pegServer(t, reader)

	for _, spelling := range pegSpellings {
		env := getPegEnvelope(t, pegPriceURL(base, spelling))
		if env.Data.Price != "0.9500000000" {
			t.Errorf("%s: price = %q, want 0.9500000000 — the classic book prices the peg under either spelling, not the pool's 2.0000000000",
				spelling, env.Data.Price)
		}
		if env.Data.AssetID != spelling {
			t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
		}
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook: the peg leg reads the
// peg's whole family. A peg whose only XLM book is a pool under its SAC id
// crosses through it for both spellings instead of falling to the declaration.
func TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook(t *testing.T) {
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegSACPool:    pegSnap(pegAliasUSDCSAC, canonical.XLMSacContractID, "10.0", poolAt),
			xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", poolAt),
		},
	}}
	base := pegServer(t, reader)

	for _, spelling := range pegSpellings {
		env := getPegEnvelope(t, pegPriceURL(base, spelling))
		if env.Data.Price != "1.0000000000" || env.Data.PriceType != "vwap" {
			t.Errorf("%s: served %s (%s), want the pool's cross 1.0000000000 (vwap), not the declaration",
				spelling, env.Data.Price, env.Data.PriceType)
		}
		if !env.Data.ObservedAt.Time().Equal(poolAt) {
			t.Errorf("%s: observed_at = %s, want the pool's %s", spelling, env.Data.ObservedAt, poolAt)
		}
		if env.Data.AssetID != spelling {
			t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
		}
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration: with no
// market anywhere the SAC spelling serves the declaration (peg, adoption
// stamp, C-address echo) and never probes its own classic form.
func TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration(t *testing.T) {
	reader := &recordingPriceReader{}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCSAC))
	assertDeclarationServed(t, env, pegAliasUSDCSAC)
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true")
	}
	asked := reader.pairsAsked()
	if len(asked) == 0 {
		t.Fatal("the reader was never consulted — the direct read and the cross must both run before the declaration")
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs: a SAC asset that is not
// the declared peg takes the sibling walk, and the XLM cross is not run.
func TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs(t *testing.T) {
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasAquaSAC + "/" + pegAliasUSDCClassic: pegSnap(pegAliasAquaSAC, pegAliasUSDCClassic, "0.0041", poolAt),
			// Present but must not be consulted: the cross is the declared peg's route only.
			pegAliasAquaSAC + "/native": pegSnap(pegAliasAquaSAC, "native", "0.02", poolAt),
			xlmDollarPair:               pegSnap("crypto:XLM", "fiat:USD", "0.10", poolAt),
		},
	}}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasAquaSAC))
	if env.Data.Price != "0.0041" {
		t.Errorf("price = %q, want the proxy walk's 0.0041 (not the XLM cross 0.0020000000)", env.Data.Price)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — served through the peg, not the requested quote")
	}
	if env.Data.AssetID != pegAliasAquaSAC || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want %s/fiat:USD", env.Data.AssetID, env.Data.Quote, pegAliasAquaSAC)
	}
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasAquaSAC+"/"+pegAliasUSDCClassic) < 0 {
		t.Errorf("the walk never read %s/%s (asked=%v)", pegAliasAquaSAC, pegAliasUSDCClassic, asked)
	}
	for _, pair := range asked {
		if strings.HasSuffix(pair, "/native") || strings.HasSuffix(pair, "/crypto:XLM") ||
			strings.HasSuffix(pair, "/"+canonical.XLMSacContractID) {
			t.Errorf("reader asked for %s — the XLM cross must not run for an asset that is not the declared peg", pair)
		}
	}
}

// TestPrice_DeclaredPegXLMCrossClassicBookFailureEndsTheSpellingWalk: a
// flagged-issuer refusal or a per-pair read failure (v0.60.0's 42883 planning
// error) on the classic book ends the walk and serves the declaration. Taking
// either as "no market" would reprice the peg off the SAC spelling's thin
// pool (2.0000000000), republishing, for a refusal, a market the scam gate
// withheld.
func TestPrice_DeclaredPegXLMCrossClassicBookFailureEndsTheSpellingWalk(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		spellings []string
	}{
		{"flagged-issuer refusal", v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer), pegSpellings},
		{"read failure", errors.New(`ERROR: operator does not exist: numeric = text (SQLSTATE 42883)`), pegSpellings[:1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), tc.err)
			base := pegServer(t, reader)

			for _, spelling := range tc.spellings {
				assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, spelling)), spelling)
			}
			asked := reader.pairsAsked()
			if callIndex(asked, pegClassicXLMBook) < 0 {
				t.Errorf("the classic book was never read (asked=%v)", asked)
			}
			assertSACPoolUnread(t, asked, "after its classic book failed — a refusal or broken read must end the walk, not redirect it")
			assertNoPegSelfPairRead(t, asked)
		})
	}
}

// TestPrice_DeclaredPegXLMCrossSubstanceRefusalWithholds: a peg whose XLM
// book the substance gate refuses is withheld on every surface that reads
// the stablecoin fallback, not answered with the declaration. A refusal that
// names no gate fails closed the same way; only a flagged-issuer refusal
// keeps the declaration.
func TestPrice_DeclaredPegXLMCrossSubstanceRefusalWithholds(t *testing.T) {
	cases := map[string]error{
		"substance":    v1.PriceWithheldError(pricingguard.WithheldThinMarket),
		"unattributed": v1.ErrPriceWithheld,
	}
	for name, refusal := range cases {
		t.Run(name, func(t *testing.T) {
			reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), refusal)
			base := pegServer(t, reader)

			for _, spelling := range pegSpellings {
				assertPriceWithheld(t, pegPriceURL(base, spelling))
				assertPriceWithheld(t, base+"/v1/oracle/x_last_price?base="+spelling+"&quote=fiat:USD")
			}
			asked := reader.pairsAsked()
			if callIndex(asked, pegClassicXLMBook) < 0 {
				t.Errorf("the classic book was never read (asked=%v) — the withholding would then not be the gate's", asked)
			}
			assertSACPoolUnread(t, asked, "after its classic book was WITHHELD")
		})
	}
}

// TestPrice_DeclaredPegXLMCrossPivotRefusalWithholds: the same rule on the
// cross's other leg. A withheld XLM/fiat:USD pivot is not "no market", so
// the declaration must not answer over it.
func TestPrice_DeclaredPegXLMCrossPivotRefusalWithholds(t *testing.T) {
	reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), nil)
	reader.errs = map[string]error{xlmDollarPair: v1.PriceWithheldError(pricingguard.WithheldThinMarket)}
	base := pegServer(t, reader)

	assertPriceWithheld(t, pegPriceURL(base, pegAliasUSDCClassic))
	if callIndex(reader.pairsAsked(), xlmDollarPair) < 0 {
		t.Errorf("the pivot was never read (asked=%v)", reader.pairsAsked())
	}
}

// TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk: a
// probe MISS is what means "this spelling found nothing". The classic book is
// held but gated out, so it is never read and the live SAC pool prices the
// cross.
func TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk(t *testing.T) {
	reader := gatingDormantBookReader(time.Unix(1745000000, 0).UTC())
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "2.0000000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the pool's cross 2.0000000000 (vwap) — a gate miss is the one "+
			"verdict that advances the spelling walk", env.Data.Price, env.Data.PriceType)
	}
	if probed := reader.probesMade(); callIndex(probed, pegClassicXLMBook) < 0 {
		t.Errorf("the classic book was never probed (probes=%v)", probed)
	}
	if asked := reader.pairsAsked(); callIndex(asked, pegClassicXLMBook) >= 0 {
		t.Errorf("the gated-out classic book was READ (asked=%v) — the probe exists to skip that "+
			"unbounded last-trade scan", asked)
	}
}

// TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook: a probe
// OUTAGE must not hide a price nor hand it to another spelling. The classic
// book is read anyway, answers, and the SAC pool is never reached.
func TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook(t *testing.T) {
	reader := gatingDormantBookReader(time.Unix(1745000000, 0).UTC())
	reader.existsErr = map[string]error{pegClassicXLMBook: errors.New("probe unavailable")}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "0.9500000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the classic book's cross 0.9500000000 (vwap) — a probe blip "+
			"must not hide a price", env.Data.Price, env.Data.PriceType)
	}
	assertSACPoolUnread(t, reader.pairsAsked(), "although the classic book answered")
}

// TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook:
// a classic price that READ but is zero ends the walk (the spelling
// answered), crossThroughPivot declines it, and the declaration serves
// rather than a 2x move onto the SAC pool.
func TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), false)
	book := reader.snapshots[pegClassicXLMBook]
	book.Price = "0"
	reader.snapshots[pegClassicXLMBook] = book
	base := pegServer(t, reader)

	assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic)), pegAliasUSDCClassic)
	assertSACPoolUnread(t, reader.pairsAsked(), "although the classic book returned a row — the walk advances on FOUND NOTHING, never on found-something-unusable")
}

// TestPrice_FlaggedDeclaredPegServesTheDeclarationUnderBothSpellings: the
// peg route widening the walk carries a flagged issuer's price to a spelling
// the gate could not see, and the gate resolving its base closes it. The
// classic form finds nothing, the walk reaches the SAC pool (2x off the
// declaration), and both spellings must serve the SAME declaration.
//
// RED without the gate's `base = canonical.CanonicalAsset(base)`, or with the
// peg match narrowed to `peg.Equal(asset)` (the contract id 404s).
// NOT parallel: newSACSpellingFixture installs the process-global alias registry.
func TestPrice_FlaggedDeclaredPegServesTheDeclarationUnderBothSpellings(t *testing.T) {
	f := newSACSpellingFixture(t, true)
	reader := flaggedPegLivePoolReader(time.Unix(1745000000, 0).UTC(), f)
	base := pegServerFor(t, reader, f.classic)

	classic, sac := f.classic.String(), f.sac.String()
	bodies := map[string]string{}
	for _, spelling := range []string{classic, sac} {
		assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, spelling)), spelling)
		bodies[spelling] = maskedPriceBody(t, pegPriceURL(base, spelling))
		if strings.Contains(bodies[spelling], "soroswap") {
			t.Errorf("%s: the served body credits the pool's venue — the flagged issuer's price "+
				"was republished through the peg's SAC spelling: %s", spelling, bodies[spelling])
		}
	}
	if bodies[classic] != bodies[sac] {
		t.Errorf("the two spellings of one asset disagree:\n classic = %s\n sac     = %s",
			bodies[classic], bodies[sac])
	}

	// Non-vacuity: the pool was read, so the declaration serves because the
	// gate refused it, not because the walk never got there.
	asked := reader.pairsAsked()
	if callIndex(asked, sac+"/"+canonical.XLMSacContractID) < 0 {
		t.Errorf("the peg's SAC pool was never read (asked=%v) — the declaration would then be "+
			"serving for want of a market rather than because the gate refused one", asked)
	}
	// The mechanism: the gate asked the directory about the classic issuance's G-address.
	if len(f.dir.asked) == 0 || f.dir.asked[0] != sacSpellingFlaggedIssuer {
		t.Errorf("directory asked about %v, want the peg's issuer %q — the gate never resolved the "+
			"Soroban base to the issuance it wraps", f.dir.asked, sacSpellingFlaggedIssuer)
	}
	for _, pair := range asked {
		switch pair {
		case sac + "/" + classic, classic + "/" + sac:
			t.Errorf("reader asked for %s — the two sides are one asset, never a market", pair)
		}
	}
}

// TestPrice_XLMIdentitiesUnchangedOnTheCombinedPath is a CONTROL that cannot
// currently fail: XLM canonicalises to `native` (no issuer for the gate) and
// is not a declared peg, so neither the gate's alias resolution nor the peg
// match has a path to it. It guards a later widening that would reach XLM
// (a base resolved into the peg match, an XLM form in the peg list).
//
// The dollar bucket is held under crypto:XLM alone, so `native` answers only
// by the literal-first alias fallback; this pins that the combined path
// leaves that behaviour where it was.
func TestPrice_XLMIdentitiesUnchangedOnTheCombinedPath(t *testing.T) {
	at := time.Unix(1745000000, 0).UTC()
	// Armed gate (a flagged issuer in the directory) so XLM passing it is a verdict.
	dir := &sacScamDirectory{flagged: map[string]bool{sacSpellingFlaggedIssuer: true}}
	reader := &scamGatedPegReader{
		recordingPriceReader: recordingPriceReader{stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", at)},
			sources:   map[string][]string{xlmDollarPair: {"coinbase"}},
		}},
		scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	}
	base := pegServer(t, reader)

	want := maskedPriceBody(t, pegPriceURL(base, "native"))
	for _, spelling := range []string{"crypto:XLM", canonical.XLMSacContractID} {
		got := maskedPriceBody(t, pegPriceURL(base, spelling))
		if got != want {
			t.Errorf("%s serves a different body from `native`:\n %s = %s\n native = %s",
				spelling, spelling, got, want)
		}
	}
	// Agreement must not be on the wrong thing: the observed CEX bucket, not the declaration.
	for _, member := range []string{`"price":"0.10"`, `"price_type":"vwap"`, `"sources":["coinbase"]`} {
		if !strings.Contains(want, member) {
			t.Errorf("the `native` body is missing %s — an XLM form must still serve its own "+
				"observed market on the combined path: %s", member, want)
		}
	}
}

// TestPrice_DeclaredPegServesTheObservedXLMCross pins the rule the
// stablecoin-proxy peg arm broke: the operator's 1:1 declaration is a
// CONSTANT, and a constant must never pre-empt an observation.
//
// The peg arm must not answer the moment it recognises the requested
// asset as a declared peg, so /v1/price?asset=USDC-GA5Z…&quote=fiat:USD
// would publish 1.000000000000 while the market was somewhere else — the
// failure shape is /v1/price serving the flat peg in the same
// minute /v1/assets served 1.0008594347 for the same asset. Under a real
// depeg the surface would have gone on publishing $1 rather than the
// break, which is the one moment the number matters.
//
// Here USDC has no fiat:USD bucket anywhere (nothing on chain quotes in
// fiat), but its XLM book on SDEX has repriced: 9.5 XLM per USDC while
// XLM's own CEX dollar market prints 0.10 — a cross of 0.95. The route
// must find it and serve it as an observation — price_type, observed_at
// and window_seconds intact — with the two legs' venues both credited.
func TestPrice_DeclaredPegServesTheObservedXLMCross(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	pegLegAt := time.Unix(1745000000, 0).UTC()
	pivotAt := pegLegAt.Add(-3 * time.Minute) // the staler leg bounds freshness
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			// The peg's own book, quoted in `native` — where SDEX prints it.
			// Both legs are closed 1-minute buckets, the shape
			// VWAP1mToSnapshot hands the reader's callers.
			usdcClassicID + "/native": {
				AssetID: usdcClassicID, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: v1.WireTime(pegLegAt), WindowSeconds: 60,
			},
			// XLM's dollar market, stored under the CEX spelling — reached
			// through the alias loop, one form away from `native`.
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: v1.WireTime(pivotAt), WindowSeconds: 60,
			},
		},
		sources: map[string][]string{
			usdcClassicID + "/native": {"sdex"},
			"crypto:XLM/fiat:USD":     {"coinbase", "bitstamp"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want 0.9500000000 — the observed XLM cross, not the declared peg", env.Data.Price)
	}
	if env.Data.PriceType != "vwap" {
		t.Errorf("price_type = %q, want vwap (an observation, not a declaration)", env.Data.PriceType)
	}
	if env.Data.WindowSeconds != 60 {
		t.Errorf("window_seconds = %d, want 60 — a served vwap names its window; only last_trade omits it", env.Data.WindowSeconds)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	if !env.Data.ObservedAt.Time().Equal(pivotAt) {
		t.Errorf("observed_at = %s, want the OLDER leg's %s — a derived price is only as fresh as its staler input",
			env.Data.ObservedAt, pivotAt)
	}
	if env.Data.AssetID != usdcClassicID || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", env.Data.AssetID, env.Data.Quote, usdcClassicID)
	}
	// sources ride the envelope, not the snapshot; window_seconds is
	// omitempty, so its presence is checked on the wire, not the struct.
	resp := mustGet(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"sources":["bitstamp","coinbase","sdex"]`) {
		t.Errorf("sources must credit both legs' venues, sorted: %s", body)
	}
	if !strings.Contains(body, `"window_seconds":60`) {
		t.Errorf("window_seconds must be on the wire for a vwap: %s", body)
	}
}

// TestPrice_DeclaredPegWithNoObservationServesTheDeclaration pins the
// fallback that remains when NO market prices the peg at all: the flat $1
// still serves rather than a 404, it is labelled a declaration,
// and it carries the declaration's adoption stamp — not the clock.
func TestPrice_DeclaredPegWithNoObservationServesTheDeclaration(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "1.000000000000" {
		t.Errorf("price = %q, want 1.000000000000 — the declaration is the last resort, not a 404", env.Data.Price)
	}
	if env.Data.PriceType != "peg" {
		t.Errorf("price_type = %q, want peg", env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Time().Equal(declaredPegAdoptedAt) {
		t.Errorf("observed_at = %s, want the declaration stamp %s — a constant is not re-observed per request",
			env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the wire shape F-1232 fixed")
	}
}

// TestPrice_DeclaredPegDirectUSDObservationWins pins the order: a
// directly observed fiat:USD market for the peg is served as-is, and the
// XLM cross is never consulted for it — no XLM-quoted read of the peg,
// no read of XLM's dollar market. The cross is a fallback for a missing
// market, never a competitor to a present one.
func TestPrice_DeclaredPegDirectUSDObservationWins(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	directAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			usdcClassicID + "/fiat:USD": {
				AssetID: usdcClassicID, Quote: "fiat:USD",
				Price: "0.9700", PriceType: "vwap", ObservedAt: v1.WireTime(directAt),
			},
			// Both cross legs are present and would compose to 0.95 —
			// they must not be read.
			usdcClassicID + "/native": {
				AssetID: usdcClassicID, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: v1.WireTime(directAt),
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: v1.WireTime(directAt),
			},
		},
		sources: map[string][]string{
			usdcClassicID + "/fiat:USD": {"kraken"},
		},
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "0.9700" {
		t.Errorf("price = %q, want the direct 0.9700", env.Data.Price)
	}
	if env.Data.PriceType != "vwap" || !env.Data.ObservedAt.Time().Equal(directAt) {
		t.Errorf("snapshot = %s@%s, want the direct observation vwap@%s", env.Data.PriceType, env.Data.ObservedAt, directAt)
	}
	if env.Flags.Triangulated {
		t.Errorf("flags.triangulated = true — a directly observed market is not composed")
	}
	for _, pair := range reader.pairsAsked() {
		if !strings.HasSuffix(pair, "/fiat:USD") || !strings.HasPrefix(pair, usdcClassicID+"/") {
			t.Errorf("reader asked for %s — the cross must not run once the direct market answered", pair)
		}
	}
}

// TestPrice_DeclaredPegXLMLegPrefersAFreshForm pins which of XLM's
// forms prices the peg leg when more than one holds a row: a fresh
// answer under any form beats a stale one under an earlier form, and a
// stale answer still serves when it is the only one — the preference
// readPriceWithAliases applies on the direct read, mirrored on the leg.
//
// `native` is the first form in alias order. Taking the first form that
// answers would serve a three-day-old `native`-quoted book at 9.5 (a
// cross of 0.95, stamped three days ago) while a fresh SAC-quoted book
// prints 10.0 (a cross of 1.00, stamped minutes ago). The wire carries
// flags.stale=true on every fallback answer regardless
// (TestPrice_FallbackChainSetsStaleFlag), so the leg chosen shows in
// the price and in observed_at, not in the flag.
func TestPrice_DeclaredPegXLMLegPrefersAFreshForm(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	staleAt := now.Add(-72 * time.Hour)
	freshAt := now.Add(-2 * time.Minute)
	pivotAt := now.Add(-1 * time.Minute)
	staleNativeLeg := usdcClassicID + "/native"
	freshSACLeg := usdcClassicID + "/" + canonical.XLMSacContractID
	legs := func(withFresh bool) *stubPriceReader {
		reader := &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				staleNativeLeg: {
					AssetID: usdcClassicID, Quote: "native",
					Price: "9.5", PriceType: "vwap", ObservedAt: v1.WireTime(staleAt), WindowSeconds: 60,
				},
				"crypto:XLM/fiat:USD": {
					AssetID: "crypto:XLM", Quote: "fiat:USD",
					Price: "0.10", PriceType: "vwap", ObservedAt: v1.WireTime(pivotAt), WindowSeconds: 60,
				},
			},
			stale: map[string]bool{
				staleNativeLeg:        true,
				"crypto:XLM/fiat:USD": false,
			},
		}
		if withFresh {
			reader.snapshots[freshSACLeg] = v1.PriceSnapshot{
				AssetID: usdcClassicID, Quote: canonical.XLMSacContractID,
				Price: "10.0", PriceType: "vwap", ObservedAt: v1.WireTime(freshAt), WindowSeconds: 60,
			}
			reader.stale[freshSACLeg] = false
		}
		return reader
	}

	t.Run("a fresh SAC-quoted book beats the stale native-quoted one", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:            legs(true),
			USDPeggedClassics: []canonical.Asset{usdc},
			PegDeclaredAt:     declaredPegAdoptedAt,
		})
		ts := startHTTPTest(t, srv.Handler())
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
		if env.Data.Price != "1.0000000000" {
			t.Errorf("price = %q, want 1.0000000000 — the fresh SAC-quoted book, not the stale native one", env.Data.Price)
		}
		// The older of the two legs is the fresh peg leg, two minutes
		// ago — not the three-day-old book the earlier form holds.
		if !env.Data.ObservedAt.Time().Equal(freshAt) {
			t.Errorf("observed_at = %s, want the fresh leg's %s", env.Data.ObservedAt, freshAt)
		}
		if env.Data.ObservedAt.Time().Before(now.Add(-time.Hour)) {
			t.Errorf("observed_at = %s is not recent — the stale leg was served", env.Data.ObservedAt)
		}
	})

	t.Run("a stale book still serves when it is the only one", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:            legs(false),
			USDPeggedClassics: []canonical.Asset{usdc},
			PegDeclaredAt:     declaredPegAdoptedAt,
		})
		ts := startHTTPTest(t, srv.Handler())
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
		if env.Data.Price != "0.9500000000" {
			t.Errorf("price = %q, want 0.9500000000 — a stale observation still beats the declaration", env.Data.Price)
		}
		if !env.Data.ObservedAt.Time().Equal(staleAt) {
			t.Errorf("observed_at = %s, want the stale leg's %s", env.Data.ObservedAt, staleAt)
		}
		if !env.Flags.Stale {
			t.Errorf("flags.stale = false, want true — a fallback answer is below the closed-bucket contract")
		}
	})
}

// TestPrice_DeclaredPegIsNotStampedAsAFreshObservation pins the wire
// shape of the declaration under the DEFAULT stamp — the one the API
// binary runs with, since the operator declaration carries no timestamp
// of its own: the server's construction time.
//
// observed_at must not be time.Now() on every request, or the constant
// would be indistinguishable on the wire from an observation taken this
// instant — live, two consecutive GETs came back 123ms apart, each
// carrying an observed_at equal to its own envelope as_of. It
// predates the first request ever made (the server existed before the
// request did), does not advance between requests, and does not claim
// to have been observed after the response was built.
func TestPrice_DeclaredPegIsNotStampedAsAFreshObservation(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := startHTTPTest(t, srv.Handler())

	// Taken AFTER the server was built and BEFORE anything was asked of
	// it: the declaration was adopted before this instant, and a stamp
	// taken inside a request handler cannot precede it.
	firstRequestStart := time.Now().UTC()
	first := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	second := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")

	// The declaration itself is unchanged — this surface still answers
	// $1 for a peg no market prices.
	if first.Data.Price != "1.000000000000" {
		t.Errorf("price = %q, want 1.000000000000", first.Data.Price)
	}
	if first.Data.PriceType != "peg" {
		t.Errorf("price_type = %q, want peg", first.Data.PriceType)
	}
	// The stamp predates the FIRST request: it is the adoption time, not
	// the request clock.
	if first.Data.ObservedAt.Time().After(firstRequestStart) {
		t.Errorf("observed_at %s is after the first request began at %s — the constant is stamped with the request clock, not the declaration's adoption time",
			first.Data.ObservedAt, firstRequestStart)
	}
	// A constant is not re-observed per request.
	if !first.Data.ObservedAt.Time().Equal(second.Data.ObservedAt.Time()) {
		t.Errorf("observed_at advanced between requests: %s then %s — a declaration is not an observation",
			first.Data.ObservedAt, second.Data.ObservedAt)
	}
	// And it predates the response: an observed_at that equals the
	// envelope's as_of reads as an observation taken this instant.
	if !first.Data.ObservedAt.Time().Before(first.AsOf) {
		t.Errorf("observed_at %s is not before as_of %s — the constant is still stamped with the clock",
			first.Data.ObservedAt, first.AsOf)
	}
}

// A held value's confidence is the one its own window scored.
func TestPrice_FrozenHeldValueLooksUpConfidenceForItsOwnWindow(t *testing.T) {
	conf := &windowRecordingConfidence{}
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/3600": heldLKG},
		Confidence:   conf,
	})
	ts := startHTTPTest(t, srv.Handler())

	if status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if len(conf.windows) == 0 || conf.windows[0] != time.Hour {
		t.Errorf("confidence looked up for windows %v, want the held 1h window", conf.windows)
	}
}

// The held value's router-quality flags describe the window that holds it,
// not the 5m default used before the held window is known.
func TestPrice_FrozenHeldValueCarriesItsOwnWindowCompositeFlags(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices: movedBucketReader(),
		Freeze: frozenPairs{xlmGBP: true},
		Triangulated: windowMetaPairs{
			lkgPairs: lkgPairs{xlmGBP + "/3600": heldLKG},
			metas: map[string][]byte{
				xlmGBP + "/3600": []byte(`{"path_count":1,"combined_confidence":0.9,"diverged":true,"rerouted":false}`),
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"window_seconds":3600`, `"frozen":true`, `"diverged":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestPrice_FrozenPairServesLastKnownGoodNotTheMovedBucket(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("frozen pair served the prices_1m bucket the freeze refused (%s): %s", movedBucket, body)
	}
	for _, want := range []string{
		`"price":"` + heldLKG + `"`,
		`"window_seconds":300`, // the value's real window, not the 60 it replaced
		`"frozen":true`,
		`"single_source":true`,
		`"stale":true`, // below the closed-1m-bucket contract, like every other non-bucket serve
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	// The refused bucket's venues must not be credited for a value they
	// did not produce.
	if strings.Contains(body, "kraken") {
		t.Errorf("sources still credit the refused bucket's venues: %s", body)
	}
}

// The freeze lifecycle is per (pair, window) while the marker is per
// pair: the 5m key can be cold (a thin window is dropped before the
// freeze step) while 1h holds the value. The held value is served with
// ITS window, never relabelled as something it is not.
func TestPrice_FrozenPairFallsToTheWindowThatHoldsAValue(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/3600": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("served the refused bucket: %s", body)
	}
	for _, want := range []string{`"price":"` + heldLKG + `"`, `"window_seconds":3600`, `"frozen":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// Frozen with no held value anywhere: the only price on hand is the one
// the freeze exists to withhold. Refuse loudly rather than publish it
// under a flag that says it is something else.
func TestPrice_FrozenPairWithNoHeldValueRefusesRatherThanServeTheBucket(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("refusal leaked the withheld bucket: %s", body)
	}
	if !strings.Contains(body, "errors/price-unavailable") {
		t.Errorf("want the price-unavailable problem type: %s", body)
	}
}

// The substitution is destructive (it discards the bucket, its window
// and its sources), so pin the path where it must NOT fire: an unfrozen
// pair is served exactly as read even though a cached VWAP exists.
func TestPrice_UnfrozenPairStillServesTheClosedBucketUntouched(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{
		`"price":"` + movedBucket + `"`, `"window_seconds":60`, `"stale":false`,
		`"sources":["kraken","coinbase","bitstamp"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, heldLKG) || strings.Contains(body, `"frozen":true`) {
		t.Errorf("unfrozen pair must not be touched by the freeze path: %s", body)
	}
}

// XLM is asked for as `native` but priced — and frozen — as crypto:XLM.
// The alias walk reads crypto:XLM's prices_1m bucket, so the freeze
// that governs THAT pair must govern the response, or the refused
// bucket is one spelling away.
func TestPrice_FrozenAliasPairServesItsLastKnownGood(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(), // only crypto:XLM/fiat:GBP has rows
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("alias spelling served the refused bucket: %s", body)
	}
	for _, want := range []string{`"asset_id":"native"`, `"price":"` + heldLKG + `"`, `"frozen":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestPrice_FreezeOnRequestedLiteralOnlyDoesNotDiscardTheHealthyAliasBucket(t *testing.T) {
	for name, cache := range literalOnlyFreezeShapes() {
		t.Run(name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       healthyAliasReader(),
				Freeze:       frozenPairs{nativeGBP: true}, // crypto:XLM/fiat:GBP is NOT frozen
				Triangulated: cache,
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 with the healthy alias bucket: %s", status, body)
			}
			if strings.Contains(body, literalHeld24h) {
				t.Fatalf("the literal's held value replaced the healthy alias bucket: %s", body)
			}
			for _, want := range []string{
				`"asset_id":"native"`,
				`"price":"` + healthyAliasBucket + `"`,
				`"window_seconds":60`,
				`"sources":["kraken","coinbase","bitstamp"]`,
				`"stale":false`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			// The served pair is not frozen; the flag describes the value
			// in the response, so it must not claim otherwise. (Both flags
			// are omitted from the wire when false.)
			if strings.Contains(body, `"frozen":true`) || strings.Contains(body, `"single_source":true`) {
				t.Errorf("an unfrozen three-venue bucket must not be flagged frozen/single_source: %s", body)
			}

			// Both spellings are the same market and must agree.
			_, direct := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
			if !strings.Contains(direct, `"price":"`+healthyAliasBucket+`"`) {
				t.Fatalf("control: crypto:XLM spelling should serve the bucket: %s", direct)
			}
		})
	}
}

// The bound must not weaken the main fix. When the SERVED alias is
// frozen, a second marker on the requested literal changes nothing: the
// served pair's held value is what goes out, never the refused bucket
// and never the literal's held value.
func TestPrice_FrozenServedAliasStillHoldsWhenTheLiteralIsAlsoFrozen(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices: movedBucketReader(),
		Freeze: frozenPairs{xlmGBP: true, nativeGBP: true},
		Triangulated: lkgPairs{
			xlmGBP + "/300":      heldLKG,
			nativeGBP + "/86400": literalHeld24h,
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) || strings.Contains(body, literalHeld24h) {
		t.Fatalf("want the served pair's held value only: %s", body)
	}
	for _, want := range []string{`"price":"` + heldLKG + `"`, `"frozen":true`, `"stale":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// ...and a frozen served alias with nothing held still refuses, even
// though the literal holds a value: one venue population's held value is
// never substituted for another's.
func TestPrice_FrozenServedAliasWithNothingHeldStillRefuses(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true, nativeGBP: true},
		Triangulated: lkgPairs{nativeGBP + "/86400": literalHeld24h},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", status, body)
	}
	if strings.Contains(body, movedBucket) || strings.Contains(body, literalHeld24h) {
		t.Fatalf("refusal leaked a value: %s", body)
	}
}

// No closed-bucket read served the response (served alias is zero): the
// requested literal's marker is the only one there is, and it still
// governs — the bound applies only when a bucket was actually read.
func TestPrice_FreezeOnLiteralStillGovernsWhenNoBucketWasServed(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{}, // every alias misses -> fallback chain
		Freeze:       frozenPairs{nativeGBP: true},
		Triangulated: lkgPairs{nativeGBP + "/300": literalHeld24h},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"price":"` + literalHeld24h + `"`, `"frozen":true`, `"single_source":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// A direct (non-triangulated) answer never carries the flag, even with a
// depegged peg elsewhere.
func TestPrice_ProxyDeviation_NotTriangulated(t *testing.T) {
	ts := devDeviationServer(t, "0.95")
	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if strings.Contains(body, `"proxy_deviation":true`) {
		t.Errorf("direct price must not flag proxy_deviation: %s", body)
	}
}

func TestPrice_Withheld_Distinct404Type(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing price-withheld problem type: %s", body)
	}
	if strings.Contains(string(body), "errors/price-not-found") {
		t.Errorf("withheld must not be reported as not-found: %s", body)
	}
}

// TestPrice_Withheld_SkipsFallbackChain — the read-time stablecoin
// proxy (one arm of priceFallback) must NOT rescue a withheld pair:
// falling back would re-serve the same substanceless market through a
// side door. The verdict is per pair, as at the production reader seam:
// native/fiat:USD is withheld while native/<USD peg> holds a servable
// snapshot, so a handler that ran priceFallback (first, or at all) would
// serve the peg's price with a 200.
func TestPrice_Withheld_SkipsFallbackChain(t *testing.T) {
	peg, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	serverURL := func(errByPair map[string]error) string {
		reader := &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				"native/" + peg.String(): {Price: "0.12", PriceType: "vwap"},
			},
			errByPair: errByPair,
		}
		srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{peg}})
		return startHTTPTest(t, srv.Handler()).URL
	}

	// Control: without the verdict the peg fallback serves, so the fixture
	// can build the 200 the withheld arm exists to prevent.
	control := mustGet(t, serverURL(nil)+"/v1/price?asset=native&quote=fiat:USD")
	if control.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(control.Body)
		t.Fatalf("control: status = %d, want 200 via the USD peg fallback: %s", control.StatusCode, body)
	}

	withheld := map[string]error{"native/fiat:USD": v1.ErrPriceWithheld}
	resp := mustGet(t, serverURL(withheld)+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (withheld must not fall back to the stablecoin proxy): %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing price-withheld problem type: %s", body)
	}
}

// TestPrice_ThinPoolThirdAlias_ClassicQuoteServesTheQuietBook: the
// classic-keyed, classic-quoted read serves the deep book's STALE bucket,
// flagged stale, and never consults the fresh SAC/SAC pool. The base's
// SAC form IS walked (second, against the literal classic quote — a pair
// no venue produces), which is what makes the assertion non-vacuous: the
// alias family reached the SAC spelling and still could not land on the
// pool.
func TestPrice_ThinPoolThirdAlias_ClassicQuoteServesTheQuietBook(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice {
		t.Errorf("price = %q, want the quiet SDEX book's %q — the fresh Soroban pool must not displace it: %s", env.Data.Price, thinPoolDeepPrice, body)
	}
	if !env.Flags.Stale {
		t.Errorf("stale = false, want true — a quiet book served as a price must say so: %s", body)
	}
	if got := strings.Join(env.Sources, ","); got != "sdex" {
		t.Errorf("sources = %q, want sdex", got)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		pegAliasAquaClassic + "/" + pegAliasUSDCClassic,
		pegAliasAquaSAC + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_NativeQuoteWalkStaysOnTheLiteralQuote is
// the same shape for XLM, whose three-way family is unconditional
// (native / crypto:XLM / the XLM SAC) and whose Soroban book is stored
// SAC/SAC (measured on r1; see the queryDB doc in
// internal/storage/timescale/usd_fx_resolver.go). All three base forms
// are walked, in canonical order, every one against classic USDC.
func TestPrice_ThinPoolThirdAlias_NativeQuoteWalkStaysOnTheLiteralQuote(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, "native", pegAliasUSDCClassic, canonical.XLMSacContractID, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset=native&quote="+pegAliasUSDCClassic)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice || !env.Flags.Stale {
		t.Errorf("price/stale = %q/%t, want %q/true (the SDEX bucket, flagged stale): %s", env.Data.Price, env.Flags.Stale, thinPoolDeepPrice, body)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		"native/" + pegAliasUSDCClassic,
		"crypto:XLM/" + pegAliasUSDCClassic,
		canonical.XLMSacContractID + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_FiatProxyWalksClassicPegsOnly covers the
// default request shape, ?quote=fiat:USD, which no on-chain venue quotes
// and which therefore resolves through the stablecoin proxy. The proxy
// walks the operator's declared pegs in their CLASSIC spelling only
// (config.TradesConfig.USDPeggedClassics rejects any other form), so
// the deep book answers and the SAC-quoted pool is never a candidate —
// even though it is fresher.
func TestPrice_ThinPoolThirdAlias_FiatProxyWalksClassicPegsOnly(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaClassic+"&quote=fiat:USD")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice || !env.Flags.Triangulated {
		t.Errorf("price/triangulated = %q/%t, want %q/true (the classic-peg proxy of the SDEX book): %s", env.Data.Price, env.Flags.Triangulated, thinPoolDeepPrice, body)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		pegAliasAquaClassic + "/fiat:USD",
		pegAliasAquaSAC + "/fiat:USD",
		pegAliasAquaClassic + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_SACKeyedRequestServesTheNamedPool states
// the other half of the contract, and proves the pool is reachable at
// all (so the three tests above are not passing against a pair the stub
// could never have answered): a caller who names the SAC form on BOTH
// sides is asking about that pool, and gets that pool's own price, its
// own sources, and no alias fallback ahead of it. The thin-market gates
// that then apply are the reader's (substance, trailing-baseline guard,
// freshness) — documented in the d7 thin-pool third-alias VWAP review under docs/methodology/.
func TestPrice_ThinPoolThirdAlias_SACKeyedRequestServesTheNamedPool(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaSAC+"&quote="+pegAliasUSDCSAC)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolThinPrice || env.Flags.Stale {
		t.Errorf("price/stale = %q/%t, want %q/false (the named pool's own bucket): %s", env.Data.Price, env.Flags.Stale, thinPoolThinPrice, body)
	}
	if env.Data.AssetID != pegAliasAquaSAC || env.Data.Quote != pegAliasUSDCSAC {
		t.Errorf("echo = %s/%s, want the requested SAC spellings", env.Data.AssetID, env.Data.Quote)
	}
	assertCallOrder(t, fx.reader.calls, []string{pegAliasAquaSAC + "/" + pegAliasUSDCSAC})
}

var (
	pegClassicXLMBook = pegAliasUSDCClassic + "/native"
	pegSACPool        = pegAliasUSDCSAC + "/" + canonical.XLMSacContractID
)

const xlmDollarPair = "crypto:XLM/fiat:USD"

var pegSpellings = []string{pegAliasUSDCClassic, pegAliasUSDCSAC}

// pegSelfPairs are the two orientations of the pair the SAC twin would
// build on the sibling walk: itself against its own classic form. Never a
// market, never read.
var pegSelfPairs = []string{
	pegAliasUSDCSAC + "/" + pegAliasUSDCClassic,
	pegAliasUSDCClassic + "/" + pegAliasUSDCSAC,
}

func assertNoPegSelfPairRead(t *testing.T, asked []string) {
	t.Helper()
	for _, pair := range asked {
		for _, self := range pegSelfPairs {
			if pair == self {
				t.Errorf("reader asked for %s — the two sides are one asset, never a market", pair)
			}
		}
	}
}

func pegSnap(base, quote, price string, at time.Time) v1.PriceSnapshot {
	return v1.PriceSnapshot{
		AssetID: base, Quote: quote,
		Price: price, PriceType: "vwap", ObservedAt: v1.WireTime(at), WindowSeconds: 60,
	}
}

// pegServerFor serves prices with the given declared pegs.
func pegServerFor(t *testing.T, prices v1.PriceReader, pegs ...canonical.Asset) string {
	t.Helper()
	srv := v1.New(v1.Options{
		Prices:            prices,
		USDPeggedClassics: pegs,
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	return startHTTPTest(t, srv.Handler()).URL
}

// pegServer installs the wrapper registry and serves prices with its USDC
// declared as the peg.
func pegServer(t *testing.T, prices v1.PriceReader) string {
	t.Helper()
	return pegServerFor(t, prices, installPegAliasRegistry(t))
}

func pegPriceURL(base, spelling string) string {
	return base + "/v1/price?asset=" + spelling + "&quote=fiat:USD"
}

func assertSACPoolUnread(t *testing.T, asked []string, why string) {
	t.Helper()
	if callIndex(asked, pegSACPool) >= 0 {
		t.Errorf("the peg's SAC book was read %s (asked=%v)", why, asked)
	}
}

// xlmCrossReader is the shared cross fixture: the peg's XLM book where SDEX
// prints it (9.5 XLM per USDC) and XLM's dollar market (0.10), a cross of
// 0.95 with the pivot the staler leg.
func xlmCrossReader(pegLegAt, pivotAt time.Time) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegClassicXLMBook: pegSnap(pegAliasUSDCClassic, "native", "9.5", pegLegAt),
			xlmDollarPair:     pegSnap("crypto:XLM", "fiat:USD", "0.10", pivotAt),
		},
		sources: map[string][]string{
			pegClassicXLMBook: {"sdex"},
			xlmDollarPair:     {"coinbase", "bitstamp"},
		},
	}}
}

// dormantBookVsFreshPoolReader: the peg's classic SDEX book is dormant
// (closed bucket, optionally flagged stale) and a thin Soroban pool under
// the peg's SAC id is fresh. Crossed with the 0.10 pivot they disagree by
// more than 2x: 0.95 from the book, 2.00 from the pool.
func dormantBookVsFreshPoolReader(at time.Time, bookStale bool) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegClassicXLMBook: pegSnap(pegAliasUSDCClassic, "native", "9.5", at),
			pegSACPool:        pegSnap(pegAliasUSDCSAC, canonical.XLMSacContractID, "20.0", at),
			xlmDollarPair:     pegSnap("crypto:XLM", "fiat:USD", "0.10", at),
		},
		stale: map[string]bool{pegClassicXLMBook: bookStale},
		sources: map[string][]string{
			pegClassicXLMBook: {"sdex"},
			pegSACPool:        {"soroswap"},
			xlmDollarPair:     {"coinbase"},
		},
	}}
}

// recordingRecentReader records every pair RecentClosedSnapshots was asked for.
type recordingRecentReader struct {
	stubPriceReader
	mu    sync.Mutex
	calls []string
}

func (r *recordingRecentReader) RecentClosedSnapshots(ctx context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	r.mu.Lock()
	r.calls = append(r.calls, a.String()+"/"+q.String())
	r.mu.Unlock()
	return r.stubPriceReader.RecentClosedSnapshots(ctx, a, q, n)
}

// recordingPriceAtReader implements v1.PriceAtReader keyed on
// "<base>/<quote>" and records every pair asked. A hit is stamped at the
// requested ts, so it is inside the lookback and the staleness bound.
type recordingPriceAtReader struct {
	byPair map[string]string
	mu     sync.Mutex
	calls  []string
}

func (r *recordingPriceAtReader) PriceAt(
	_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	r.mu.Lock()
	r.calls = append(r.calls, key)
	r.mu.Unlock()
	value, ok := r.byPair[key]
	if !ok {
		return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
	}
	return value, ts, 60, nil
}

func (r *recordingPriceAtReader) pairsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// pegPriceAtServer serves a two-peg (USDC, PYUSD) point-in-time reader where
// only SAC/PYUSD has a row, so a guard that skips the SAC twin's own form
// must still walk on to the second peg.
func pegPriceAtServer(t *testing.T) (string, *recordingPriceAtReader) {
	t.Helper()
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	reader := &recordingPriceAtReader{byPair: map[string]string{
		pegAliasUSDCSAC + "/" + pegAliasPYUSDClassic: "1.0004",
	}}
	srv := v1.New(v1.Options{PriceAt: reader, USDPeggedClassics: []canonical.Asset{usdc, pyusd}})
	return startHTTPTest(t, srv.Handler()).URL, reader
}

func assertSecondPegWalked(t *testing.T, reader *recordingPriceAtReader) {
	t.Helper()
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCSAC+"/"+pegAliasPYUSDClassic) < 0 {
		t.Errorf("the second peg was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// pegLegErrorReader is a recordingPriceReader with PER-PAIR errors: a pair
// in `errs` fails with exactly that error AFTER the call is recorded.
type pegLegErrorReader struct {
	recordingPriceReader
	errs map[string]error
}

func (r *pegLegErrorReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, err := r.recordingPriceReader.LatestPrice(ctx, a, q)
	if perPair, listed := r.errs[a.String()+"/"+q.String()]; listed {
		return v1.PriceSnapshot{}, nil, false, perPair
	}
	return snap, srcs, stale, err
}

// withheldClassicBookLivePoolReader is the gate-bypass shape: the peg's
// classic XLM book fails with classicErr while a live pool holds the SAC id.
// ScamGate.Withheld is false for any non-classic base, so a walk that took
// the failure as "no market" would republish the refused market through the
// ungated spelling.
func withheldClassicBookLivePoolReader(at time.Time, classicErr error) *pegLegErrorReader {
	return &pegLegErrorReader{
		recordingPriceReader: *dormantBookVsFreshPoolReader(at, false),
		errs:                 map[string]error{pegClassicXLMBook: classicErr},
	}
}

// assertDeclarationServed checks the declaration's wire shape: the flat 1:1
// constant, labelled `peg`, stamped with the adoption time.
func assertDeclarationServed(t *testing.T, env pegEnvelope, spelling string) {
	t.Helper()
	if env.Data.Price != "1.000000000000" || env.Data.PriceType != "peg" {
		t.Errorf("%s: served %s (%s), want the declaration 1.000000000000 (peg)",
			spelling, env.Data.Price, env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Time().Equal(declaredPegAdoptedAt) {
		t.Errorf("%s: observed_at = %s, want the declaration stamp %s",
			spelling, env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if env.Data.AssetID != spelling {
		t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
	}
}

// assertPriceWithheld checks url answers 404 with the price-withheld
// problem type and serves no price.
func assertPriceWithheld(t *testing.T, url string) {
	t.Helper()
	resp := mustGet(t, url)
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("%s: status = %d body %s, want 404 errors/price-withheld", url, resp.StatusCode, body)
	}
	if strings.Contains(body, "1.000000000000") {
		t.Errorf("%s: the declaration was served over a refused market: %s", url, body)
	}
}

// gatingPegReader wires the optional `proxyPairGate` (the bounded
// recent-bucket probe run before an unbounded last-trade scan) onto the
// recording reader, and can make the probe MISS or FAIL. `exists` is keyed
// "<base>/<quote>" and defaults to false (no closed 1m bucket recently).
type gatingPegReader struct {
	recordingPriceReader
	exists    map[string]bool
	existsErr map[string]error

	pmu    sync.Mutex
	probes []string
}

func (r *gatingPegReader) RecentClosedVWAP1mExists(_ context.Context, base, quote canonical.Asset) (bool, error) {
	key := base.String() + "/" + quote.String()
	r.pmu.Lock()
	r.probes = append(r.probes, key)
	r.pmu.Unlock()
	if err, failing := r.existsErr[key]; failing {
		return false, err
	}
	return r.exists[key], nil
}

func (r *gatingPegReader) probesMade() []string {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return append([]string(nil), r.probes...)
}

// gatingDormantBookReader is the dormant-book/fresh-pool fixture with the
// gate reporting only the SAC pool live.
func gatingDormantBookReader(at time.Time) *gatingPegReader {
	return &gatingPegReader{
		recordingPriceReader: *dormantBookVsFreshPoolReader(at, false),
		exists:               map[string]bool{pegSACPool: true},
	}
}

// scamGatedPegReader wires the REAL pricingguard.ScamGate onto the recording
// reader the way storePriceReader does on the served binary: the raw
// requested base goes to the gate once a row is found, and a refusal becomes
// a flagged-issuer v1.ErrPriceWithheld. /v1/price has no handler-side gate
// call, so this seam is the only place a withholding reaches it. The gate is
// real over a fake directory because the resolution under test lives inside
// it; a stub keyed on the asset id would pin nothing.
type scamGatedPegReader struct {
	recordingPriceReader
	scam *pricingguard.ScamGate
}

func (r *scamGatedPegReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, err := r.recordingPriceReader.LatestPrice(ctx, a, q)
	if err == nil && r.scam.Withheld(ctx, a, "price_read") {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer)
	}
	return snap, srcs, stale, err
}

// volatilePriceBodyFields are the only /v1/price members two spellings of
// ONE asset may differ on: `as_of` (response clock) and `asset_id` (echo).
var volatilePriceBodyFields = regexp.MustCompile(`"(as_of|asset_id)":"[^"]*"`)

// maskedPriceBody fetches a /v1/price body with those two fields blanked, as
// served bytes rather than a decoded snapshot: `sources` and flags are not
// struct members and a disagreement there is what this looks for.
func maskedPriceBody(t *testing.T, url string) string {
	t.Helper()
	resp := mustGet(t, url)
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	return volatilePriceBodyFields.ReplaceAllString(body, `"$1":"<masked>"`)
}

// flaggedPegLivePoolReader: the declared peg's issuer carries a scam-class
// directory tag, it has NO classic XLM book, and its SAC spelling holds a
// live pool that crosses to 2.00. Absent (not refused) is deliberate: "found
// nothing" is the one verdict that reaches the SAC book at all. The peg is
// the flagged issuance newSACSpellingFixture keeps for this role, declared
// per server so installPegAliasRegistry is left as the rest of the file finds it.
func flaggedPegLivePoolReader(at time.Time, f sacSpellingFixture) *scamGatedPegReader {
	pool := f.sac.String() + "/" + canonical.XLMSacContractID
	return &scamGatedPegReader{
		recordingPriceReader: recordingPriceReader{stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				pool:          pegSnap(f.sac.String(), canonical.XLMSacContractID, "20.0", at),
				xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", at),
			},
			sources: map[string][]string{pool: {"soroswap"}, xlmDollarPair: {"coinbase"}},
		}},
		scam: f.gate(),
	}
}

// orderedPriceReader is a stubPriceReader that records every (base,
// quote) pair LatestPrice was asked for, in call order, so a test can
// prove not just which pair answered but which pairs were NEVER
// consulted. It also satisfies the optional proxyPairGate seam, so the
// stablecoin proxy walk runs in its production (gated) shape: a pair is
// "recent" exactly when the stub holds a snapshot for it.
type orderedPriceReader struct {
	stubPriceReader
	calls []string
}

func (r *orderedPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.calls = append(r.calls, a.String()+"/"+q.String())
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

func (r *orderedPriceReader) RecentClosedVWAP1mExists(_ context.Context, base, quote canonical.Asset) (bool, error) {
	_, ok := r.snapshots[base.String()+"/"+quote.String()]
	return ok, nil
}

// thinPoolFixture is the D7 shape for one wrapped classic asset:
//
//   - deepPair  — the SDEX book, classic/classic, deep but QUIET: its
//     latest closed bucket is older than the freshness window, so the
//     reader reports it stale.
//   - thinPair  — the Soroban pool, SAC/SAC, thin but FRESH: one trade
//     printed this minute at five times the book's price.
type thinPoolFixture struct {
	reader   *orderedPriceReader
	deepPair string
	thinPair string
}

const (
	thinPoolDeepPrice = "0.0010"
	thinPoolThinPrice = "0.0050"
)

func newThinPoolFixture(t *testing.T, deepBase, deepQuote, thinBase, thinQuote string) thinPoolFixture {
	t.Helper()
	deep := deepBase + "/" + deepQuote
	thin := thinBase + "/" + thinQuote
	reader := &orderedPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			deep: {
				AssetID: deepBase, Quote: deepQuote, Price: thinPoolDeepPrice, PriceType: "vwap",
				ObservedAt: v1.WireTime(time.Now().Add(-2 * time.Hour).UTC()),
			},
			thin: {
				AssetID: thinBase, Quote: thinQuote, Price: thinPoolThinPrice, PriceType: "vwap",
				ObservedAt: v1.WireTime(time.Now().UTC()),
			},
		},
		stale:   map[string]bool{deep: true, thin: false},
		sources: map[string][]string{deep: {"sdex"}, thin: {"soroswap"}},
	}}
	return thinPoolFixture{reader: reader, deepPair: deep, thinPair: thin}
}

type thinPoolEnvelope struct {
	Data    v1.PriceSnapshot `json:"data"`
	Flags   v1.Flags         `json:"flags"`
	Sources []string         `json:"sources"`
}

func getThinPoolPrice(t *testing.T, url string) (int, thinPoolEnvelope, string) {
	t.Helper()
	resp := mustGet(t, url)
	body, _ := readAll(resp)
	var env thinPoolEnvelope
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode: %v: %s", err, body)
		}
	}
	return resp.StatusCode, env, body
}

// assertNoSACQuotedRead is the invariant every classic-quoted test below
// shares: no LatestPrice call may bind a SAC form as the QUOTE.
func assertNoSACQuotedRead(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		q := c[strings.LastIndex(c, "/")+1:]
		if a, err := canonical.ParseAsset(q); err == nil && a.Type == canonical.AssetSoroban {
			t.Errorf("LatestPrice read %q — the walk bound a SAC form as the quote; the thin SAC/SAC pool is reachable from a classic-quoted request", c)
		}
	}
}

func assertCallOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("LatestPrice call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LatestPrice call[%d] = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// usdcClassicID is Circle's mainnet USDC — the ONE peg the shipped
// operator configuration declares
// (configs/ansible/roles/archival-node/templates/stellarindex.toml.j2,
// [trades].usd_pegged_classic_assets), and the asset the live defect was
// found on. Every test in this file runs in that single-peg shape: with
// one declared peg there is no sibling peg to walk, so the only market a
// depeg can print on is the peg's own XLM book.
const usdcClassicID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// declaredPegAdoptedAt is a fixed adoption stamp the tests pin the wire
// value to, well in the past so it can never be confused with a request
// clock.
var declaredPegAdoptedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// recordingPriceReader is a stubPriceReader that records every pair
// LatestPrice was asked for, so a test can prove which markets a route
// consulted — and which it never touched.
type recordingPriceReader struct {
	stubPriceReader
	mu    sync.Mutex
	calls []string
}

func (r *recordingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, a.String()+"/"+q.String())
	r.mu.Unlock()
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

func (r *recordingPriceReader) pairsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.calls...)
	sort.Strings(out)
	return out
}

type pegEnvelope struct {
	Data  v1.PriceSnapshot `json:"data"`
	AsOf  time.Time        `json:"as_of"`
	Flags v1.Flags         `json:"flags"`
}

func getPegEnvelope(t *testing.T, url string) pegEnvelope {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	var env pegEnvelope
	mustDecode(t, resp, &env)
	return env
}

// windowMetaPairs is lkgPairs plus router meta keyed like it, so a test can
// tell which (pair, window) the handler asked the composite meta for.
type windowMetaPairs struct {
	lkgPairs
	metas map[string][]byte
}

func (w windowMetaPairs) LookupCompositeMeta(
	_ context.Context, base, quote canonical.Asset, window time.Duration,
) ([]byte, bool, error) {
	m, ok := w.metas[base.String()+"/"+quote.String()+"/"+strconv.Itoa(int(window/time.Second))]
	return m, ok, nil
}

type windowRecordingConfidence struct{ windows []time.Duration }

func (c *windowRecordingConfidence) LookupConfidence(
	_ context.Context, _, _ canonical.Asset, window time.Duration,
) (v1.PriceSnapshotConfidence, bool, error) {
	c.windows = append(c.windows, window)
	return v1.PriceSnapshotConfidence{}, false, nil
}
