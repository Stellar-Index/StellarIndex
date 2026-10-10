package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// Envelope flags are OR over per-row signals, like Stale: one frozen row
// sets frozen and single_source on the batch.
func TestPriceBatch_FrozenORedAcrossRows(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD":   {Price: "0.07", PriceType: "vwap"},
			"fiat:EUR/fiat:USD": {Price: "1.10", PriceType: "vwap"},
		},
		sources: map[string][]string{
			"native/fiat:USD":   {"sdex", "soroswap"},
			"fiat:EUR/fiat:USD": {"sdex", "soroswap"},
		},
	}
	// Only the EUR row freezes; it serves its held last-known-good (see
	// TestPrice_FrozenSetsBothFlags) while the other keeps its closed bucket.
	held := &stubTriangulatedPriceLooker{value: "1.08", found: true}
	_, body := priceGet(t, v1.Options{Prices: reader, Freeze: &batchFreezeLooker{frozenForBase: "EUR"}, Triangulated: held},
		"/v1/price/batch?asset_ids=native,fiat:EUR&quote=fiat:USD")
	checkBody(t, body, []string{`"price":"1.08"`, `"price":"0.07"`, `"frozen":true`, `"single_source":true`}, []string{`"1.10"`})
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
