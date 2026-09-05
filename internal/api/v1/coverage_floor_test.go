// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
// beside price_declared_peg_test.go's fixtures).

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
// daily aggregate, measured on r1 2026-09-03. The fixtures below are
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

// TestOHLCSeries_BelowCoverageFloorIsFlagged is the defect itself.
//
// Pre-fix, GET /v1/ohlc?base=crypto:XLM&quote=fiat:USD&interval=1d
// &from=2016-01-01&to=2016-03-01 answered `{"intervals":[]}` with
// `flags.stale:false` and nothing else — byte-identical to the answer
// for a window the pair traded through quietly. The pair's daily
// candles begin 2018-07-01; 2016 is two and a half years below that
// floor, and the response said so nowhere.
func TestOHLCSeries_BelowCoverageFloorIsFlagged(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)

	env := ohlcCoverageGet(t, ts, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")
	if !env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = false for a window entirely below the %s floor",
			xlmCoverageFloor.Format(time.RFC3339))
	}
	if env.CoverageFrom == nil {
		t.Fatalf("coverage_from absent; want %s", xlmCoverageFloor.Format(time.RFC3339))
	}
	if !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %s, want %s", env.CoverageFrom, xlmCoverageFloor)
	}
}

// TestOHLCSeries_StraddlingWindowIsNotFlagged — a window that CONTAINS
// the floor contains covered time, so its emptiness is a genuine
// market answer for that part. Flagging it would be the same lie in
// the opposite direction: "never held" about a period that was.
func TestOHLCSeries_StraddlingWindowIsNotFlagged(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)

	env := ohlcCoverageGet(t, ts, "2018-01-01T00:00:00Z", "2019-01-01T00:00:00Z")
	if env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = true for a window straddling the floor")
	}
	if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want the floor echoed even when the flag is off", env.CoverageFrom)
	}
}

// TestOHLCSeries_QuietInCoverageWindowIsNotFlagged — the real hole in
// this pair's daily candles runs 2021-02 → 2026-03 (measured on r1),
// entirely ABOVE the floor. That window is covered and empty, which is
// exactly what "quiet" means; only `coverage_from` should appear.
func TestOHLCSeries_QuietInCoverageWindowIsNotFlagged(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)

	env := ohlcCoverageGet(t, ts, "2023-01-01T00:00:00Z", "2023-02-01T00:00:00Z")
	if env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = true for a quiet window inside coverage")
	}
	if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want the floor", env.CoverageFrom)
	}
}

// TestOHLCSeries_ProbeErrorYieldsNoSignal — a failed probe must leave
// the response exactly as it is today: empty series, no annotation, no
// flag. The one-way rule. Staying silent costs a caller the advisory;
// guessing would tell them their window predates the held history on
// the strength of a database hiccup.
func TestOHLCSeries_ProbeErrorYieldsNoSignal(t *testing.T) {
	probe := &coverageFloorProbe{err: errors.New("prices_1d unavailable")}
	ts := ohlcCoverageServer(t, probe)

	env := ohlcCoverageGet(t, ts, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")
	if env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = true off a FAILED probe — a false coverage claim")
	}
	if env.CoverageFrom != nil {
		t.Errorf("coverage_from = %v off a failed probe, want absent", env.CoverageFrom)
	}
}

// TestOHLCSeries_UnknownFloorYieldsNoSignal — a probe that reached the
// database and found no daily bucket is NOT proof the pair has no
// history: a pair whose first trades landed an hour ago has no closed
// daily bucket yet. Absence stays unknown.
func TestOHLCSeries_UnknownFloorYieldsNoSignal(t *testing.T) {
	probe := &coverageFloorProbe{found: false}
	ts := ohlcCoverageServer(t, probe)

	env := ohlcCoverageGet(t, ts, "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z")
	if env.Flags.OutsideCoverage || env.CoverageFrom != nil {
		t.Errorf("no-floor probe produced a signal: outside=%v from=%v",
			env.Flags.OutsideCoverage, env.CoverageFrom)
	}
}

// TestCoverageFloorProbe_IsBoundedAndDaily pins the read the handler
// actually issues for a pair served from itself: one probe, on the
// DAILY rung, over a window bounded at both ends and strictly
// increasing.
//
// The rung is not the requested interval on purpose. No price aggregate
// carries a retention policy (migration 0031 removed the ones migration
// 0002 had placed on prices_1m / prices_15m), so nothing in the daily
// rung has been dropped by age; it is the coarsest, holds the fewest
// rows per pair, and is the cheapest to prove empty — so a handler that
// probed at the requested grain would spend more to describe a rung
// whose contents follow its own refresh schedule.
//
// The fixture is a stablecoin-quoted pair, which the series read serves
// from the pair itself; a fiat-quoted pair is served from a constituent
// set and probes each constituent (see
// TestCoverageFloor_FiatQuoteProbesEachConstituentOnce).
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

// TestCoverageFloor_ProbedOncePerPairAcrossAliases — the memo, and the
// single alias-family resolution behind it. `native` and `crypto:XLM`
// are the same asset; the probe reads every form of both legs in one
// query, so the second and third requests must cost NOTHING. Without
// the fold this endpoint would issue a read per identity spelling per
// request, which is the cost profile an anonymous caller can multiply
// at will.
func TestCoverageFloor_ProbedOncePerPairAcrossAliases(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)

	window := "&interval=1d&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z"
	var afterFirst int
	for i, q := range []string{
		"/v1/ohlc?base=crypto:XLM&quote=" + usdcClassicID + window,
		"/v1/ohlc?base=native&quote=" + usdcClassicID + window,
		"/v1/ohlc?base=crypto:XLM&quote=" + usdcClassicID + window,
	} {
		if resp := mustGet(t, ts.URL+q); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", q, resp.StatusCode)
		}
		if i == 0 {
			afterFirst, _, _, _ = probe.snapshot()
		}
	}
	if afterFirst != 1 {
		t.Errorf("probe calls = %d after the first request, want 1", afterFirst)
	}
	if calls, _, _, _ := probe.snapshot(); calls != afterFirst {
		t.Errorf("probe calls = %d across three requests for two alias spellings of one pair, want the first request's %d", calls, afterFirst)
	}
}

// TestCoverageFloor_FiatQuoteProbesEachConstituentOnce — a fiat-quoted
// pair is served from a constituent set, so its floor costs one probe
// per constituent FAMILY on the first empty answer (the base's alias
// spellings fold onto one key each) and nothing on a repeat, under any
// spelling. This is the cost bound of the fiat case: bounded by the
// operator's peg list plus the fixed stablecoin backers, never by the
// caller.
func TestCoverageFloor_FiatQuoteProbesEachConstituentOnce(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := ohlcCoverageServer(t, probe)

	window := "&interval=1d&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z"
	if resp := mustGet(t, ts.URL+"/v1/ohlc?base=crypto:XLM&quote=fiat:USD"+window); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	first, _, _, _ := probe.snapshot()
	// The direct pair plus the USD stablecoin backers the aggregator
	// expands to (no classic pegs are configured on this server) — one
	// probe each, no probe per alias spelling of the base.
	want := 1 + len(aggregateUSDBackers())
	if first != want {
		t.Fatalf("probe calls = %d on the first fiat-quoted empty answer, want %d (one per constituent family)", first, want)
	}
	for _, q := range []string{
		"/v1/ohlc?base=native&quote=fiat:USD" + window,
		"/v1/ohlc?base=crypto:XLM&quote=fiat:USD" + window,
	} {
		if resp := mustGet(t, ts.URL+q); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", q, resp.StatusCode)
		}
	}
	if calls, _, _, _ := probe.snapshot(); calls != first {
		t.Errorf("probe calls = %d after two repeats under both spellings, want %d — the memo must absorb them", calls, first)
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

// TestHistory_BelowCoverageFloorIsFlagged — /v1/history returns raw
// trades, not buckets, and its empty page carries the identical
// ambiguity. The annotation rides on the ENVELOPE precisely so this
// surface, whose `data` is a bare array, can carry it too.
func TestHistory_BelowCoverageFloorIsFlagged(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{},
		CoverageFloor: probe,
	}))

	resp := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, resp, &env)
	if !env.Flags.OutsideCoverage {
		t.Errorf("flags.outside_coverage = false on an empty below-floor trade window")
	}
	if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Errorf("coverage_from = %v, want %s", env.CoverageFrom, xlmCoverageFloor)
	}
}

// TestHistory_DrainedCursorPageIsNotProbed — an empty page reached by
// PAGINATION means "you have all the rows", not "there is nothing
// here", and the cursor shadows `from` so the window the signal would
// describe is not the window that was read. Neither probe nor flag.
func TestHistory_DrainedCursorPageIsNotProbed(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{},
		CoverageFloor: probe,
	}))

	// One full page first, to obtain a server-minted cursor.
	first := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z&limit=1")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", first.StatusCode)
	}
	if calls, _, _, _ := probe.snapshot(); calls != 1 {
		t.Fatalf("setup: probe calls = %d on the first empty page, want 1", calls)
	}

	second := mustGet(t, ts.URL+"/v1/history?base=crypto:XLM&quote=fiat:USD"+
		"&from=2016-01-01T00:00:00Z&to=2016-03-01T00:00:00Z&cursor="+drainedHistoryCursor)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("cursor page status = %d, want 200", second.StatusCode)
	}
	var env coverageMeta
	mustDecode(t, second, &env)
	if env.Flags.OutsideCoverage || env.CoverageFrom != nil {
		t.Errorf("drained cursor page carried a coverage signal: outside=%v from=%v",
			env.Flags.OutsideCoverage, env.CoverageFrom)
	}
	if calls, _, _, _ := probe.snapshot(); calls != 1 {
		t.Errorf("probe calls = %d, want the cursor page to add none", calls)
	}
}

// TestChart_EmptySeriesCarriesCoverageFrom — /v1/chart's window always
// ends at now, so it can never sit BELOW the floor; the floor itself is
// the answer there. An empty 24h chart with `coverage_from: 2018-07-01`
// says "quiet"; the same chart with the field absent says "nothing is
// held for this pair".
func TestChart_EmptySeriesCarriesCoverageFrom(t *testing.T) {
	probe := &coverageFloorProbe{floor: xlmCoverageFloor, found: true}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{},
		CoverageFloor: probe,
	}))

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

// TestOHLCSeries_FiatQuoteFloorIsTheConstituentSet pins the fold on
// /v1/ohlc for AQUA/fiat:USD, whose combined series draws on AQUA/USDC
// among others. Each case names the window under test and what the
// literal-pair probe would have said instead.
func TestOHLCSeries_FiatQuoteFloorIsTheConstituentSet(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	usd := mustParseAsset(t, "fiat:USD")
	const pairQS = "base=" + aquaClassicID + "&quote=fiat:USD"

	cases := []struct {
		name        string
		byPair      map[string]time.Time
		failPairs   map[string]bool
		from, to    string
		wantFrom    *time.Time
		wantOutside bool
	}{
		{
			// The direct pair's buckets begin 2024 (a late CEX feed); the
			// USDC constituent's begin 2021. A 2022 window is inside the
			// served history — the literal-pair probe called it uncovered.
			name: "earlier constituent lifts the floor",
			byPair: map[string]time.Time{
				probeKey(aqua, usd):  directFloor2024,
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: false,
		},
		{
			// The direct pair's buckets begin 2018, a constituent's 2021.
			// The set's floor is the EARLIEST — a later constituent never
			// drags it — so a 2019 window is quiet, not uncovered.
			name: "later constituent does not drag the floor",
			byPair: map[string]time.Time{
				probeKey(aqua, usd):  xlmCoverageFloor,
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: &xlmCoverageFloor, wantOutside: false,
		},
		{
			// No direct bucket at all; only the USDC constituent, from
			// 2021. The literal-pair probe found nothing and stayed
			// silent; the set says the 2019 window is below the floor.
			name: "constituent-only floor flags a window below it",
			byPair: map[string]time.Time{
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: true,
		},
		{
			name: "constituent-only floor reads a later window as quiet",
			byPair: map[string]time.Time{
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: false,
		},
		{
			name:   "no constituent holds a bucket",
			byPair: map[string]time.Time{},
			from:   "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: nil, wantOutside: false,
		},
		{
			// The direct pair answered 2024 but the USDC constituent's
			// probe FAILED. The constituent might hold the earliest
			// bucket, so a floor computed without it could be too late —
			// the set is unknown, and the response carries no claim.
			name: "a failed constituent silences the set",
			byPair: map[string]time.Time{
				probeKey(aqua, usd): directFloor2024,
			},
			failPairs: map[string]bool{probeKey(aqua, usdc): true},
			from:      "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: nil, wantOutside: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &coverageFloorProbe{byPair: tc.byPair, failPairs: tc.failPairs}
			ts := fiatCoverageServer(t, probe)
			env := ohlcCoverageGetPair(t, ts, pairQS, tc.from, tc.to)
			assertCoverage(t, env.coverageMeta, tc.wantFrom, tc.wantOutside)
		})
	}
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

// TestOHLCSeries_NativeUSDIsNotQuietSinceAFloorItIsNotServedFrom is the
// shape that motivated the constituent-set fold: `native/fiat:USD`
// holds no daily bucket under any of XLM's spellings, and its combined
// series is served from the USDC constituent, whose buckets begin 2021.
// A 2019 window on it is BELOW the served floor. It must not come back
// as a quiet window with a floor the served set does not hold — the
// only floor that can appear is the constituent's, and the window must
// be flagged against it.
func TestOHLCSeries_NativeUSDIsNotQuietSinceAFloorItIsNotServedFrom(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	native := canonical.NativeAsset()
	pegFloor := time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC)
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		// Only the peg constituent holds buckets; native/fiat:USD and
		// crypto:XLM/fiat:USD (one probe key) hold none.
		probeKey(native, usdc): pegFloor,
	}}
	ts := fiatCoverageServer(t, probe)

	env := ohlcCoverageGetPair(t, ts, "base=native&quote=fiat:USD", "2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z")
	if env.CoverageFrom != nil && env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Fatalf("coverage_from = %s — a floor the served constituent set does not hold", xlmCoverageFloor.Format(time.RFC3339))
	}
	assertCoverage(t, env.coverageMeta, &pegFloor, true)
	if calls, _, _, _ := probe.snapshot(); calls < 2 {
		t.Errorf("probe calls = %d, want the constituents probed, not the literal pair alone", calls)
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

// TestHistory_ReverseStoredMarketCarriesTheFloor pins that correspondence.
func TestHistory_ReverseStoredMarketCarriesTheFloor(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		// Stored as USDC/AQUA only.
		probeKey(usdc, aqua): pegFloor2021,
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:       &stubHistoryReader{},
		CoverageFloor: probe,
	}))
	const window = "&from=2019-01-01T00:00:00Z&to=2019-02-01T00:00:00Z"

	getHistory := func(t *testing.T, pairQS string) coverageMeta {
		t.Helper()
		resp := mustGet(t, ts.URL+"/v1/history?"+pairQS+window)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var env coverageMeta
		mustDecode(t, resp, &env)
		return env
	}

	t.Run("requested orientation is not the stored one: the floor", func(t *testing.T) {
		env := getHistory(t, "base="+aquaClassicID+"&quote="+usdcClassicID)
		assertCoverage(t, env, &pegFloor2021, true)
		if probe.stored() != 0 {
			t.Errorf("stored-orientation probes = %d, want 0 — /v1/history reads both directions, so it must not probe one", probe.stored())
		}
	})
	t.Run("requested orientation is the stored one: the same floor", func(t *testing.T) {
		env := getHistory(t, "base="+usdcClassicID+"&quote="+aquaClassicID)
		assertCoverage(t, env, &pegFloor2021, true)
		if probe.stored() != 0 {
			t.Errorf("stored-orientation probes = %d, want 0", probe.stored())
		}
	})
	t.Run("one probe spans every market the page read reaches", func(t *testing.T) {
		// A FRESH probe, so the span under test is the one read this
		// request made — a union across the two orientations above would
		// cover both markets even under a per-orientation probe, and
		// would assert nothing.
		fresh := &coverageFloorProbe{byPair: map[string]time.Time{probeKey(usdc, aqua): pegFloor2021}}
		freshTS := httpTestServer(t, v1.New(v1.Options{
			History:       &stubHistoryReader{},
			CoverageFloor: fresh,
		}))
		resp := mustGet(t, freshTS.URL+"/v1/history?base="+aquaClassicID+"&quote="+usdcClassicID+window)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var env coverageMeta
		mustDecode(t, resp, &env)

		reads := fresh.probedReads()
		if len(reads) != 1 {
			t.Fatalf("probes = %d, want 1 — the raw-trade page has one entry in its set", len(reads))
		}
		span := map[string]bool{}
		for _, k := range probeSpanKeys(reads[0].pair, reads[0].span) {
			span[k] = true
		}
		for _, want := range []string{probeKey(usdc, aqua), probeKey(aqua, usdc)} {
			if !span[want] {
				t.Errorf("%s is outside the span of the one probe (%s), but the page read reads it",
					want, reads[0].span)
			}
		}
	})
	t.Run("the series surface reports the same floor", func(t *testing.T) {
		env := ohlcCoverageGetPair(t, ts, "base="+aquaClassicID+"&quote="+usdcClassicID,
			"2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z")
		assertCoverage(t, env.coverageMeta, &pegFloor2021, true)
	})
}

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
// call the window quiet; the gap is recorded as launch-plan row 1.15
// instead of being papered over by a probe that spans more than the
// read.

// installUSDCSACRegistry publishes a registry declaring ONLY the USDC
// SAC wrapper, so the base under test keeps a single spelling and every
// second form in play is on the quote leg. Not parallel: the registry
// is process-global.
func installUSDCSACRegistry(t *testing.T) canonical.Asset {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{
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
	if !got.T.Equal(want.T) {
		t.Errorf("t = %s, want %s", got.T.Format(time.RFC3339), want.T.Format(time.RFC3339))
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
// [assertNoSACQuotedRead] pins on the point side. Since launch-plan row
// 1.15 the fiat combine DOES read a declared peg's SAC wrapper — that is
// where a Soroban pool's USD leg lives, and 43 assets on r1 have depth
// under no other spelling — but it must read every established spelling
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

// TestOHLCSeries_FiatQuoteBookOutranksSACQuotedPool — AQUA under r1's
// registry (the USDC and AQUA wrappers both declared): the book
// `AQUA/USDC-GA5Z…` holds one bar on day 1 (n=1, 100 base); the pool
// `<AQUA SAC>/<USDC SAC>` holds bars on day 1 (n=50, high 0.50, low
// 0.01) and on day 2.
//
// Served since launch-plan row 1.15: TWO bars. Day 1 is the book's
// alone — the pool is dropped from that bucket entirely, so its two
// prints at 0.50 and 0.01 cannot become the bar's high and low, which is
// what "outranks" means here — and day 2 is the pool's, because the
// book cannot answer it and the alternative is reporting a day the
// market traded as quiet.
//
// Before 1.15 the second bar was absent and this fixture pinned that:
// the pool was never read at all, so a bucket only it could answer was
// served as nothing. The guarantee that survived unchanged is the
// per-bucket one — see the day-1 assertions.
func TestOHLCSeries_FiatQuoteBookOutranksSACQuotedPool(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day1 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	book := mkSeriesBar(day1, "0.0041", "0.0042", "0.0040", "0.0041", "100", "0.41", 1)
	poolDay2 := mkSeriesBar(day2, "0.0035", "0.0036", "0.0034", "0.0035", "10", "0.035", 3)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		pegAliasAquaClassic + "/" + usdcClassicID: {book},
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			mkSeriesBar(day1, "0.0030", "0.5000", "0.0100", "0.0035", "20", "0.07", 50),
			poolDay2,
		},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, pegAliasAquaClassic)
	if len(env.Data.Intervals) != 2 {
		t.Fatalf("intervals = %d, want the book's day-1 bar and the pool's day-2 bar: %+v (reads=%v)",
			len(env.Data.Intervals), env.Data.Intervals, reader.ohlcPairs)
	}
	// Day 1: the book's own bar, print for print. The pool's 50 prints
	// and its 0.50/0.01 extremes are not blended in and not weighted
	// down — they are not in this bucket at all.
	assertBookBar(t, env.Data.Intervals[0], book)
	// Day 2: the pool's, where the book has nothing to say.
	assertBookBar(t, env.Data.Intervals[1], poolDay2)
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg")
	}
	assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
	// The population is the classic peg under every base spelling.
	for _, spelling := range []string{
		pegAliasAquaClassic + "/" + usdcClassicID,
		pegAliasAquaSAC + "/" + usdcClassicID,
	} {
		if callIndex(reader.ohlcPairs, spelling) < 0 {
			t.Errorf("%s never read (reads=%v)", spelling, reader.ohlcPairs)
		}
	}
}

// TestOHLCSeries_XLMBookOutranksSACQuotedPool — XLM under r1's
// registry: `native/USDC-GA5Z…` holds a bar of 100 trades over
// 6,000,000 units; `<XLM SAC>/<USDC SAC>` holds a two-trade bar with
// high 0.50 and low 0.01 in the same bucket.
//
// The served bucket carries n=100 and the book's own high 0.20 and low
// 0.18 under every XLM spelling of the request — the fiat combine folds
// the base spellings, so which one was named does not change the answer.
// The measured shape this pins out is n=102 with high 0.50 and low 0.01:
// two prints setting a bar's extremes beside six million units of book
// volume.
//
// Since launch-plan row 1.15 the SAC-quoted spelling IS read — that is
// how a bucket the book cannot answer gets served at all — so what holds
// the answer still is the per-bucket gate, not an absent read. The read
// order is asserted instead: every established spelling first.
func TestOHLCSeries_XLMBookOutranksSACQuotedPool(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	book := mkSeriesBar(day, "0.19", "0.20", "0.18", "0.195", "6000000", "1140000", 100)
	pool := mkSeriesBar(day, "0.19", "0.50", "0.01", "0.19", "20", "4", 2)
	for _, base := range []string{"native", "crypto:XLM", canonical.XLMSacContractID} {
		t.Run(base, func(t *testing.T) {
			reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
				"native/" + usdcClassicID:                          {book},
				canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {pool},
			}}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))
			env := fiatSeriesGet(t, ts, base)
			if len(env.Data.Intervals) != 1 {
				t.Fatalf("intervals = %d, want 1: %+v (reads=%v)", len(env.Data.Intervals), env.Data.Intervals, reader.ohlcPairs)
			}
			assertBookBar(t, env.Data.Intervals[0], book)
			assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
			if callIndex(reader.ohlcPairs, canonical.XLMSacContractID+"/"+pegAliasUSDCSAC) < 0 {
				t.Errorf("the pool was never read (reads=%v) — the held-back set must be read, "+
					"or a bucket only it can answer is served as quiet", reader.ohlcPairs)
			}
		})
	}
}

// TestOHLCSeries_SACQuotedOnlyDepthIsServed — the gap launch-plan row
// 1.15 closed, pinned from the other side.
//
// One market, AQUA quoted in the USDC SAC, with a daily bar inside the
// window; the declared peg is classic USDC. No established spelling
// holds a bucket, so every bucket is unanswered and the held-back
// spelling fills them: the series serves the pool's bar. This was
// `intervals: []` before, the state 43 assets on r1 were in.
//
// A populated answer carries no coverage annotation at all — the floor
// exists to explain an empty one — so the probe must not run here. The
// annotation that used to be pinned SILENT on an empty answer is the
// thing this replaces: the surface no longer has to describe a market it
// cannot serve, because it serves it.
func TestOHLCSeries_SACQuotedOnlyDepthIsServed(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	poolFloor := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	pool := mkSeriesBar(poolFloor, "0.0041", "0.0042", "0.0040", "0.0041", "1000", "4.1", 3)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		aquaClassicID + "/" + pegAliasUSDCSAC: {pool},
	}}
	probe := &coverageFloorProbe{byPair: map[string]time.Time{}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		CoverageFloor:     probe,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, aquaClassicID)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("intervals = %d, want the pool's bar (reads=%v)", len(env.Data.Intervals), reader.ohlcPairs)
	}
	assertBookBar(t, env.Data.Intervals[0], pool)
	assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
	if env.CoverageFrom != nil || env.Flags.OutsideCoverage {
		t.Errorf("populated answer carries coverage_from=%v outside_coverage=%v; the floor annotates empties only",
			env.CoverageFrom, env.Flags.OutsideCoverage)
	}
	if n := len(probe.probed()); n != 0 {
		t.Errorf("%d coverage probes issued for a populated series", n)
	}
}

// TestOHLCSeries_SACQuotedDepthOutsideTheWindowStillCarriesItsFloor —
// the annotation half of the same change. The pool's only bucket sits
// BEFORE the requested window, so the series is genuinely empty, and the
// floor must now name the pool's first bucket: the combine reads that
// market, so a floor measured over it is a claim this surface can keep.
//
// Before 1.15 naming it would have been wrong — the read could not serve
// the market the floor described, so `outside_coverage` had to stay
// silent rather than report "quiet" about a window the pool traded
// through. Widening the read is what makes the wider floor honest.
func TestOHLCSeries_SACQuotedDepthOutsideTheWindowStillCarriesItsFloor(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	poolFloor := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		aquaClassicID + "/" + pegAliasUSDCSAC: {
			mkSeriesBar(poolFloor, "0.0041", "0.0042", "0.0040", "0.0041", "1000", "4.1", 3),
		},
	}}
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		probeKey(aqua, usdcSAC): poolFloor,
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		CoverageFloor:     probe,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))
	const pairQS = "base=" + aquaClassicID + "&quote=fiat:USD"

	env := ohlcCoverageGetPair(t, ts, pairQS, "2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z")
	assertCoverage(t, env.coverageMeta, &poolFloor, true)
	if probe.literal() == 0 {
		t.Error("no quote-literal probe was issued; the fiat series must not be measured with the quote leg alias-folded")
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

// TestOHLCSeries_FiatProbeSpansWhatTheCombineReads pins the equality
// the annotation rests on, so the two sides cannot come apart by
// editing one: on an empty answer the set of literal pairs the combine
// REQUESTED must equal the set of markets the floor probes SPAN.
//
// A quote-literal probe spans its base leg's alias family crossed with
// the one quote spelling it was given, in both stored directions — so
// its span is computed here the way the SQL computes it, and the memo's
// collapse of repeated quote spellings is why the probe list is the
// shorter of the two. Pinned for a SAC-declared base and for XLM's
// three-form base, under the registry shape r1 runs.
//
// Since launch-plan row 1.15 the SAC-quoted market is on BOTH sides of
// the equality: the combine reads a declared peg's SAC wrapper, so the
// floor measures it. The equality is what keeps the two honest — a probe
// wider than the read reports a served-and-empty window as quiet, and a
// probe narrower than the read leaves a market it can serve unmeasured.
// An empty answer is the only one a floor annotates, and an empty answer
// is one where the held-back set was read too, so the set the probe must
// span is the whole constituent list.
func TestOHLCSeries_FiatProbeSpansWhatTheCombineReads(t *testing.T) {
	cases := []struct {
		name     string
		registry func(*testing.T) canonical.Asset
		base     string
	}{
		{"AQUA under the USDC wrapper alone", installUSDCSACRegistry, aquaClassicID},
		{"AQUA under r1's registry", installPegAliasRegistry, aquaClassicID},
		{"native under r1's registry", installPegAliasRegistry, "native"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usdc := tc.registry(t)
			if forms := canonical.AssetAliases(usdc); len(forms) < 2 {
				t.Fatalf("fixture puts no second quote spelling in play: %v", forms)
			}
			reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{}}
			probe := &coverageFloorProbe{byPair: map[string]time.Time{}}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				CoverageFloor:     probe,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))
			ohlcCoverageGetPair(t, ts, "base="+tc.base+"&quote=fiat:USD", "2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z")

			requested := map[string]bool{}
			for _, raw := range reader.ohlcPairs {
				p, err := canonical.ParsePair(raw)
				if err != nil {
					t.Fatalf("ParsePair(%q): %v", raw, err)
				}
				requested[coverageMarketKey(p.Base, p.Quote)] = true
			}
			span := map[string]bool{}
			for _, rd := range probe.probedReads() {
				for _, k := range probeSpanKeys(rd.pair, rd.span) {
					sp, err := canonical.ParsePair(k)
					if err != nil {
						continue // one asset against itself: NewPair rejects it, so the combine never requests it
					}
					span[coverageMarketKey(sp.Base, sp.Quote)] = true
				}
			}
			if len(span) == 0 {
				t.Fatalf("no probe spanned anything; reads=%v", reader.ohlcPairs)
			}
			if missing := coverageSetDiff(span, requested); len(missing) > 0 {
				t.Errorf("the floor spans markets the combine never requested: %v (span=%v)", missing, coverageKeys(span))
			}
			if extra := coverageSetDiff(requested, span); len(extra) > 0 {
				t.Errorf("the combine requested pairs the floor does not measure: %v", extra)
			}
			sacQuoted := coverageMarketKey(mustParseAsset(t, tc.base), mustParseAsset(t, pegAliasUSDCSAC))
			if !requested[sacQuoted] {
				t.Errorf("%s was not requested — a declared peg's SAC wrapper is where a Soroban pool's "+
					"USD leg lives, and an empty answer means every established spelling missed", sacQuoted)
			}
			if !span[sacQuoted] {
				t.Errorf("%s is not in the probed span — the combine reads it, so the floor must measure it "+
					"or an empty window it could serve carries no explanation", sacQuoted)
			}
		})
	}
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
