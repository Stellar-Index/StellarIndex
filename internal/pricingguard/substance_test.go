package pricingguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func mustAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

func testPolicy() SubstancePolicy {
	return SubstancePolicy{
		MinVolumeUSD: new(big.Rat).SetInt64(1000),
		MinBuckets:   20,
		MinSpan:      6 * time.Hour,
		Window:       24 * time.Hour,
	}
}

func TestSubstanceOK_Floors(t *testing.T) {
	pol := testPolicy()
	ok := func(vol int64, buckets, span int64) bool {
		return SubstanceOK(new(big.Rat).SetInt64(vol), buckets, span, pol)
	}
	if !ok(1000, 20, 6*3600) {
		t.Error("exactly-at-floor must pass")
	}
	if ok(999, 20, 6*3600) {
		t.Error("below volume floor must fail")
	}
	if ok(1000, 19, 6*3600) {
		t.Error("below bucket floor must fail")
	}
	if ok(1000, 20, 6*3600-1) {
		t.Error("below span floor must fail")
	}
	// The incident shape: one $8.57 seed bucket + one dump
	// bucket. Massively below every floor.
	if ok(9, 2, 21*60) {
		t.Error("incident-shaped market must fail")
	}
	if SubstanceOK(nil, 100, 24*3600, pol) {
		t.Error("nil volume must count as zero (fail-closed)")
	}
}

func TestSubstanceGated_Applicability(t *testing.T) {
	classic := mustAsset(t, "USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	fiatUSD := mustAsset(t, "fiat:USD")
	fiatEUR := mustAsset(t, "fiat:EUR")
	cryptoBTC := mustAsset(t, "crypto:BTC")

	cases := []struct {
		name        string
		base, quote canonical.Asset
		want        bool
	}{
		{"classic/native (the attack class)", classic, native, true},
		{"native/fiat:USD (one on-chain leg)", native, fiatUSD, true},
		{"fiat/fiat cross", fiatEUR, fiatUSD, false},
		{"CEX crypto ticker vs fiat", cryptoBTC, fiatUSD, false},
		{"crypto vs crypto", cryptoBTC, mustAsset(t, "crypto:ETH"), false},
	}
	for _, tc := range cases {
		if got := SubstanceGated(tc.base, tc.quote); got != tc.want {
			t.Errorf("%s: SubstanceGated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeSubstanceReader returns the canned substance of the one pair key
// the requested spelling sets cover, and counts calls. Canned aggregates
// cannot be unioned (shared minutes are invisible in them), so a request
// covering two canned keys is an error; minuteStore models that case.
type fakeSubstanceReader struct {
	byPair map[string]timescale.MarketSubstance
	err    error
	calls  int
}

func (f *fakeSubstanceReader) PairMarketSubstance(
	_ context.Context, bases, quotes []canonical.Asset, _ time.Duration,
) (timescale.MarketSubstance, error) {
	f.calls++
	if f.err != nil {
		return timescale.MarketSubstance{}, f.err
	}
	var hits []timescale.MarketSubstance
	for _, b := range bases {
		for _, q := range quotes {
			pair, err := canonical.NewPair(b, q)
			if err != nil {
				continue
			}
			if sub, ok := f.byPair[pair.String()]; ok {
				hits = append(hits, sub)
			}
		}
	}
	switch len(hits) {
	case 0:
		return timescale.MarketSubstance{VolumeUSD: "0"}, nil
	case 1:
		return hits[0], nil
	default:
		return timescale.MarketSubstance{}, errors.New("fakeSubstanceReader: request spans several canned pairs")
	}
}

func (f *fakeSubstanceReader) PairMarketSubstanceAt(
	ctx context.Context, bases, quotes []canonical.Asset, _ time.Time, window time.Duration, _ timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	return f.PairMarketSubstance(ctx, bases, quotes, window)
}

func TestSubstanceGate_WithholdsThinPair(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("empty-substance on-chain pair must be withheld")
	}
}

func TestSubstanceGate_AllowsHealthyPair(t *testing.T) {
	classic := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	native := mustAsset(t, "native")
	pair, _ := canonical.NewPair(classic, native)
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{
		pair.String(): {VolumeUSD: "250000.5", Buckets: 900, SpanSeconds: 23 * 3600},
	}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("healthy pair must be allowed")
	}
}

func TestSubstanceGate_AliasUnion(t *testing.T) {
	// The pair as requested: native/fiat:USD — zero rows by
	// construction. The alias family (crypto:XLM/fiat:USD) carries the
	// CEX volume. The gate must sum across the union and allow.
	native := mustAsset(t, "native")
	fiatUSD := mustAsset(t, "fiat:USD")
	cexPair, _ := canonical.NewPair(mustAsset(t, "crypto:XLM"), fiatUSD)
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{
		cexPair.String(): {VolumeUSD: "9000000", Buckets: 1400, SpanSeconds: 24 * 3600},
	}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), native, fiatUSD, "test") {
		t.Fatal("native/fiat:USD must be allowed via the crypto:XLM alias union")
	}
}

func TestSubstanceGate_CachesVerdict(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	gate.Allowed(context.Background(), classic, native, "test")
	after := reader.calls
	if after == 0 {
		t.Fatal("first verdict must measure")
	}
	gate.Allowed(context.Background(), classic, native, "test")
	// Direction-insensitive cache: the flipped orientation shares it.
	gate.Allowed(context.Background(), native, classic, "test")
	if reader.calls != after {
		t.Errorf("cached verdict re-measured: %d calls after, %d now", after, reader.calls)
	}
}

func TestSubstanceGate_FailsOpenOnStoreError(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{err: errors.New("db down")}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("store error must fail open (a DB blip must not 404 the price surface)")
	}
	// And the error verdict must NOT be cached — recovery is immediate.
	reader.err = nil
	before := reader.calls
	gate.Allowed(context.Background(), classic, native, "test")
	if reader.calls == before {
		t.Error("error verdict was cached; next request must re-measure")
	}
}

// TestSubstanceGate_VerdictReportsUnmeasured pins the gate half: a
// store error is reported as NO verdict (allowed=false, measured=false)
// to callers that must not publish an unverified price, and is counted,
// while Allowed keeps its single-lookup fail-open.
func TestSubstanceGate_VerdictReportsUnmeasured(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{err: context.DeadlineExceeded}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})
	counter := obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues("verdict_test")
	before := testutil.ToFloat64(counter)

	allowed, measured := gate.Verdict(context.Background(), classic, native, "verdict_test")
	if allowed || measured {
		t.Fatalf("Verdict on store error = (allowed=%v, measured=%v), want (false, false)", allowed, measured)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("unmeasured counter moved by %v, want 1", got)
	}
	if !gate.Allowed(context.Background(), classic, native, "verdict_test") {
		t.Error("Allowed must still fail open on a store error")
	}

	reader.err = nil
	allowed, measured = gate.Verdict(context.Background(), classic, native, "verdict_test")
	if allowed || !measured {
		t.Errorf("Verdict on a thin pair = (allowed=%v, measured=%v), want (false, true)", allowed, measured)
	}
}

func TestSubstanceGate_NilGateAllows(t *testing.T) {
	var gate *SubstanceGate
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if !gate.Allowed(context.Background(), classic, mustAsset(t, "native"), "test") {
		t.Fatal("nil gate must allow everything")
	}
}

func TestSubstanceGate_OffChainPairSkipsMeasurement(t *testing.T) {
	reader := &fakeSubstanceReader{}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})
	if !gate.Allowed(context.Background(), mustAsset(t, "fiat:EUR"), mustAsset(t, "fiat:USD"), "test") {
		t.Fatal("fiat cross must be exempt")
	}
	if reader.calls != 0 {
		t.Errorf("exempt pair must not hit the store, got %d calls", reader.calls)
	}
}

func TestSubstancePolicyFromValues(t *testing.T) {
	pol := SubstancePolicyFromValues(2500, 30, 120, 48)
	if pol.MinVolumeUSD.Cmp(new(big.Rat).SetInt64(2500)) != 0 {
		t.Errorf("MinVolumeUSD = %v", pol.MinVolumeUSD)
	}
	if pol.MinBuckets != 30 || pol.MinSpan != 2*time.Hour || pol.Window != 48*time.Hour {
		t.Errorf("policy = %+v", pol)
	}
	// Zeros defer to package defaults at construction time.
	gate := NewSubstanceGate(&fakeSubstanceReader{}, SubstanceGateOptions{Policy: SubstancePolicyFromValues(0, 0, 0, 0)})
	if gate.policy.MinVolumeUSD.Cmp(new(big.Rat).SetInt64(DefaultSubstanceMinVolumeUSD)) != 0 {
		t.Errorf("default MinVolumeUSD = %v", gate.policy.MinVolumeUSD)
	}
	if gate.policy.MinBuckets != DefaultSubstanceMinBuckets || gate.policy.MinSpan != DefaultSubstanceMinSpan || gate.policy.Window != DefaultSubstanceWindow {
		t.Errorf("defaults = %+v", gate.policy)
	}
}

// TestSubstancePolicyFromValues_OverflowFallsBackToDefault: a window or
// span too large for time.Duration must not wrap negative (a negative
// window fails the gate open for every pair); it resolves to the default.
func TestSubstancePolicyFromValues_OverflowFallsBackToDefault(t *testing.T) {
	gate := NewSubstanceGate(&fakeSubstanceReader{}, SubstanceGateOptions{
		Policy: SubstancePolicyFromValues(0, 0, math.MaxInt64/int(time.Minute)+1, 3_000_000),
	})
	if gate.policy.Window != DefaultSubstanceWindow {
		t.Errorf("window_hours=3,000,000: Window = %v, want default %v", gate.policy.Window, DefaultSubstanceWindow)
	}
	if gate.policy.MinSpan != DefaultSubstanceMinSpan {
		t.Errorf("overflowing span: MinSpan = %v, want default %v", gate.policy.MinSpan, DefaultSubstanceMinSpan)
	}
	if got := SubstancePolicyFromValues(0, 0, 0, 2_562_047).Window; got != 2_562_047*time.Hour {
		t.Errorf("largest representable window_hours: Window = %v, want %v", got, 2_562_047*time.Hour)
	}
}

// countingHandler counts slog records at Warn+ so the transition-only
// logging contract is testable.
type countingHandler struct{ warns *int }

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		*h.warns++
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

// TestSubstanceGate_LogsOnTransitionsOnly — the steady state
// (hundreds of thin pairs re-measured every TTL expiry) produced
// 6,000 WARNs/hour on r1. The metric carries the volume;
// the log carries only verdict CHANGES.
func TestSubstanceGate_LogsOnTransitionsOnly(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	warns := 0
	gate := NewSubstanceGate(reader, SubstanceGateOptions{
		Policy: testPolicy(),
		Logger: slog.New(countingHandler{warns: &warns}),
	})
	now := time.Unix(1_700_000_000, 0)
	gate.now = func() time.Time { return now }

	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 1 {
		t.Fatalf("first withheld observation must WARN once, got %d", warns)
	}
	// TTL expiry → re-measure, still withheld → NO new warn.
	now = now.Add(substanceCacheTTL + time.Second)
	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 1 {
		t.Errorf("repeat withheld verdict must not re-WARN, got %d", warns)
	}
	// Pair recovers → next re-measure logs no warn either (Info only).
	pair, _ := canonical.NewPair(classic, native)
	reader.byPair[pair.String()] = timescale.MarketSubstance{
		VolumeUSD: "50000", Buckets: 500, SpanSeconds: 23 * 3600,
	}
	now = now.Add(substanceCacheTTL + time.Second)
	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("recovered pair must be allowed")
	}
	if warns != 1 {
		t.Errorf("recovery must not WARN, got %d", warns)
	}
	// And a flip BACK to withheld warns again.
	delete(reader.byPair, pair.String())
	now = now.Add(substanceCacheTTL + time.Second)
	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 2 {
		t.Errorf("allowed→withheld flip must WARN, got %d", warns)
	}
}

// A store that answers only the live question must not be accepted by
// the constructor: the gate would otherwise judge history by today's
// market, and would do so without failing or logging.
func TestNewSubstanceGate_RequiresPointInTimeReader(t *testing.T) {
	param := reflect.TypeOf(NewSubstanceGate).In(0)
	atReader := reflect.TypeFor[MarketSubstanceAtReader]()
	if !param.Implements(atReader) {
		t.Fatalf("NewSubstanceGate accepts %v, which does not require %v", param, atReader)
	}
}

// substanceLegs counts a market given as trade minutes (offsets from a
// UTC hour boundary) at the named grain, as PairMarketSubstanceAt does:
// distinct buckets, and max(bucket) - min(bucket) in seconds.
func substanceLegs(minutes []int64, grain time.Duration) (buckets, spanSeconds int64) {
	seen := map[int64]bool{}
	var lo, hi int64
	for i, m := range minutes {
		k := int64((time.Duration(m) * time.Minute).Truncate(grain) / time.Second)
		seen[k] = true
		if i == 0 || k < lo {
			lo = k
		}
		if i == 0 || k > hi {
			hi = k
		}
	}
	return int64(len(seen)), hi - lo
}

// substanceLayouts is the weakest-shaped market for the policy (packed
// minutes, the last pushed out to the span floor) plus every union of
// up to two runs of consecutive minutes drawn from a grid of hours,
// in-hour offsets and lengths.
func substanceLayouts(pol SubstancePolicy) [][]int64 {
	packed := make([]int64, 0, pol.MinBuckets)
	for m := range pol.MinBuckets - 1 {
		packed = append(packed, m)
	}
	packed = append(packed, max(pol.MinBuckets-1, int64(pol.MinSpan/time.Minute)))
	var runs [][]int64
	for _, hour := range []int64{0, 1, 6} {
		for _, start := range []int64{0, 5, 30, 59} {
			for _, n := range []int64{1, 10, 20, 31, 60, 120} {
				run := make([]int64, 0, n)
				for m := range n {
					run = append(run, hour*60+start+m)
				}
				runs = append(runs, run)
			}
		}
	}
	out := [][]int64{packed}
	for i, a := range runs {
		out = append(out, a)
		for _, b := range runs[i+1:] {
			u := slices.Concat(a, b)
			slices.Sort(u)
			out = append(out, slices.Compact(u))
		}
	}
	return out
}

// The historical verdict must never refuse what the live verdict admits
// under ANY legal policy, not just the default one; the hour floor is
// derived from the policy and is the strictest floor with that property.
func TestSubstanceGate_HourGrainFloorAgreesWithMinuteFloorForAnyPolicy(t *testing.T) {
	past := timescale.PriceAtMinuteRungMaxAge + time.Hour
	vol := new(big.Rat).SetInt64(5000)
	cases := []struct {
		minBuckets  int64
		minSpan     time.Duration
		wantBuckets int64
		wantSpan    time.Duration
	}{
		{20, 6 * time.Hour, 2, 6 * time.Hour},
		{20, 30 * time.Minute, 1, 0}, // twenty minutes inside one hour clear the live floor
		{20, 90 * time.Minute, 2, time.Hour},
		{90, 45 * time.Minute, 2, 0},
		{200, 6 * time.Hour, 4, 6 * time.Hour},
		{1, time.Minute, 1, 0},
	}
	for _, tc := range cases {
		pol := testPolicy()
		pol.MinBuckets, pol.MinSpan = tc.minBuckets, tc.minSpan
		gate := NewSubstanceGate(&fakeSubstanceReader{}, SubstanceGateOptions{Policy: pol})
		grain, hourly := gate.policyAt(past)
		if grain != timescale.Granularity1h {
			t.Fatalf("policyAt(%v) grain = %s, want %s", past, grain, timescale.Granularity1h)
		}
		if hourly.MinBuckets != tc.wantBuckets || hourly.MinSpan != tc.wantSpan {
			t.Errorf("policy %d buckets / %v: hour floor = %d buckets / %v, want %d / %v",
				tc.minBuckets, tc.minSpan, hourly.MinBuckets, hourly.MinSpan, tc.wantBuckets, tc.wantSpan)
		}
		admitted := 0
		for _, layout := range substanceLayouts(pol) {
			mb, ms := substanceLegs(layout, time.Minute)
			if !SubstanceOK(vol, mb, ms, pol) {
				continue
			}
			admitted++
			if hb, hs := substanceLegs(layout, time.Hour); !SubstanceOK(vol, hb, hs, hourly) {
				t.Errorf("policy %d buckets / %v: live floor admits %d buckets over %ds, hour floor refuses the same market (%d buckets over %ds)",
					tc.minBuckets, tc.minSpan, mb, ms, hb, hs)
				break
			}
		}
		if admitted == 0 {
			t.Fatalf("policy %d buckets / %v: no layout clears the live floor — the property was not exercised", tc.minBuckets, tc.minSpan)
		}
	}
}

// TestNewSubstanceGate_LogsTheEffectivePolicy: r1 sets no
// substance keys, so it runs at library defaults no config file shows.
// The gate states the floors it resolved at construction, so a boot log
// answers "what floor is this deployment enforcing" without a code read.
func TestNewSubstanceGate_LogsTheEffectivePolicy(t *testing.T) {
	var buf bytes.Buffer
	NewSubstanceGate(nil, SubstanceGateOptions{
		Policy: SubstancePolicyFromValues(0, 0, 0, 0),
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
	})
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("want one JSON log record, got %q: %v", buf.String(), err)
	}
	want := map[string]any{
		"msg":            "substance gate armed",
		"min_volume_usd": "1000",
		"min_buckets":    float64(DefaultSubstanceMinBuckets),
		"min_span":       DefaultSubstanceMinSpan.String(),
		"window":         DefaultSubstanceWindow.String(),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("boot log %s = %v, want %v (record %s)", k, rec[k], v, buf.String())
		}
	}
}

// The dangerous direction: thick TODAY, attacker-seeded dust at the
// requested instant. The old gate asked about today, passed, and the
// point-in-time read served the manipulated historical price.
func TestSubstanceGate_AllowedAt_WithholdsInstantThatWasDustThoughMarketIsThickToday(t *testing.T) {
	base, quote := scamPair(t)
	seeded := time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at:   func(time.Time) timescale.MarketSubstance { return dustSubstance },
	}
	gate := newTimedGate(r)

	if !gate.Allowed(context.Background(), base, quote, "test") {
		t.Fatal("fixture: the live market must clear the floor, or this test proves nothing about time")
	}
	if gate.AllowedAt(context.Background(), base, quote, seeded, "test") {
		t.Fatal("AllowedAt served an instant whose own market was one $8.57 bucket, because " +
			"the market is thick TODAY — the historical read would publish the seeded price")
	}
}

// The harmful direction: deep and honest at the requested instant,
// dormant today. The old gate withheld every historical price we hold.
func TestSubstanceGate_AllowedAt_ServesInstantThatWasDeepThoughMarketIsDormantToday(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{
		live: timescale.MarketSubstance{VolumeUSD: "0"},
		at:   func(time.Time) timescale.MarketSubstance { return thickSubstance },
	}
	gate := newTimedGate(r)

	if gate.Allowed(context.Background(), base, quote, "test") {
		t.Fatal("fixture: the live market must be below the floor")
	}
	if !gate.AllowedAt(context.Background(), base, quote, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "test") {
		t.Fatal("AllowedAt withheld an instant whose own market was deep, because the market " +
			"is dormant TODAY — a cost-basis read 404s for data we hold and trust")
	}
}

// The measurement must END at the requested instant, at the grain the
// point-in-time reader can serve that instant from.
func TestSubstanceGate_AllowedAt_MeasuresTheWindowEndingAtTheInstant(t *testing.T) {
	base, quote := scamPair(t)
	cases := []struct {
		name      string
		at        time.Time
		wantAsOf  time.Time
		wantGrain timescale.HistoryGranularity
	}{
		{
			name:      "inside the minute rung: minute grain, truncated to the minute",
			at:        gateNow.Add(-time.Hour),
			wantAsOf:  time.Date(2026, 9, 18, 11, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
		{
			name:      "exactly at the minute-rung boundary: still minute grain",
			at:        gateNow.Add(-timescale.PriceAtMinuteRungMaxAge),
			wantAsOf:  time.Date(2026, 9, 16, 12, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
		{
			name:      "past the minute rung: hour grain, truncated to the hour",
			at:        time.Date(2024, 6, 1, 15, 42, 10, 0, time.UTC),
			wantAsOf:  time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1h,
		},
		{
			name:      "a future instant is measured as of now, never past it",
			at:        gateNow.Add(72 * time.Hour),
			wantAsOf:  time.Date(2026, 9, 18, 12, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return thickSubstance }}
			gate := newTimedGate(r)
			gate.AllowedAt(context.Background(), base, quote, tc.at, "test")

			if r.liveCalls != 0 {
				t.Errorf("made %d trailing-from-now measurement(s) for a point-in-time verdict", r.liveCalls)
			}
			if len(r.atCalls) == 0 {
				t.Fatal("no point-in-time measurement was made")
			}
			for _, c := range r.atCalls {
				if !c.asOf.Equal(tc.wantAsOf) {
					t.Errorf("measured as of %s, want %s", c.asOf, tc.wantAsOf)
				}
				if c.grain != tc.wantGrain {
					t.Errorf("measured at grain %q, want %q", c.grain, tc.wantGrain)
				}
				if c.window != testPolicy().withDefaults().Window {
					t.Errorf("measured a %s window, want the policy's %s", c.window, testPolicy().withDefaults().Window)
				}
			}
		})
	}
}

// The hour floor must admit exactly the weakest market the minute floor
// admits — 20 minutes across a 6h span can be as few as two hour
// buckets — and must still refuse a single burst.
func TestSubstanceGate_AllowedAt_HourGrainFloor(t *testing.T) {
	base, quote := scamPair(t)
	old := time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		sub  timescale.MarketSubstance
		want bool
	}{
		{"two hour buckets six hours apart, over the volume floor", timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 6 * 3600}, true},
		{"one hour bucket — a single burst, whatever its size", timescale.MarketSubstance{VolumeUSD: "9000000", Buckets: 1, SpanSeconds: 0}, false},
		{"two adjacent hours — span leg unchanged at hour grain", timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 3600}, false},
		{"spread out but under the volume floor — volume leg unchanged", timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 12, SpanSeconds: 20 * 3600}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return tc.sub }}
			if got := newTimedGate(r).AllowedAt(context.Background(), base, quote, old, "test"); got != tc.want {
				t.Errorf("AllowedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// Inside the minute rung the floor is the LIVE floor, unweakened: two
// buckets that would clear the hour floor must not clear this one, or
// /v1/price/at?ts=<a minute ago> republishes what /v1/price refuses.
func TestSubstanceGate_AllowedAt_RecentInstantKeepsTheMinuteFloor(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance {
		return timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 6 * 3600}
	}}
	if newTimedGate(r).AllowedAt(context.Background(), base, quote, gateNow.Add(-5*time.Minute), "test") {
		t.Fatal("a recent instant was held to the hour-grain bucket floor — the minute rung " +
			"serves the raw 1m bucket and must keep the live gate's floor")
	}
}

// Verdicts for different instants must not share a cache slot, and must
// not share one with the live verdict.
func TestSubstanceGate_AllowedAt_CachesPerInstant(t *testing.T) {
	base, quote := scamPair(t)
	dustDay := time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC)
	deepDay := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at: func(asOf time.Time) timescale.MarketSubstance {
			if asOf.Equal(dustDay) {
				return dustSubstance
			}
			return thickSubstance
		},
	}
	gate := newTimedGate(r)
	ctx := context.Background()

	gate.Allowed(ctx, base, quote, "test")
	if gate.AllowedAt(ctx, base, quote, dustDay, "test") {
		t.Error("dust instant inherited the live verdict")
	}
	if !gate.AllowedAt(ctx, base, quote, deepDay, "test") {
		t.Error("deep instant inherited the dust instant's verdict")
	}
	before := len(r.atCalls)
	gate.AllowedAt(ctx, base, quote, dustDay.Add(20*time.Minute), "test") // same hour → same verdict
	if len(r.atCalls) != before {
		t.Errorf("an instant in an already-measured hour re-queried the store (%d → %d calls)", before, len(r.atCalls))
	}
	if !gate.Allowed(ctx, base, quote, "test") {
		t.Error("a withheld point-in-time verdict leaked into the live verdict")
	}
}

// Same asymmetric posture as the live gate: a store error serves, and
// is not cached.
func TestSubstanceGate_AllowedAt_FailsOpenOnStoreErrorAndDoesNotCache(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{atErr: errors.New("connection reset")}
	gate := newTimedGate(r)
	at := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)

	if !gate.AllowedAt(context.Background(), base, quote, at, "test") {
		t.Fatal("a store error must fail open — a DB blip must not 404 the price surface")
	}
	r.atErr = nil
	r.at = func(time.Time) timescale.MarketSubstance { return dustSubstance }
	if gate.AllowedAt(context.Background(), base, quote, at, "test") {
		t.Fatal("the fail-open answer was cached: a dust instant stayed served after the store recovered")
	}
}

func TestSubstanceGate_AllowedAt_NilGateAndUngatedPairAllow(t *testing.T) {
	var gate *SubstanceGate
	base, quote := scamPair(t)
	if !gate.AllowedAt(context.Background(), base, quote, gateNow, "test") {
		t.Error("nil gate must allow — a disabled [pricing_guard] must not withhold")
	}
	r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return dustSubstance }}
	if !newTimedGate(r).AllowedAt(context.Background(), mustAsset(t, "fiat:EUR"), mustAsset(t, "fiat:USD"), gateNow, "test") {
		t.Error("an off-chain pair is out of the gate's scope at any instant")
	}
	if len(r.atCalls) != 0 {
		t.Error("an ungated pair was measured")
	}
}

// TestSubstanceGate_WithheldCountCarriesTheFloor: the counter says which
// floor refused the pair, from a fresh measurement and from the cache.
func TestSubstanceGate_WithheldCountCarriesTheFloor(t *testing.T) {
	const surface = "floor_label_test"
	asset := mustAsset(t, "SHRT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	pair, err := canonical.NewPair(asset, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	short := timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 40, SpanSeconds: 3600, ValuedBuckets: 40}
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): short}},
		SubstanceGateOptions{Policy: testPolicy()})
	counter := obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface, string(FloorSpan))
	for range 2 {
		if gate.Allowed(context.Background(), asset, canonical.NativeAsset(), surface) {
			t.Fatal("a one-hour market cleared the six-hour span floor")
		}
	}
	var pb dto.Metric
	if err := counter.Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 2 {
		t.Errorf("floor=%q series = %v, want 2", FloorSpan, got)
	}
	if got := withheldFor(t, surface); got != 2 {
		t.Errorf("withheld counted %v across all floors, want 2", got)
	}
}

// TestSubstanceGate_UnvaluedMarketIsWithheldAndNamed: a market
// with real persistence but no USD valuation — the SEP-41/SEP-41 shape,
// 800 buckets over 22h — stays withheld (an unvaluable volume cannot be
// verified, and waiving the floor would admit the self-minted pair the
// gate exists for), but under its own floor, not as a thin market.
func TestSubstanceGate_UnvaluedMarketIsWithheldAndNamed(t *testing.T) {
	a := mustAsset(t, "TOKA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	b := mustAsset(t, "TOKB-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	pair, err := canonical.NewPair(a, b)
	if err != nil {
		t.Fatal(err)
	}
	unvalued := timescale.MarketSubstance{VolumeUSD: "0", Buckets: 800, SpanSeconds: 22 * 3600}
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): unvalued}},
		SubstanceGateOptions{Policy: testPolicy()})
	allowed, measured, floor := gate.Probe(context.Background(), a, b)
	if allowed || !measured || floor != FloorVolumeUnvalued {
		t.Errorf("Probe = (allowed %v, measured %v, floor %q), want (false, true, %q)",
			allowed, measured, floor, FloorVolumeUnvalued)
	}
}

// minuteRow is one prices_1m row: a closed minute of one stored spelling.
type minuteRow struct {
	base, quote string
	bucket      time.Time
	usd         int64
}

// minuteStore models prices_1m as rows and answers the substance question
// the way the SQL does: distinct buckets, summed volume and span over
// every row the request selects, in both stored orientations. It is a
// model of the table, not a canned answer, so it can only agree with a
// gate that asks for the union in one read.
type minuteStore struct{ rows []minuteRow }

func (m *minuteStore) measure(match func(base, quote string) bool) timescale.MarketSubstance {
	vol := new(big.Rat)
	seen := map[time.Time]bool{}
	var lo, hi time.Time
	for _, r := range m.rows {
		if !match(r.base, r.quote) && !match(r.quote, r.base) {
			continue
		}
		vol.Add(vol, new(big.Rat).SetInt64(r.usd))
		seen[r.bucket] = true
		if lo.IsZero() || r.bucket.Before(lo) {
			lo = r.bucket
		}
		if r.bucket.After(hi) {
			hi = r.bucket
		}
	}
	return timescale.MarketSubstance{
		VolumeUSD:   vol.FloatString(0),
		Buckets:     int64(len(seen)),
		SpanSeconds: int64(hi.Sub(lo) / time.Second),
	}
}

func (m *minuteStore) PairMarketSubstance(
	_ context.Context, bases, quotes []canonical.Asset, _ time.Duration,
) (timescale.MarketSubstance, error) {
	in := func(set []canonical.Asset, s string) bool {
		for _, a := range set {
			if a.String() == s {
				return true
			}
		}
		return false
	}
	return m.measure(func(b, q string) bool { return in(bases, b) && in(quotes, q) }), nil
}

// PairMarketSubstanceAt satisfies [SubstanceStore]; these fixtures only
// exercise the live path ([SubstanceGate.Verdict]), so it ignores the
// point-in-time parameters and answers the same union as the live read.
func (m *minuteStore) PairMarketSubstanceAt(
	ctx context.Context, bases, quotes []canonical.Asset, _ time.Time, window time.Duration, _ timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	return m.PairMarketSubstance(ctx, bases, quotes, window)
}

// XLM's SDEX leg (native) and CEX leg (crypto:XLM) trading in the SAME ten
// minutes are ten minutes of market, not twenty. Counting each spelling's
// distinct minutes and adding them let a market clear the 20-minute floor
// on half the persistence it demands.
func TestSubstanceGate_AliasUnionCountsSharedMinutesOnce(t *testing.T) {
	usdc := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := &minuteStore{}
	for i := 0; i < 10; i++ {
		minute := start.Add(time.Duration(i) * 47 * time.Minute) // spans 7h03m
		for _, spelling := range []string{"native", "crypto:XLM"} {
			store.rows = append(store.rows, minuteRow{spelling, usdc.String(), minute, 5000})
		}
	}
	gate := NewSubstanceGate(store, SubstanceGateOptions{Policy: testPolicy()})

	allowed, measured := gate.Verdict(context.Background(), canonical.NativeAsset(), usdc, "test")
	if !measured {
		t.Fatal("verdict unmeasured")
	}
	if allowed {
		t.Fatal("10 distinct minutes quoted under two XLM spellings cleared a 20-distinct-minute floor")
	}
}

// The span leg is the union's wall-clock reach too: one spelling trading
// early in the window and another late is one market active across the
// whole stretch, not two short-lived ones.
func TestSubstanceGate_AliasUnionSpanCoversEverySpelling(t *testing.T) {
	usdc := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := &minuteStore{}
	for i := 0; i < 15; i++ {
		early := start.Add(time.Duration(i) * 12 * time.Minute)          // 0h00 – 2h48
		late := start.Add(4*time.Hour + time.Duration(i)*12*time.Minute) // 4h00 – 6h48
		store.rows = append(store.rows,
			minuteRow{"native", usdc.String(), early, 5000},
			minuteRow{"crypto:XLM", usdc.String(), late, 5000})
	}
	gate := NewSubstanceGate(store, SubstanceGateOptions{Policy: testPolicy()})

	allowed, measured := gate.Verdict(context.Background(), canonical.NativeAsset(), usdc, "test")
	if !measured || !allowed {
		t.Fatalf("30 distinct minutes spanning 6h48m across two spellings: allowed=%v measured=%v, want served",
			allowed, measured)
	}
}
