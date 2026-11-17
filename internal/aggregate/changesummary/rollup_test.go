package changesummary

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestComputeSummary_DeltasAndATH walks computeSummary against a
// hand-constructed series and checks the multi-window deltas, ATH/ATL,
// and current value all line up.
func TestComputeSummary_DeltasAndATH(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-30 * 24 * time.Hour), Value: "100.00"}, // 30 days ago
		{At: now.Add(-7 * 24 * time.Hour), Value: "110.00"},  // 7 days ago
		{At: now.Add(-24 * time.Hour), Value: "120.00"},      // 24h ago
		{At: now.Add(-1 * time.Hour), Value: "125.00"},       // 1h ago
		{At: now, Value: "130.00"},                           // current
	}
	row := computeSummary(Entity{Type: "coin", ID: "stellar"}, series, now)

	if row.CurrentValue != "130.00" {
		t.Errorf("CurrentValue = %v, want 130.00", row.CurrentValue)
	}
	// (130 - 125) / 125 * 100 = 4.0
	if row.H1DeltaPct == nil || *row.H1DeltaPct < 3.99 || *row.H1DeltaPct > 4.01 {
		t.Errorf("H1DeltaPct = %v, want ~4.0", row.H1DeltaPct)
	}
	// (130 - 120) / 120 * 100 ≈ 8.33
	if row.H24DeltaPct == nil || *row.H24DeltaPct < 8.32 || *row.H24DeltaPct > 8.34 {
		t.Errorf("H24DeltaPct = %v, want ~8.33", row.H24DeltaPct)
	}
	// (130 - 110) / 110 * 100 ≈ 18.18
	if row.D7DeltaPct == nil || *row.D7DeltaPct < 18.17 || *row.D7DeltaPct > 18.19 {
		t.Errorf("D7DeltaPct = %v, want ~18.18", row.D7DeltaPct)
	}
	// (130 - 100) / 100 * 100 = 30
	if row.D30DeltaPct == nil || *row.D30DeltaPct < 29.99 || *row.D30DeltaPct > 30.01 {
		t.Errorf("D30DeltaPct = %v, want 30", row.D30DeltaPct)
	}

	// 100, 110 and 120 have nothing in the 24h before them, so no baseline validated them for the ratchet.
	if row.ATHValue == nil || *row.ATHValue != "130.00" {
		t.Errorf("ATH = %v, want 130.00", row.ATHValue)
	}
	if row.ATLValue == nil || *row.ATLValue != "125.00" {
		t.Errorf("ATL = %v, want 125.00", row.ATLValue)
	}
}

// TestComputeSummary_SparseHistoryGracefulNullables — an entity
// with only a few hours of history must leave d7 / d30 unset rather
// than fabricating zeros.
func TestComputeSummary_SparseHistoryGracefulNullables(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-30 * time.Minute), Value: "100"},
		{At: now, Value: "105"},
	}
	row := computeSummary(Entity{Type: "coin", ID: "freshasset"}, series, now)

	if row.H1Value != nil {
		t.Errorf("H1Value = %v, want nil (no observation 1h ago)", row.H1Value)
	}
	if row.H24Value != nil {
		t.Errorf("H24Value = %v, want nil (entity is <1h old)", row.H24Value)
	}
	if row.D7Value != nil {
		t.Errorf("D7Value = %v, want nil", row.D7Value)
	}
	if row.D30Value != nil {
		t.Errorf("D30Value = %v, want nil", row.D30Value)
	}
}

// TestValueAt_BinarySearch — pin the at-or-before semantic.
func TestValueAt_BinarySearch(t *testing.T) {
	t0 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: t0.Add(0 * time.Hour), Value: "1"},
		{At: t0.Add(1 * time.Hour), Value: "2"},
		{At: t0.Add(2 * time.Hour), Value: "3"},
		{At: t0.Add(3 * time.Hour), Value: "4"},
	}
	for _, tc := range []struct {
		target time.Time
		want   string
		ok     bool
	}{
		{t0.Add(-1 * time.Hour), "", false}, // before any data
		{t0.Add(0 * time.Hour), "1", true},
		{t0.Add(90 * time.Minute), "2", true},
		{t0.Add(2 * time.Hour), "3", true},
		{t0.Add(10 * time.Hour), "4", true}, // after last
	} {
		got, ok := valueAt(series, tc.target, time.Time{})
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("valueAt(%v) = (%v, %v), want (%v, %v)", tc.target, got, ok, tc.want, tc.ok)
		}
	}
}

// TestValueAt_RejectsBaselineOlderThanNotBefore — the found observation
// must not predate notBefore, or it describes a different period.
func TestValueAt_RejectsBaselineOlderThanNotBefore(t *testing.T) {
	t0 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{{At: t0, Value: "1"}, {At: t0.Add(5 * time.Hour), Value: "2"}}
	if _, ok := valueAt(series, t0.Add(3*time.Hour), t0.Add(time.Hour)); ok {
		t.Error("valueAt accepted a baseline 3h before target with a 2h tolerance")
	}
	if got, ok := valueAt(series, t0.Add(3*time.Hour), t0); !ok || got != "1" {
		t.Errorf("valueAt at the tolerance edge = (%v, %v), want (1, true)", got, ok)
	}
}

// TestComputeSummary_DormantPairLeavesHorizonsNull: a pair whose newest observation is five days old has no price
// information in the 1h or 24h period, so those horizons must be NULL, not a 0%; the 7d horizon keeps its real delta.
func TestComputeSummary_DormantPairLeavesHorizonsNull(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-20 * 24 * time.Hour), Value: "100"},
		{At: now.Add(-8 * 24 * time.Hour), Value: "110"},
		{At: now.Add(-5 * 24 * time.Hour), Value: "121"},
	}
	row := computeSummary(Entity{Type: "pair", ID: "dormant"}, series, now)

	if row.H1Value != nil || row.H1DeltaPct != nil {
		t.Errorf("H1 = (%v, %v), want nil (no observation in the last hour)", row.H1Value, row.H1DeltaPct)
	}
	if row.H24Value != nil || row.H24DeltaPct != nil {
		t.Errorf("H24 = (%v, %v), want nil (no observation in the last 24h)", row.H24Value, row.H24DeltaPct)
	}
	// (121 - 110) / 110 * 100 = 10
	if row.D7DeltaPct == nil || *row.D7DeltaPct < 9.99 || *row.D7DeltaPct > 10.01 {
		t.Errorf("D7DeltaPct = %v, want 10", row.D7DeltaPct)
	}
	if row.D30Value != nil {
		t.Errorf("D30Value = %v, want nil (no observation 30d ago)", row.D30Value)
	}
	if row.CurrentValue != "121" {
		t.Errorf("CurrentValue = %v, want 121", row.CurrentValue)
	}
}

// TestComputeSummary_StaleBaselineLeavesHorizonNull — a live pair whose
// previous observation is three days old has no baseline for the 1h or
// 24h period; a 3-day move must not be published as a 1h or 24h delta.
func TestComputeSummary_StaleBaselineLeavesHorizonNull(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-3 * 24 * time.Hour), Value: "100"},
		{At: now, Value: "150"},
	}
	row := computeSummary(Entity{Type: "pair", ID: "thin"}, series, now)

	if row.H1DeltaPct != nil {
		t.Errorf("H1DeltaPct = %v, want nil (baseline is 3 days old)", *row.H1DeltaPct)
	}
	if row.H24DeltaPct != nil {
		t.Errorf("H24DeltaPct = %v, want nil (baseline is 3 days old)", *row.H24DeltaPct)
	}
}

// TestComputeStreak_Up — three consecutive daily increases → "up", 3. The first day has no baseline, so it does not count.
func TestComputeStreak_Up(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := make([]TimedValue, 5)
	for i := range series {
		series[i] = TimedValue{
			At:    now.Add(-time.Duration(4-i) * 24 * time.Hour),
			Value: strconv.Itoa(100 + i*10),
		}
	}
	dir, days := computeStreak(newGuarded(series))
	if dir != "up" || days == nil || *days != 3 {
		t.Errorf("got dir=%q days=%v, want up/3", dir, days)
	}
}

// TestComputeSummary_ATLSkipsZero — a zero (or unparseable) point
// mid-series must NOT corrupt ATL. A
// `|| atlValue == 0` reset turns [100,50,0,90] into ATL=90. (A 20x drop on a
// one-point history would be refused by the served-price guard instead.)
func TestComputeSummary_ATLSkipsZero(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-4 * time.Hour), Value: "100"}, // baseline for the next point
		{At: now.Add(-3 * time.Hour), Value: "100"},
		{At: now.Add(-2 * time.Hour), Value: "50"},
		{At: now.Add(-1 * time.Hour), Value: "0"}, // bad/zero point
		{At: now, Value: "90"},
	}
	row := computeSummary(Entity{Type: "coin", ID: "x"}, series, now)
	if row.ATLValue == nil || *row.ATLValue != "50" {
		t.Errorf("ATL = %v, want 50 (zero point must be skipped, not adopted)", row.ATLValue)
	}
	if row.ATHValue == nil || *row.ATHValue != "100" {
		t.Errorf("ATH = %v, want 100", row.ATHValue)
	}
}

// A single wild bucket must not become the published ATH/ATL: the sink
// ratchets them for good.
func TestComputeSummary_ExtremesIgnoreOutlierBuckets(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	// Five calm points give the served-price guard a full baseline, so 9000 does not taint 0.12's.
	series := []TimedValue{
		{At: now.Add(-9 * time.Hour), Value: "0.10"},
		{At: now.Add(-8 * time.Hour), Value: "0.10"},
		{At: now.Add(-7 * time.Hour), Value: "0.10"},
		{At: now.Add(-6 * time.Hour), Value: "0.10"},
		{At: now.Add(-4 * time.Hour), Value: "0.10"},
		{At: now.Add(-3 * time.Hour), Value: "9000.00"}, // bad bucket
		{At: now.Add(-2 * time.Hour), Value: "0.12"},
		{At: now.Add(-1 * time.Hour), Value: "0.00001"}, // bad bucket
		{At: now, Value: "0.11"},
	}
	row := computeSummary(Entity{Type: "coin", ID: "x"}, series, now)
	if row.ATHValue == nil || *row.ATHValue != "0.12" {
		t.Errorf("ATH = %v, want 0.12", row.ATHValue)
	}
	if row.ATLValue == nil || *row.ATLValue != "0.10" {
		t.Errorf("ATL = %v, want 0.10", row.ATLValue)
	}
}

// A wild newest bucket must not reach the ratchet: ATH/ATL stay NULL so the
// sink's GREATEST/LEAST keep the stored extremes.
func TestComputeSummary_WildCurrentLeavesExtremesNil(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	build := func(base, cur string) []TimedValue {
		var s []TimedValue
		for i := 20; i >= 1; i-- {
			s = append(s, TimedValue{At: now.Add(-time.Duration(i) * time.Minute), Value: base})
		}
		return append(s, TimedValue{At: now, Value: cur})
	}
	for _, c := range []struct{ base, cur string }{{"0.11", "9000"}, {"1", "0.0001"}} {
		row := computeSummary(Entity{Type: "coin", ID: "x"}, build(c.base, c.cur), now)
		// The served-price guard refuses the wild bucket, so the summary stands on the last bucket it accepts.
		if row.CurrentValue != c.base {
			t.Errorf("CurrentValue = %q, want %q", row.CurrentValue, c.base)
		}
		if row.ATHValue == nil || *row.ATHValue != c.base || row.ATLValue == nil || *row.ATLValue != c.base {
			t.Errorf("base %s cur %s: ATH/ATL = %v/%v, want both %s", c.base, c.cur, row.ATHValue, row.ATLValue, c.base)
		}
	}
}

// One dust print must not move a change the served price never showed, as the newest point or as a horizon anchor.
func TestComputeSummary_RefusedBucketIsNotAChange(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	flat := func(spikeAt time.Time) []TimedValue {
		var s []TimedValue
		for i := 180; i >= 0; i-- {
			at := now.Add(-time.Duration(i) * time.Minute)
			v := "0.11"
			if at.Equal(spikeAt) {
				v = "11.0"
			}
			s = append(s, TimedValue{At: at, Value: v})
		}
		return s
	}
	for name, spikeAt := range map[string]time.Time{
		"newest": now, "h1 anchor": now.Add(-time.Hour), "mid-series": now.Add(-90 * time.Minute),
	} {
		row := computeSummary(Entity{Type: "coin", ID: "x"}, flat(spikeAt), now)
		if row.CurrentValue != "0.11" {
			t.Errorf("%s: CurrentValue = %q, want 0.11", name, row.CurrentValue)
		}
		if row.H1DeltaPct == nil || *row.H1DeltaPct != 0 {
			t.Errorf("%s: H1DeltaPct = %v, want 0", name, row.H1DeltaPct)
		}
		// The sink ratchets ATH/ATL, so a refused print reaching them would outlive the window.
		if row.ATHValue == nil || *row.ATHValue != "0.11" || row.ATLValue == nil || *row.ATLValue != "0.11" {
			t.Errorf("%s: ATH/ATL = %v/%v, want 0.11/0.11", name, row.ATHValue, row.ATLValue)
		}
	}
}

// A refused print as the newest point or a day's close moves neither the streak nor the acceleration.
func TestComputeSummary_RefusedBucketMovesNoTrendFigure(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	var calm []TimedValue
	for i := 5 * 144; i >= 0; i-- {
		calm = append(calm, TimedValue{
			At:    now.Add(-time.Duration(i) * 10 * time.Minute),
			Value: strconv.FormatFloat(1-float64(5*144-i)*0.0001, 'f', 6, 64),
		})
	}
	want := computeSummary(Entity{Type: "coin", ID: "x"}, calm, now)
	if want.StreakDirection != "down" || want.Acceleration != "flat" {
		t.Fatalf("calm series: streak %q acceleration %q, want down/flat", want.StreakDirection, want.Acceleration)
	}
	dayClose := now.Truncate(24 * time.Hour).Add(-2*24*time.Hour - 10*time.Minute)
	for name, at := range map[string]time.Time{"newest": now, "day close": dayClose} {
		series := slices.Clone(calm)
		i := slices.IndexFunc(series, func(p TimedValue) bool { return p.At.Equal(at) })
		if i < 0 {
			t.Fatalf("%s: no point at %v", name, at)
		}
		series[i].Value = "100"
		got := computeSummary(Entity{Type: "coin", ID: "x"}, series, now)
		if got.StreakDirection != want.StreakDirection || *got.StreakDays != *want.StreakDays || got.Acceleration != want.Acceleration {
			t.Errorf("%s: streak %s/%d acceleration %q, want %s/%d %q", name,
				got.StreakDirection, *got.StreakDays, got.Acceleration,
				want.StreakDirection, *want.StreakDays, want.Acceleration)
		}
	}
}

func TestComputeSummary_ExtremesKeepRealMovesAndShortSeries(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	// A 50x genuine rally stays inside the band.
	row := computeSummary(Entity{Type: "coin", ID: "x"}, []TimedValue{
		{At: now.Add(-26 * time.Hour), Value: "1"}, // baseline for the next point only
		{At: now.Add(-2 * time.Hour), Value: "1"},
		{At: now.Add(-1 * time.Hour), Value: "25"},
		{At: now, Value: "50"},
	}, now)
	if row.ATHValue == nil || row.ATLValue == nil || *row.ATHValue != "50" || *row.ATLValue != "1" {
		t.Errorf("ATH/ATL = %v/%v, want 50/1", row.ATHValue, row.ATLValue)
	}
	// Too short for the median band: /v1/price refuses a 100000x jump on a one-point history and serves 1,
	// which no baseline ever validated, so nothing reaches the ratchet.
	row = computeSummary(Entity{Type: "coin", ID: "x"}, []TimedValue{
		{At: now.Add(-1 * time.Hour), Value: "1"},
		{At: now, Value: "100000"},
	}, now)
	if row.CurrentValue != "1" || row.ATHValue != nil || row.ATLValue != nil {
		t.Errorf("current=%s ATH/ATL = %v/%v, want 1 and NULL/NULL", row.CurrentValue, row.ATHValue, row.ATLValue)
	}
}

// A fat-finger print after a quiet day is served fail-open (empty baseline) but must never ratchet into ATH/ATL,
// nor refuse the genuine buckets after it into a stand-in that does.
func TestComputeSummary_UnvalidatedPrintAfterGapNeverRatchets(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ calm, wild string }{{"1", "400"}, {"100000", "1"}, {"100000", "1000"}} {
		var s []TimedValue
		for i := 10; i >= 1; i-- {
			s = append(s, TimedValue{At: now.Add(-25*time.Hour - time.Duration(i)*time.Hour), Value: c.calm})
		}
		s = append(s, TimedValue{At: now.Add(-10 * time.Minute), Value: c.wild})
		for i := 9; i >= 0; i-- {
			s = append(s, TimedValue{At: now.Add(-time.Duration(i) * time.Minute), Value: c.calm})
		}
		row := computeSummary(Entity{Type: "coin", ID: "x"}, s, now)
		if row.CurrentValue != c.calm {
			t.Errorf("calm %s wild %s: current = %s, want %s", c.calm, c.wild, row.CurrentValue, c.calm)
		}
		if row.ATHValue == nil || *row.ATHValue != c.calm || row.ATLValue == nil || *row.ATLValue != c.calm {
			t.Errorf("calm %s wild %s: ATH/ATL = %v/%v, want both %s", c.calm, c.wild, row.ATHValue, row.ATLValue, c.calm)
		}
	}
}

// TestComputeSummary_PreservesFullPrecision: a price with more significant digits than float64 carries must round-trip
// byte-for-byte into CurrentValue/ATHValue/ATLValue; a ParseFloat round-trip loses everything past the 17th digit.
func TestComputeSummary_PreservesFullPrecision(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	const precise = "0.104735294117647058823529412"
	series := []TimedValue{
		{At: now.Add(-time.Hour), Value: "0.05"},
		{At: now, Value: precise},
	}
	row := computeSummary(Entity{Type: "coin", ID: "precise"}, series, now)

	if row.CurrentValue != precise {
		t.Errorf("CurrentValue = %q, want %q (exact digits lost)", row.CurrentValue, precise)
	}
	if row.ATHValue == nil {
		t.Fatalf("ATHValue = nil, want %q", precise)
	}
	if *row.ATHValue != precise {
		t.Errorf("ATHValue = %q, want %q (exact digits lost)", *row.ATHValue, precise)
	}
}

// TestComputeAcceleration — direction-agnostic momentum. A steepening
// downtrend must read "increasing" (momentum building), not "decreasing".
// Regression: a signed comparison inverts negative deltas.
func TestComputeAcceleration(t *testing.T) {
	base := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	// build constructs a series from per-step values starting at v0.
	build := func(v0 float64, steps ...float64) []TimedValue {
		vals := []float64{v0}
		for _, s := range steps {
			vals = append(vals, vals[len(vals)-1]+s)
		}
		out := make([]TimedValue, len(vals))
		for i, v := range vals {
			out[i] = TimedValue{
				At:    base.Add(time.Duration(i) * time.Minute),
				Value: strconv.FormatFloat(v, 'f', -1, 64),
			}
		}
		return out
	}

	// computeAcceleration compares the last quarter's mean step with the previous quarter's; the cases vary the LAST step's magnitude against an earlier one.
	// Key regression: a steepening DOWNtrend (more-negative last step) must read increasing, not decreasing.
	for _, tc := range []struct {
		name   string
		series []TimedValue
		want   string
	}{
		{
			// Last step (+10) much bigger than the earlier baseline (+1).
			name:   "steepening uptrend → increasing",
			series: build(100, 1, 1, 1, 1, 1, 1, 1, 10),
			want:   "increasing",
		},
		{
			// Last step (-10) much more negative than the earlier (-1):
			// the signed-comparison bug labelled this "decreasing".
			name:   "steepening downtrend → increasing",
			series: build(100, -1, -1, -1, -1, -1, -1, -1, -10),
			want:   "increasing",
		},
		{
			// Earlier step (-10) bigger than the last (-1): momentum waning.
			name:   "softening downtrend → decreasing",
			series: build(100, -1, -1, -1, -1, -1, -10, -1, -1),
			want:   "decreasing",
		},
		{
			// Constant slope — neither accelerating nor decelerating.
			name:   "steady uptrend → flat",
			series: build(100, 5, 5, 5, 5, 5, 5, 5, 5),
			want:   "flat",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeAcceleration(newGuarded(tc.series)); got != tc.want {
				t.Errorf("computeAcceleration = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtremes_BeyondFloat64NotDropped(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	series := []TimedValue{
		{At: now.Add(-3 * time.Hour), Value: "1e400"}, // baseline for the next point
		{At: now.Add(-2 * time.Hour), Value: "1e400"},
		{At: now.Add(-1 * time.Hour), Value: "3e400"},
		{At: now, Value: "2e400"},
	}
	ath, _, atl, _, ok := extremes(newGuarded(series), series[3])
	if !ok || ath != "3e400" || atl != "1e400" {
		t.Errorf("ath=%q atl=%q ok=%v, want 3e400/1e400/true", ath, atl, ok)
	}
}

func TestExtremes_BandEdgesInclusive(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	vals := []string{"1", "1", "1", "1000", "0.001", "1001", "0.00099"}
	var series []TimedValue
	for i, v := range vals {
		series = append(series, TimedValue{At: now.Add(time.Duration(i-len(vals)) * time.Hour), Value: v})
	}
	cur := TimedValue{At: now, Value: "1"}
	series = append(series, cur)
	// The served-price guard would refuse 1000 after a run of 1s; accept every point to test the band alone.
	g := newGuarded(series)
	for i := range g.verdict {
		g.verdict[i] = 1
	}
	ath, _, atl, _, ok := extremes(g, cur)
	if !ok || ath != "1000" || atl != "0.001" {
		t.Errorf("ath=%q atl=%q ok=%v, want 1000/0.001/true", ath, atl, ok)
	}
}

// Zero and negative points never become ATL even when the guard accepts every point.
func TestExtremes_NonPositiveNeverATL(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	for _, vals := range [][]string{{"0", "3"}, {"-5", "3"}, {"0", "-5", "3"}, {"3", "0", "-5", "3"}} {
		var series []TimedValue
		for i, v := range vals {
			series = append(series, TimedValue{At: now.Add(time.Duration(i-len(vals)+1) * time.Hour), Value: v})
		}
		g := newGuarded(series)
		for i := range g.verdict {
			g.verdict[i] = 1
		}
		ath, _, atl, _, ok := extremes(g, series[len(series)-1])
		if !ok || ath != "3" || atl != "3" {
			t.Errorf("%v: ath=%q atl=%q ok=%v, want 3/3/true", vals, ath, atl, ok)
		}
	}
}

type pairSource map[string][]TimedValue

func (s pairSource) TimedVWAPs1m(_ context.Context, p canonical.Pair, _, _ time.Time) ([]TimedValue, error) {
	v, ok := s[p.String()]
	if !ok {
		return nil, errors.New("source down")
	}
	return v, nil
}

// TestRefresh_PublishesPassOutcomeAndHeartbeat: a total outage must be
// visible as a `failed` pass that leaves the heartbeat alone; any upsert
// advances it.
func TestRefresh_PublishesPassOutcomeAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	good := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"}, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}
	bad := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}
	src := pairSource{good.String(): {{At: now.Add(-time.Minute), Value: "0.1"}}}

	counter := func(outcome string) float64 {
		return testutil.ToFloat64(obs.ChangeSummaryPassesTotal.WithLabelValues(outcome))
	}
	run := func(ents ...Entity) {
		w, err := New(src, &recordingSink{}, ents, slog.New(slog.DiscardHandler), Options{Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		w.refresh(context.Background())
	}
	goodEnt := Entity{Type: "coin", ID: good.Base.String(), Pair: good}
	badEnt := Entity{Type: "coin", ID: bad.Base.String(), Pair: bad}

	obs.ChangeSummaryLastSuccessUnix.Set(1)
	beforeFailed := counter("failed")
	run(badEnt)
	if got := counter("failed") - beforeFailed; got != 1 {
		t.Fatalf("failed passes +%v, want +1", got)
	}
	if got := testutil.ToFloat64(obs.ChangeSummaryLastSuccessUnix); got != 1 {
		t.Fatalf("heartbeat moved to %v on a total outage", got)
	}

	beforePartial := counter("partial")
	run(goodEnt, badEnt)
	if got := counter("partial") - beforePartial; got != 1 {
		t.Fatalf("partial passes +%v, want +1", got)
	}
	if got := testutil.ToFloat64(obs.ChangeSummaryLastSuccessUnix); got != float64(now.Unix()) {
		t.Fatalf("heartbeat = %v, want %d after a partial pass", got, now.Unix())
	}

	beforeOK := counter("ok")
	run(goodEnt)
	if got := counter("ok") - beforeOK; got != 1 {
		t.Fatalf("ok passes +%v, want +1", got)
	}
}

// TestRefresh_ReportsFailuresOncePerPassAtWarn: a per-entity Debug line hid
// a quarter of the working set failing every pass on r1.
func TestRefresh_ReportsFailuresOncePerPassAtWarn(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 30, 0, time.UTC)
	var buf bytes.Buffer
	ents := []Entity{{Type: "coin", ID: "a"}, {Type: "pair", ID: "a/b"}}
	w, err := New(fixedSource(nil), &recordingSink{}, ents,
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.refresh(context.Background())
	out := buf.String()
	if n := strings.Count(out, "change-summary pass had failures"); n != 1 {
		t.Fatalf("got %d Warn lines, want 1: %s", n, out)
	}
	if !strings.Contains(out, "failed=2") || !strings.Contains(out, "total=2") {
		t.Errorf("Warn line lacks the failure count: %s", out)
	}
}
