// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The fiat-quoted fixtures below are served from the declared peg
// usdcClassicID (Circle's Stellar USDC in its classic spelling, declared
// beside price_test.go's fixtures).

// aquaClassicID is a classic asset with exactly one canonical spelling
// under the default registry, so a pair on it probes as itself and
// nothing else.
const aquaClassicID = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

// coverageFloorProbe is the CoverageFloorReader test double.
//
// It enforces the SAME window contract the real store does — `to` must
// be strictly after `from` — because that guard is the whole reason the
// probe has an explicit window at all. A fake that shrugged at a
// degenerate range would let a handler bug ("probe from now to now")
// pass every test here while, in production, timescale.Store returned
// an error and the signal silently vanished. Recording the breach as
// well as returning the error means a test can assert on the shape
// directly rather than inferring it from a missing flag.
//
// Two fixture shapes. The flat `floor`/`found`/`err` triple answers
// every pair alike, for the tests that pin the annotation's arithmetic.
// `byPair`, when set, answers per pair and models the real store's
// folds, per method:
//
//   - EarliestBucket looks a key up under the alias-canonical form of
//     each leg, in both orders — so a fixture stored as USDC/AQUA is
//     found by AQUA/USDC.
//   - EarliestBucketAsStored does the same in the given order only, so
//     that fixture is NOT found by AQUA/USDC.
//   - EarliestBucketLiteralQuote folds the base leg onto its family and
//     takes the quote leg's LITERAL spelling, in both orders — so a
//     fixture stored under a peg's SAC wrapper is invisible to a probe
//     that named the peg's classic form, exactly as prices_1d would
//     answer a query whose quote array holds one string.
type coverageFloorProbe struct {
	mu sync.Mutex

	floor time.Time
	found bool
	err   error
	// byPair maps probeKey(base, quote) → floor. A pair absent from a
	// non-nil map is "read and absent".
	byPair map[string]time.Time
	// failPairs names the probeKeys whose read fails, for the tests that
	// pin how a set folds a constituent it could not read.
	failPairs map[string]bool

	calls         int
	storedCalls   int
	literalCalls  int
	granularities []string
	windows       [][2]time.Time
	pairs         []canonical.Pair
	spans         []string
	badWindows    int
}

// probeKey is the double's per-market fixture key: the two legs in the
// literal spellings the rung would hold them under, in the stored
// order. Literal rather than alias-canonical because which SPELLINGS a
// read reaches is the property under test — a market held only under a
// declared peg's SAC wrapper must be findable by one read and not by
// another.
func probeKey(base, quote canonical.Asset) string {
	return base.String() + "/" + quote.String()
}

// probeSpanKeys enumerates every stored market one read would look at,
// modelling the statement's cross-join: the base leg's alias family
// crossed with the quote leg's family — or, for the quote-literal read,
// with the one spelling it was given — and, unless the read is bound to
// one orientation, the flipped arm as well.
func probeSpanKeys(pair canonical.Pair, span string) []string {
	quotes := []canonical.Asset{pair.Quote}
	if span != "literal_quote" {
		quotes = canonical.AssetAliases(pair.Quote)
	}
	bases := canonical.AssetAliases(pair.Base)
	out := make([]string, 0, 2*len(bases)*len(quotes))
	for _, b := range bases {
		for _, q := range quotes {
			out = append(out, probeKey(b, q))
		}
	}
	if span == "as_stored" {
		return out
	}
	for _, b := range bases {
		for _, q := range quotes {
			out = append(out, probeKey(q, b))
		}
	}
	return out
}

func (p *coverageFloorProbe) EarliestBucket(
	_ context.Context, pair canonical.Pair, granularity string, from, to time.Time,
) (time.Time, bool, error) {
	return p.answer(pair, granularity, from, to, "aliased")
}

func (p *coverageFloorProbe) EarliestBucketAsStored(
	_ context.Context, pair canonical.Pair, granularity string, from, to time.Time,
) (time.Time, bool, error) {
	return p.answer(pair, granularity, from, to, "as_stored")
}

func (p *coverageFloorProbe) EarliestBucketLiteralQuote(
	_ context.Context, pair canonical.Pair, granularity string, from, to time.Time,
) (time.Time, bool, error) {
	return p.answer(pair, granularity, from, to, "literal_quote")
}

func (p *coverageFloorProbe) answer(pair canonical.Pair, granularity string, from, to time.Time, span string) (time.Time, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch span {
	case "as_stored":
		p.storedCalls++
	case "literal_quote":
		p.literalCalls++
	}
	p.granularities = append(p.granularities, granularity)
	p.windows = append(p.windows, [2]time.Time{from, to})
	p.pairs = append(p.pairs, pair)
	p.spans = append(p.spans, span)
	if !to.After(from) {
		p.badWindows++
		return time.Time{}, false, fmt.Errorf("coverage probe: to %v <= from %v", to, from)
	}
	if p.err != nil {
		return time.Time{}, false, p.err
	}
	if p.byPair == nil {
		return p.floor, p.found, nil
	}
	keys := probeSpanKeys(pair, span)
	for _, k := range keys {
		if p.failPairs[k] {
			return time.Time{}, false, errors.New("prices_1d unavailable for this pair")
		}
	}
	// min() over every arm, as the statement computes it.
	var (
		floor time.Time
		found bool
	)
	for _, k := range keys {
		f, ok := p.byPair[k]
		if !ok {
			continue
		}
		if !found || f.Before(floor) {
			floor, found = f, true
		}
	}
	return floor, found, nil
}

func (p *coverageFloorProbe) snapshot() (calls, badWindows int, grains []string, windows [][2]time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.badWindows, append([]string(nil), p.granularities...), append([][2]time.Time(nil), p.windows...)
}

func (p *coverageFloorProbe) stored() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.storedCalls
}

// literal counts the quote-literal probes — the read the fiat /v1/ohlc
// series takes.
func (p *coverageFloorProbe) literal() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.literalCalls
}

// probed returns every pair the double was asked about, in order.
func (p *coverageFloorProbe) probed() []canonical.Pair {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]canonical.Pair(nil), p.pairs...)
}

// probedRead is one consultation: the pair asked about and the read
// that asked, which together decide which stored markets were spanned.
type probedRead struct {
	pair canonical.Pair
	span string
}

// probedReads pairs each call with the read that made it, so a test can
// reconstruct the spanned population from the SAME model the double
// answers with ([probeSpanKeys]) rather than assuming one.
func (p *coverageFloorProbe) probedReads() []probedRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]probedRead, len(p.pairs))
	for i := range p.pairs {
		out[i] = probedRead{pair: p.pairs[i], span: p.spans[i]}
	}
	return out
}

// heal turns a failing flat fixture into one that answers `floor`, for
// the tests that pin which outcomes the memo keeps.
func (p *coverageFloorProbe) heal(floor time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err, p.floor, p.found = nil, floor, true
}

// coverageMeta is the wire projection the assertions below read: the
// envelope-level annotation plus the one flag it explains. `data` is
// deliberately not modelled — the three surfaces under test return
// three different body shapes, which is why the annotation lives on the
// envelope in the first place.
type coverageMeta struct {
	CoverageFrom *time.Time `json:"coverage_from"`
	Flags        struct {
		OutsideCoverage bool `json:"outside_coverage"`
	} `json:"flags"`
}

// coverageEnvelope adds the OHLC series body, so the series tests can
// assert the fixture really did come back empty before reading the
// signal that explains the emptiness.
type coverageEnvelope struct {
	coverageMeta
	Data struct {
		Intervals []v1.OHLCSeriesBar `json:"intervals"`
	} `json:"data"`
}

// xlmCoverageFloor is the live floor of crypto:XLM / fiat:USD on the
// daily aggregate, measured on r1. The fixtures below are
// built around it so the windows under test are the real ones: 2016 is
// genuinely below it, and the pair's daily candles genuinely have a
// multi-year hole ABOVE it.
var xlmCoverageFloor = time.Date(2018, 7, 1, 0, 0, 0, 0, time.UTC)

func ohlcCoverageServer(t *testing.T, probe *coverageFloorProbe) *testServer {
	t.Helper()
	return httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{ohlcBars: nil},
		CoverageFloor: probe,
	}))
}

func ohlcCoverageGet(t *testing.T, ts *testServer, from, to string) coverageEnvelope {
	t.Helper()
	return ohlcCoverageGetPair(t, ts, "base=crypto:XLM&quote=fiat:USD", from, to)
}

// ohlcCoverageGetPair is [ohlcCoverageGet] for an arbitrary pair query.
func ohlcCoverageGetPair(t *testing.T, ts *testServer, pairQS, from, to string) coverageEnvelope {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/ohlc?"+pairQS+"&interval=1d&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env coverageEnvelope
	mustDecode(t, resp, &env)
	if len(env.Data.Intervals) != 0 {
		t.Fatalf("fixture returned %d bars, want an empty series", len(env.Data.Intervals))
	}
	return env
}

// TestCoverageFloorProbe_IsBoundedAndDaily pins the read the handler
// actually issues for a pair served from itself: one probe, on the
// DAILY rung, over a window bounded at both ends and strictly
// increasing.
//
// The rung is not the requested interval on purpose. prices_1d carries
// no retention policy — migration 0031 removed the ones migration 0002
// had placed on prices_1m / prices_15m, and migration 0156 attaches one
// to prices_1m and to nothing else (90 days, shipped disabled). So the
// minute rung is the only rung whose bottom edge can move with the
// clock, and only where that policy is armed; nothing in the daily rung
// has been dropped by age. It is also the coarsest, holds the fewest
// rows per pair, and is the cheapest to prove empty — so a handler that
// probed at the requested grain would spend more to describe a rung
// whose contents follow its own refresh schedule, and on the minute
// grain could describe one a retention run had truncated.
//
// The fixture is a stablecoin-quoted pair, which the series read serves
// from the pair itself; a fiat-quoted pair is served from a constituent
// set and probes each constituent (see
// TestCoverageFloor_ProbedOncePerConstituentFamily).
func TestCoverageFloorProbe_IsBoundedAndDaily(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)
	ohlcCoverageGetPair(t, ts, "base=crypto:XLM&quote="+usdcClassicID, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")

	calls, bad, grains, windows := probe.snapshot()
	if calls != 1 {
		t.Fatalf("probe calls = %d, want exactly 1 per empty response on a pair served from itself", calls)
	}
	if bad != 0 {
		t.Errorf("probe was called with a degenerate window %d time(s)", bad)
	}
	if grains[0] != "1d" {
		t.Errorf("probe granularity = %q, want 1d (the coarsest rung)", grains[0])
	}
	from, to := windows[0][0], windows[0][1]
	if from.IsZero() || to.IsZero() {
		t.Errorf("probe window unbounded: [%v, %v)", from, to)
	}
	if !to.After(from) {
		t.Errorf("probe window not strictly increasing: [%v, %v)", from, to)
	}
	if from.Year() > 2015 {
		t.Errorf("probe lower bound %v is above pubnet genesis — it would hide real coverage", from)
	}
}

// TestCoverageFloor_ProbedOncePerConstituentFamily pins the probe's cost
// bound: the first empty answer costs one probe per constituent family
// and repeats under any alias spelling of the base cost nothing, so a
// caller cannot multiply reads at will.
func TestCoverageFloor_ProbedOncePerConstituentFamily(t *testing.T) {
	window := "&interval=1d&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z"
	for _, tc := range []struct {
		name      string
		quote     string
		wantFirst int
	}{
		// native and crypto:XLM are one asset: one probe, then free.
		{"stablecoin_quote", usdcClassicID, 1},
		// The direct pair plus the USD stablecoin backers the aggregator
		// expands to (no classic pegs are configured on this server).
		{"fiat_quote", "fiat:USD", 1 + len(aggregateUSDBackers())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
			ts := ohlcCoverageServer(t, probe)
			var first int
			for i, base := range []string{"crypto:XLM", "native", "crypto:XLM"} {
				q := "/v1/ohlc?base=" + base + "&quote=" + tc.quote + window
				if resp := mustGet(t, ts.URL+q); resp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s: status %d", q, resp.StatusCode)
				}
				if i == 0 {
					first, _, _, _ = probe.snapshot()
					if first != tc.wantFirst {
						t.Fatalf("probe calls = %d after the first request, want %d", first, tc.wantFirst)
					}
				}
			}
			if calls, _, _, _ := probe.snapshot(); calls != first {
				t.Errorf("probe calls = %d after repeats under both spellings, want %d: the memo must absorb them", calls, first)
			}
		})
	}
}

// aggregateUSDBackers is the abstract USD stablecoin set the fiat
// expansion adds to a `fiat:USD` target, deduplicated the way the
// probe's memo would see it.
func aggregateUSDBackers() []string {
	return aggregate.FiatBackers("USD")
}

// TestCoverageFloor_NotProbedWhenTheAnswerIsPopulated — the signal only
// exists to explain an EMPTY answer, so a populated series must not pay
// for it. This is the bound on the whole feature's cost: at most one
// extra read behind a read that already returned nothing.
func TestCoverageFloor_NotProbedWhenTheAnswerIsPopulated(t *testing.T) {
	bar := mkSeriesBar(xlmCoverageFloor, "0.16", "0.17", "0.15", "0.165", "1000", "165", 4)
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{bar}},
		CoverageFloor: probe,
	}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=crypto:XLM&quote=fiat:USD&interval=1d&limit=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if calls, _, _, _ := probe.snapshot(); calls != 0 {
		t.Errorf("probe calls = %d on a populated series, want 0", calls)
	}
}

// TestCoverageFloor_UnwiredReaderChangesNothing — the whole signal is
// opt-in. With no CoverageFloor wired the four surfaces serve their
// pre-signal bytes: no `coverage_from` key, no `outside_coverage` key.
func TestCoverageFloor_UnwiredReaderChangesNothing(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{ohlcBars: nil}}))
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=crypto:XLM&quote=fiat:USD&interval=1d&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, absent := range []string{"coverage_from", "outside_coverage"} {
		if strings.Contains(body, absent) {
			t.Errorf("body carries %q with no CoverageFloor wired: %s", absent, body)
		}
	}
}

// TestPriceAt_NotFoundCarriesCoverageExtensions — /v1/price/at answers
// "nothing there" with a 404, which has no envelope to annotate, so the
// two members ride on the problem body as RFC 9457 §3.2 extensions. An
// instant below the floor is a coverage answer; an instant inside it
// (the pair's real 2021-2026 daily hole) is a market one.
func TestPriceAt_NotFoundCarriesCoverageExtensions(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := httpTestServer(t, v1.New(v1.Options{
		PriceAt:       priceAtMissStub{},
		CoverageFloor: probe,
	}))

	type problemBody struct {
		Status          int        `json:"status"`
		CoverageFrom    *time.Time `json:"coverage_from"`
		OutsideCoverage bool       `json:"outside_coverage"`
	}

	below := mustGet(t, ts.URL+"/v1/price/at?asset=crypto:XLM&quote=fiat:USD&ts=2016-01-01T00:00:00Z")
	if below.StatusCode != http.StatusNotFound {
		t.Fatalf("below-floor status = %d, want 404", below.StatusCode)
	}
	var got problemBody
	mustDecode(t, below, &got)
	if !got.OutsideCoverage {
		t.Errorf("outside_coverage = false on a 404 for an instant below the floor")
	}
	if got.CoverageFrom == nil || !got.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want %s", got.CoverageFrom, xlmCoverageFloor)
	}

	inside := mustGet(t, ts.URL+"/v1/price/at?asset=crypto:XLM&quote=fiat:USD&ts=2023-06-01T00:00:00Z")
	if inside.StatusCode != http.StatusNotFound {
		t.Fatalf("in-coverage status = %d, want 404", inside.StatusCode)
	}
	var gap problemBody
	mustDecode(t, inside, &gap)
	if gap.OutsideCoverage {
		t.Errorf("outside_coverage = true for an instant inside coverage — that is a market gap, not a coverage one")
	}
	if gap.CoverageFrom == nil || !gap.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want the floor echoed on the gap answer too", gap.CoverageFrom)
	}
}

// emptyHistoryServer serves empty history reads with the probe wired.
func emptyHistoryServer(t *testing.T, probe *coverageFloorProbe) *testServer {
	t.Helper()
	return httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{},
		CoverageFloor: probe,
	}))
}

// priceAtMissStub always misses, which is the only path that reaches
// the coverage-annotated 404.
type priceAtMissStub struct{}

func (priceAtMissStub) PriceAt(context.Context, canonical.Pair, time.Time, time.Duration) (string, time.Time, int, error) {
	return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
}

// drainedHistoryCursor is a syntactically valid /v1/history cursor —
// base64url of "<ts_ns>:<ledger>:<source>:<tx_hash>:<op_index>" — of
// the shape a client holds after draining a page.
var drainedHistoryCursor = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
	"%d:1234:sdex:%s:0",
	time.Date(2016, 2, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
	strings.Repeat("ab", 32),
)))

// ─── Fiat quotes: the floor is the served constituent set's ───────
//
// Nothing on chain quotes in `fiat:USD`. A fiat-quoted request is
// answered from the USD-pegged constituents each surface enumerates —
// /v1/ohlc combines them, /v1/chart walks a proxy list and then derives
// through XLM, /v1/price/at retries the declared pegs — so the floor
// behind an empty fiat-quoted answer must be measured over that same
// set. A floor read on the literal pair alone is wrong in both
// directions: a constituent with earlier buckets makes a served-and-
// quiet window look uncovered, and a literal bucket no constituent read
// would return makes an uncovered window look quiet.

var (
	pegFloor2021    = time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	directFloor2024 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
)

func mustParseAsset(t *testing.T, id string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(id)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", id, err)
	}
	return a
}

// fiatCoverageServer wires the probe with one declared USD peg (classic
// USDC), so every fiat-quoted surface has a constituent to reach.
func fiatCoverageServer(t *testing.T, probe *coverageFloorProbe) *testServer {
	t.Helper()
	return httpTestServer(t, v1.New(v1.Options{
		History:           &stubHistoryReader{},
		PriceAt:           priceAtMissStub{},
		CoverageFloor:     probe,
		USDPeggedClassics: []canonical.Asset{mustParseAsset(t, usdcClassicID)},
	}))
}

// assertCoverage compares the wire annotation against the expected
// floor (nil = must be absent) and flag.
func assertCoverage(t *testing.T, got coverageMeta, wantFrom *time.Time, wantOutside bool) {
	t.Helper()
	switch {
	case wantFrom == nil && got.CoverageFrom != nil:
		t.Errorf("coverage_from = %s, want absent", got.CoverageFrom.Format(time.RFC3339))
	case wantFrom != nil && got.CoverageFrom == nil:
		t.Errorf("coverage_from absent, want %s", wantFrom.Format(time.RFC3339))
	case wantFrom != nil && !got.CoverageFrom.Equal(*wantFrom):
		t.Errorf("coverage_from = %s, want %s", got.CoverageFrom.Format(time.RFC3339), wantFrom.Format(time.RFC3339))
	}
	if got.Flags.OutsideCoverage != wantOutside {
		t.Errorf("flags.outside_coverage = %v, want %v", got.Flags.OutsideCoverage, wantOutside)
	}
}

// TestPriceAt_FiatQuoteFloorIsThePegSet — /v1/price/at retries the
// declared USD pegs after the literal pair, so its 404's coverage
// members are measured over the pair and those pegs together.
func TestPriceAt_FiatQuoteFloorIsThePegSet(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	usd := mustParseAsset(t, "fiat:USD")
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		probeKey(aqua, usd):  directFloor2024,
		probeKey(aqua, usdc): pegFloor2021,
	}}
	ts := fiatCoverageServer(t, probe)

	type problemBody struct {
		CoverageFrom    *time.Time `json:"coverage_from"`
		OutsideCoverage bool       `json:"outside_coverage"`
	}
	get := func(t *testing.T, tsParam string) problemBody {
		t.Helper()
		resp := mustGet(t, ts.URL+"/v1/price/at?asset="+aquaClassicID+"&quote=fiat:USD&ts="+tsParam)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var got problemBody
		mustDecode(t, resp, &got)
		return got
	}

	inside := get(t, "2022-06-01T00:00:00Z")
	if inside.OutsideCoverage {
		t.Errorf("outside_coverage = true for an instant the USDC peg covers — the literal pair's 2024 floor is not this lookup's floor")
	}
	if inside.CoverageFrom == nil || !inside.CoverageFrom.Equal(pegFloor2021) {
		t.Errorf("coverage_from = %v, want the peg's %s", inside.CoverageFrom, pegFloor2021.Format(time.RFC3339))
	}
	below := get(t, "2019-06-01T00:00:00Z")
	if !below.OutsideCoverage {
		t.Errorf("outside_coverage = false for an instant below every constituent's floor")
	}
}

// ─── /v1/history: the floor spans the directions the page read spans ──
//
// The raw-trade page walks both legs' alias families and reads each
// form in BOTH stored directions, folding the flipped rows into the
// requested orientation. So a market the decoder recorded only as
// USDC/AQUA IS served under `base=AQUA&quote=USDC`, and the floor over
// both orientations is a claim about rows the page returns — the same
// population, and the same floor, the CAGG-backed surfaces measure.
//
// The correspondence is the invariant, not the width: while the page
// read took the stored orientation as given, this probe was narrowed to
// match it and that market carried no floor here at all. Widening the
// read without widening the probe would under-report the history the
// page can serve, exactly as the reverse over-promised it.

// ─── Fiat quotes: the probe spans what the combine reads, no more ───
//
// The fiat-quoted /v1/ohlc series is combined from the USD-pegged
// constituents, and each constituent is read under the ONE quote
// spelling the peg expansion named it in — a declared peg in its
// classic form, an abstract backer, or the fiat itself.
// [Store.OHLCSeries] takes that spelling literally, so a declared peg's
// SAC wrapper is out of this surface's reach. Soroban AMMs quote in
// exactly that wrapper and their decoders stamp BOTH legs of a pool
// trade as contract addresses, so a pool sits under
// `<AQUA SAC>/<USDC SAC>` while the SDEX book sits under
// `AQUA/USDC-GA5Z…`.
//
// Two consequences, both pinned below. The book is never displaced: a
// thin pool cannot set a served bar's high, low, count or volume,
// because the pool's spelling is never requested at all. And an asset
// whose only USD depth is such a pool serves an empty series — with NO
// floor, because the probe is scoped to the spellings the combine
// requests rather than to the quote's alias family. A quote-alias fold
// there would name the pool's first bucket as the surface's floor and
// call the window quiet; the gap is left visible
// instead of being papered over by a probe that spans more than the
// read.

// installUSDCSACRegistry publishes a registry declaring ONLY the USDC
// SAC wrapper, so the base under test keeps a single spelling and every
// second form in play is on the quote leg. Not parallel: the registry
// is process-global.
func installUSDCSACRegistry(t *testing.T) canonical.Asset {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
		pegAliasUSDCSAC: "USDC:" + pegAliasUSDCIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
	return mustClassicAsset(t, "USDC", pegAliasUSDCIssuer)
}

// fiatSeriesEnvelope is the series body with both flags the fiat
// combine's answer carries, so a served case can assert the bar came
// through the peg and that a populated answer carries no floor.
type fiatSeriesEnvelope struct {
	CoverageFrom *time.Time `json:"coverage_from"`
	Flags        struct {
		OutsideCoverage bool `json:"outside_coverage"`
		Triangulated    bool `json:"triangulated"`
	} `json:"flags"`
	Data struct {
		Intervals []v1.OHLCSeriesBar `json:"intervals"`
	} `json:"data"`
}

// assertBookBar pins that a served bar carries one fixture bar's own
// count, volumes and prices — nothing summed in from a second
// constituent. Prices and quote volume are compared numerically because
// the combine re-renders them at its own fixed precision; count and
// base volume are integer strings it reproduces exactly.
func assertBookBar(t *testing.T, got, want v1.OHLCSeriesBar) {
	t.Helper()
	if !got.T.Time().Equal(want.T.Time()) {
		t.Errorf("t = %s, want %s", got.T.Time().Format(time.RFC3339), want.T.Time().Format(time.RFC3339))
	}
	if got.N != want.N {
		t.Errorf("n = %d, want %d", got.N, want.N)
	}
	if got.VBase != want.VBase {
		t.Errorf("v_base = %q, want %q", got.VBase, want.VBase)
	}
	for _, f := range []struct {
		name, got, want string
	}{
		{"o", got.O, want.O},
		{"h", got.H, want.H},
		{"l", got.L, want.L},
		{"c", got.C, want.C},
		{"v_quote", got.VQuote, want.VQuote},
	} {
		if g, w := mustFloat(t, f.got), mustFloat(t, f.want); !approxEq(g, w) {
			t.Errorf("%s = %s, want %s", f.name, f.got, f.want)
		}
	}
}

// fiatSeriesGet fetches a fiat-quoted daily series over June 2024 and
// decodes the envelope the pool tests read.
func fiatSeriesGet(t *testing.T, ts *testServer, base string) fiatSeriesEnvelope {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+base+"&quote=fiat:USD&interval=1d&from=2024-06-01T00:00:00Z&to=2024-07-01T00:00:00Z")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env fiatSeriesEnvelope
	mustDecode(t, resp, &env)
	return env
}

// assertSACQuotedSeriesReadLast is the series twin of the ordering
// [assertNoSACQuotedRead] pins on the point side. The fiat combine DOES read a declared peg's SAC wrapper — that is
// where a Soroban pool's USD leg lives — but it must read every established spelling
// of every family FIRST, because a held-back bar is admitted only into a
// bucket none of them answered. Interleaving the two would let one
// family's thin pool be consulted before another family's deep book.
func assertSACQuotedSeriesReadLast(t *testing.T, reads []string) {
	t.Helper()
	firstSAC := -1
	for i, raw := range reads {
		p, err := canonical.ParsePair(raw)
		if err != nil {
			t.Fatalf("ParsePair(%q): %v", raw, err)
		}
		if p.Quote.Type == canonical.AssetSoroban {
			if firstSAC < 0 {
				firstSAC = i
			}
			continue
		}
		if firstSAC >= 0 {
			t.Errorf("%s (an established spelling) read at %d, after a SAC-quoted one at %d — "+
				"every established spelling of every family must be read before any held-back "+
				"form (reads=%v)", raw, i, firstSAC, reads)
		}
	}
}

// coverageMarketKey is an orientation-free literal pair key: the
// probe's SQL and the series read both fold the two stored directions,
// so A/B and B/A are one market to either.
func coverageMarketKey(a, b canonical.Asset) string {
	x, y := a.String(), b.String()
	if x > y {
		x, y = y, x
	}
	return x + "/" + y
}

func coverageKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// coverageSetDiff returns the keys of a that b lacks, sorted.
func coverageSetDiff(a, b map[string]bool) []string {
	out := []string{}
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestCoverageFloor_UnfinishedProbeIsNotMemoised — a probe that did not
// finish established nothing about the pair. Memoised as failed, it
// would answer every request for the pair with silence for the TTL:
// thirty minutes of no signal because one client hung up, or because
// one response's probe ceiling ran out. Only a probe the database
// answered is memoised — an error there is a fact about the read, and
// re-issuing it on every empty window is the cost the memo exists to
// bound.
func TestCoverageFloor_UnfinishedProbeIsNotMemoised(t *testing.T) {
	// A stablecoin-quoted pair: one probe per empty answer.
	const pairQS = "base=crypto:XLM&quote=" + usdcClassicID
	cases := []struct {
		name       string
		err        error
		wantCalls  int
		wantSignal bool
	}{
		{"caller went away", context.Canceled, 2, true},
		{"deadline ran out", context.DeadlineExceeded, 2, true},
		{"database answered with an error", errors.New("prices_1d unavailable"), 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &coverageFloorProbe{err: tc.err}
			ts := ohlcCoverageServer(t, probe)

			first := ohlcCoverageGetPair(t, ts, pairQS, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")
			assertCoverage(t, first.coverageMeta, nil, false)

			probe.heal(xlmCoverageFloor)
			second := ohlcCoverageGetPair(t, ts, pairQS, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")
			if calls, _, _, _ := probe.snapshot(); calls != tc.wantCalls {
				t.Errorf("probe calls = %d across two requests, want %d", calls, tc.wantCalls)
			}
			if tc.wantSignal {
				assertCoverage(t, second.coverageMeta, &xlmCoverageFloor, true)
			} else {
				assertCoverage(t, second.coverageMeta, nil, false)
			}
		})
	}
}

// TestPriceAt_ProxyDeviationBand: a triangulated fiat:USD answer served
// through a declared peg flags proxy_deviation only when the peg's own
// observed dollar price is more than 2% from $1.
func TestPriceAt_ProxyDeviationBand(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name, usdcUSD string
		want          bool
	}{
		{"depegged below", "0.95", true},
		{"depegged above", "1.03", true},
		{"inside band", "1.019", false},
		{"exactly at band", "0.98", false},
		{"no observation", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := depegServer(t, tc.usdcUSD)
			env := getPegEnvelope(t, base+"/v1/price/at?asset=native&quote=fiat:USD&ts="+at)
			if !env.Flags.Triangulated {
				t.Fatal("flags.triangulated = false, want true (served through the peg)")
			}
			if env.Flags.ProxyDeviation != tc.want {
				t.Errorf("flags.proxy_deviation = %v, want %v", env.Flags.ProxyDeviation, tc.want)
			}
		})
	}
}

// A declared peg that did not serve the answer still trips the flag: the
// proxy cannot say which peg a triangulated figure leans on, so any
// off-band declared peg is reported.
func TestPriceAt_ProxyDeviationAnyDeclaredPeg(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	srv := v1.New(v1.Options{
		PriceAt: &recordingPriceAtReader{byPair: map[string]string{
			depegXLMUSDCPair:        "0.10",
			depegUSDCUSDPair:        "1.00",
			"crypto:PYUSD/fiat:USD": "0.90",
		}},
		USDPeggedClassics: []canonical.Asset{usdc, pyusd},
	})
	base := startHTTPTest(t, srv.Handler()).URL
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	env := getPegEnvelope(t, base+"/v1/price/at?asset=native&quote=fiat:USD&ts="+at)
	if !env.Flags.ProxyDeviation {
		t.Error("flags.proxy_deviation = false, want true (PYUSD observed at 0.90)")
	}
}

func TestPriceAt_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			byPair:   map[string]string{flaggedAsset + "/fiat:USD": "41.32"},
			bucketAt: ts.Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/at?asset="+flaggedAsset+"&quote=fiat:USD&ts="+ts.Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// RAW 41.32 × K(=100) = 4132; serving the raw "41.32" would be wrong.
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/price/at not normalized (want 4132.0000000000): %s", body)
	}
}

func TestPriceAt_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			byPair:   map[string]string{"native/fiat:USD": "0.1242"},
			bucketAt: ts.Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/at?asset=native&quote=fiat:USD&ts="+ts.Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1242"`) {
		t.Errorf("7dp /v1/price/at must be byte-identical: %s", body)
	}
}

// TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn: the self-pair
// guard on /v1/price/at folds through the alias registry and skips only
// THAT peg; a second declared peg is still walked.
func TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	base, reader := pegPriceAtServer(t)
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	env := getPegEnvelope(t, base+"/v1/price/at?asset="+pegAliasUSDCSAC+"&quote=fiat:USD&ts="+at.Format(time.RFC3339))
	if env.Data.Price != "1.0004" {
		t.Errorf("price = %q, want 1.0004 — the second declared peg must still be walked", env.Data.Price)
	}
	if env.Data.AssetID != pegAliasUSDCSAC || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", env.Data.AssetID, env.Data.Quote, pegAliasUSDCSAC)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true — served through a peg, not the requested quote")
	}
	assertSecondPegWalked(t, reader)
}

// m2PriceAtStub implements v1.PriceAtReader keyed on "<base>/<quote>". When
// `historical` is set it returns `current` for a near-now ts and `historical`
// for an older ts, so /v1/price/changes horizons see a non-trivial delta.
type m2PriceAtStub struct {
	byPair     map[string]string
	current    string
	historical string
	histPair   string
	bucketAt   time.Time
}

func (s m2PriceAtStub) PriceAt(_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	if s.histPair != "" && key == s.histPair {
		if time.Since(ts) < 30*time.Minute {
			return s.current, s.bucketAt, 60, nil
		}
		return s.historical, ts, 60, nil
	}
	if v, ok := s.byPair[key]; ok {
		return v, s.bucketAt, 60, nil
	}
	return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
}

// m2Trade builds a trade for the given pair with raw smallest-unit amounts.
func m2Trade(t *testing.T, base, quote, txHash string, baseAmt, quoteAmt *big.Int) canonical.Trade {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset base: %v", err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset quote: %v", err)
	}
	pair, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source: "aquarius", Ledger: 1, TxHash: txHash,
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmt),
		QuoteAmount: canonical.NewAmount(quoteAmt),
	}
}
