// Package changesummary computes the multi-window delta strip (h1/h24/d7/d30, ATH/ATL, streak, acceleration) every explorer entity renders.
// A periodic worker writes one change_summary_5m row per entity so views read in O(1) instead of N+1 prices_1m queries.
// See migrations/0022_create_change_summary_5m.up.sql and docs/architecture/explorer-data-inventory.md §6.1, §9.6.
package changesummary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Row is the storage-neutral summary the worker hands to the Sink.
type Row struct {
	EntityType   string
	EntityID     string
	RefreshedAt  time.Time
	CurrentValue string

	// Every *Value field is the RAW prices_1m ratio, NOT decimals-normalised: normalising here would change a row's scale mid-life and pin the GREATEST/LEAST ATH/ATL ratchet to the old scale.
	// Readers normalise (internal/api/v1 changeSummaryResponse); any new reader of change_summary_5m must too.
	H1Value     *string
	H1DeltaPct  *float64
	H24Value    *string
	H24DeltaPct *float64
	D7Value     *string
	D7DeltaPct  *float64
	D30Value    *string
	D30DeltaPct *float64

	ATHValue *string
	ATHAt    *time.Time
	ATLValue *string
	ATLAt    *time.Time

	StreakDirection string
	StreakDays      *int
	Acceleration    string
}

// PriceSource is the read seam: CLOSED buckets only (ADR-0015) over [from, to), oldest-first, stamped by bucket end.
type PriceSource interface {
	TimedVWAPs1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]TimedValue, error)
}

// TimedValue is one (timestamp, decimal-string value) point; parsed to float64 only for display-grade delta percentages.
type TimedValue struct {
	At    time.Time
	Value string
}

// Sink is the write seam where computed summaries land.
type Sink interface {
	UpsertChangeSummary(ctx context.Context, row Row) error
}

// Worker periodically refreshes change_summary_5m for its configured entities; stop it via the Run context.
type Worker struct {
	source   PriceSource
	sink     Sink
	logger   *slog.Logger
	interval time.Duration
	clock    func() time.Time

	// entities is the working set, each mapping an (entity_type, entity_id) to the canonical pair driving its deltas.
	// Entities with no single canonical pair (protocol, source) cannot be expressed, so those rows exist nowhere;
	// see allowedChangeSummaryEntityTypes in internal/api/v1/changes.go.
	entities []Entity
}

// Entity binds an (entity_type, entity_id) tuple to the pair whose 1m VWAP series drives its deltas.
type Entity struct {
	Type string         // 'coin' | 'pair'
	ID   string         // canonical id, e.g. "crypto:XLM" or "crypto:XLM/fiat:USD"
	Pair canonical.Pair // source of truth for prices_1m lookups
}

// Options tunes a Worker.
type Options struct {
	// Interval between refreshes. Default 5 min — matches the
	// change_summary_5m table name.
	Interval time.Duration

	// Clock injection for tests. Default time.Now.
	Clock func() time.Time
}

// New constructs a Worker, erroring when required fields are missing.
func New(source PriceSource, sink Sink, entities []Entity, logger *slog.Logger, opts Options) (*Worker, error) {
	if source == nil {
		return nil, errors.New("changesummary: PriceSource is required")
	}
	if sink == nil {
		return nil, errors.New("changesummary: Sink is required")
	}
	if logger == nil {
		return nil, errors.New("changesummary: logger is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	// Defensive copy so the caller's slice can't be mutated under us.
	cp := make([]Entity, len(entities))
	copy(cp, entities)
	return &Worker{
		source:   source,
		sink:     sink,
		logger:   logger,
		interval: interval,
		clock:    clock,
		entities: cp,
	}, nil
}

// Run refreshes every interval until ctx is cancelled and returns nil; per-entity failures are logged, never fatal.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()

	obs.ChangeSummaryLastSuccessUnix.Set(float64(w.clock().Unix()))

	// Refresh once immediately so a fresh boot doesn't wait a full
	// interval before the explorer has data.
	w.refresh(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.refresh(ctx)
		}
	}
}

// refresh runs one pass over every configured entity. Per-entity
// failures continue — a single broken pair must not block the rest
// of the working set — and are reported once per pass at Warn.
func (w *Worker) refresh(ctx context.Context) {
	now := w.clock().UTC()
	// Look back 30 days + a small buffer to capture the current
	// observation that anchors d30 deltas.
	from := now.Add(-30 * 24 * time.Hour).Add(-1 * time.Hour)

	var failed []string
	var firstErr error
	for _, ent := range w.entities {
		if err := w.refreshOne(ctx, ent, from, now); err != nil {
			failed = append(failed, ent.Type+"/"+ent.ID)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	recordPass(len(w.entities), len(failed), now)
	if len(failed) > 0 {
		w.logger.Warn("change-summary pass had failures",
			"failed", len(failed), "total", len(w.entities),
			"entities", failed, "first_err", firstErr)
	}
}

// recordPass publishes the pass outcome. The heartbeat advances on any
// upsert, so one permanently-empty pair neither masks nor fakes an outage.
func recordPass(total, failed int, now time.Time) {
	outcome := "ok"
	switch {
	case total > 0 && failed == total:
		outcome = "failed"
	case failed > 0:
		outcome = "partial"
	}
	obs.ChangeSummaryPassesTotal.WithLabelValues(outcome).Inc()
	if outcome != "failed" {
		obs.ChangeSummaryLastSuccessUnix.Set(float64(now.Unix()))
	}
}

func (w *Worker) refreshOne(ctx context.Context, ent Entity, from, now time.Time) error {
	series, err := w.source.TimedVWAPs1m(ctx, ent.Pair, from, now)
	if err != nil {
		return err
	}
	series = closedPoints(series, now)
	if len(series) == 0 {
		return errors.New("no closed observations in window")
	}
	// The newest point becomes current_value and seeds the ATH/ATL fold;
	// the upsert ratchets those with GREATEST/LEAST, so a 0 from an
	// unparseable value would pin atl_value to 0 permanently.
	newest := series[len(series)-1].Value
	if v, err := strconv.ParseFloat(newest, 64); err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fmt.Errorf("newest closed point %q is not a positive price", newest)
	}
	row := computeSummary(ent, series, now)
	return w.sink.UpsertChangeSummary(ctx, row)
}

// closedPoints drops any trailing point whose bucket ends after now: that
// bucket is still filling, and ADR-0015 never publishes it. The source
// already filters on the database clock; this holds the worker to its own.
func closedPoints(series []TimedValue, now time.Time) []TimedValue {
	end := len(series)
	for end > 0 && series[end-1].At.After(now) {
		end--
	}
	return series[:end]
}

// computeSummary derives the Row from an oldest-first series (len >= 1, enforced upstream);
// split out so it is testable without a fake PriceSource.
func computeSummary(ent Entity, series []TimedValue, now time.Time) Row {
	current := series[len(series)-1]

	row := Row{
		EntityType:   ent.Type,
		EntityID:     ent.ID,
		RefreshedAt:  now,
		CurrentValue: current.Value,
	}

	// A horizon delta is a statement about the period [now-h, now]: it
	// needs the current observation inside that period and a baseline no
	// more than one horizon older than now-h. Otherwise the pointers stay
	// nil (serialized as NULL) — a dormant pair must not read as flat.
	for _, hz := range []struct {
		d   time.Duration
		val **string
		pct **float64
	}{
		{time.Hour, &row.H1Value, &row.H1DeltaPct},
		{24 * time.Hour, &row.H24Value, &row.H24DeltaPct},
		{7 * 24 * time.Hour, &row.D7Value, &row.D7DeltaPct},
		{30 * 24 * time.Hour, &row.D30Value, &row.D30DeltaPct},
	} {
		target := now.Add(-hz.d)
		if !current.At.After(target) {
			continue
		}
		if v, ok := valueAt(series, target, target.Add(-hz.d)); ok {
			*hz.val = ptrStr(v)
			*hz.pct = ptr(deltaPct(v, current.Value))
		}
	}

	// ATH / ATL across the full series, compared with big.Rat so prices with more digits than float64 are never truncated into the ratcheted columns.
	// This is a 30d ATH/ATL, not all-time: the worker only fetches 30d.
	if athValue, athAt, atlValue, atlAt, ok := extremes(series, current); ok {
		row.ATHValue = ptrStr(athValue)
		row.ATHAt = ptrTime(athAt)
		row.ATLValue = ptrStr(atlValue)
		row.ATLAt = ptrTime(atlAt)
	}

	row.StreakDirection, row.StreakDays = computeStreak(series)
	row.Acceleration = computeAcceleration(series)

	return row
}

// extremeBand bounds how far a point may sit from the series median and
// still count toward ATH/ATL. The sink ratchets those with GREATEST/LEAST,
// so a single wild 1m bucket would otherwise pin the published extreme
// permanently.
const extremeBand = 1000

// minPointsForBand is the fewest positive points at which a median says
// anything about which side of a two-point gap is the outlier.
const minPointsForBand = 3

// extremes returns the ATH and ATL over series, ignoring unparseable/zero points and (once long enough) points further than extremeBand from the median.
// ok is false when current is out of band or nothing qualifies: ATH/ATL stay NULL so the sink's GREATEST/LEAST keep stored extremes.
// A zero mid-series must not become the ATL ([100,5,0,90] has ATL 5).
func extremes(series []TimedValue, current TimedValue) (athValue string, athAt time.Time, atlValue string, atlAt time.Time, ok bool) {
	pts := positivePoints(series)
	var lo, hi *big.Rat
	if len(pts) >= minPointsForBand {
		vals := make([]*big.Rat, len(pts))
		for i, q := range pts {
			vals[i] = q.v
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i].Cmp(vals[j]) < 0 })
		med := vals[(len(vals)-1)/2]
		band := new(big.Rat).SetInt64(extremeBand)
		lo, hi = new(big.Rat).Quo(med, band), new(big.Rat).Mul(med, band)
	}
	var athRat, atlRat *big.Rat
	currentIn := false
	for _, q := range pts {
		if hi != nil && (q.v.Cmp(lo) < 0 || q.v.Cmp(hi) > 0) {
			continue
		}
		if q.p.At.Equal(current.At) && q.p.Value == current.Value {
			currentIn = true
		}
		if athRat == nil || q.v.Cmp(athRat) > 0 {
			athRat, athValue, athAt = q.v, q.p.Value, q.p.At
		}
		if atlRat == nil || q.v.Cmp(atlRat) < 0 {
			atlRat, atlValue, atlAt = q.v, q.p.Value, q.p.At
		}
	}
	return athValue, athAt, atlValue, atlAt, currentIn
}

type ratPoint struct {
	v *big.Rat
	p TimedValue
}

// positivePoints keeps the parseable, strictly positive points with their
// exact value.
func positivePoints(series []TimedValue) []ratPoint {
	pts := make([]ratPoint, 0, len(series))
	for _, p := range series {
		v, ok := new(big.Rat).SetString(p.Value)
		if !ok || v.Sign() <= 0 {
			continue
		}
		pts = append(pts, ratPoint{v, p})
	}
	return pts
}

// valueAt returns the most recent observation at or before target (series is oldest-first).
// ok is false when none reaches back that far, or it is older than notBefore (a stale baseline describes a different period).
func valueAt(series []TimedValue, target, notBefore time.Time) (string, bool) {
	// Binary-search the largest index whose At <= target.
	idx := sort.Search(len(series), func(i int) bool {
		return series[i].At.After(target)
	}) - 1
	if idx < 0 || series[idx].At.Before(notBefore) {
		return "", false
	}
	return series[idx].Value, true
}

// deltaPct = (current - past) / past * 100; 0 on past=0 or an unparseable leg (undefined delta, avoids Inf%).
// A percentage, not money: display-grade float64 is accepted here; the *Value fields stay exact.
func deltaPct(pastStr, currentStr string) float64 {
	past, errP := strconv.ParseFloat(pastStr, 64)
	current, errC := strconv.ParseFloat(currentStr, 64)
	if errP != nil || errC != nil || past == 0 {
		return 0
	}
	return (current - past) / past * 100.0
}

// dailyPoint is one daily summary used by computeStreak. The
// streak walks days, not minutes, so an intraday wobble doesn't
// reset the count.
type dailyPoint struct {
	Day   time.Time
	Value float64
}

// dailyize buckets a minute-level series into daily-last-value
// observations. Returns nil if every value parses as invalid.
func dailyize(series []TimedValue) []dailyPoint {
	days := make([]dailyPoint, 0, 32)
	var current dailyPoint
	started := false
	for _, p := range series {
		v, err := strconv.ParseFloat(p.Value, 64)
		if err != nil {
			continue
		}
		dayStart := p.At.Truncate(24 * time.Hour)
		if !started {
			current = dailyPoint{Day: dayStart, Value: v}
			started = true
			continue
		}
		if !dayStart.Equal(current.Day) {
			days = append(days, current)
			current = dailyPoint{Day: dayStart, Value: v}
		} else {
			current.Value = v
		}
	}
	if started {
		days = append(days, current)
	}
	return days
}

// directionOf returns "up" / "down" / "flat" for one diff.
func directionOf(diff float64) string {
	switch {
	case diff > 0:
		return "up"
	case diff < 0:
		return "down"
	default:
		return "flat"
	}
}

// computeStreak walks the daily-summarised series back-to-front
// looking for the longest run of consecutive same-direction moves
// ending at the latest day. Per-day granularity dodges intraday
// noise that would yield ~0 streaks at minute level.
func computeStreak(series []TimedValue) (string, *int) {
	days := dailyize(series)
	if len(days) < 2 {
		return "", nil
	}
	streakDir := ""
	count := 0
	for i := len(days) - 1; i > 0; i-- {
		dir := directionOf(days[i].Value - days[i-1].Value)
		if streakDir == "" {
			streakDir = dir
			count = 1
			continue
		}
		if dir != streakDir {
			break
		}
		count++
	}
	return streakDir, ptrInt(count)
}

// computeAcceleration returns increasing / flat / decreasing for whether the MAGNITUDE of recent per-step moves exceeds older ones (direction-agnostic: a steepening downtrend is increasing).
// It compares the absolute mean per-step delta of the last quarter against the previous with a ±5% deadband; signed deltas mislabelled negative trends.
// Returns empty (NULL) if the series is too short.
func computeAcceleration(series []TimedValue) string {
	if len(series) < 4 {
		return ""
	}
	// Compare the last quarter of the series against the previous
	// quarter. Cheap proxy for "is the move accelerating?"
	q := len(series) / 4
	if q < 1 {
		return ""
	}
	last := math.Abs(avgDelta(series[len(series)-q:]))
	prev := math.Abs(avgDelta(series[len(series)-2*q : len(series)-q]))
	switch {
	case last > prev*1.05:
		return "increasing"
	case last < prev*0.95:
		return "decreasing"
	default:
		return "flat"
	}
}

// avgDelta returns the mean per-step change across a slice. Used by
// computeAcceleration; returns 0 on input < 2.
func avgDelta(slice []TimedValue) float64 {
	if len(slice) < 2 {
		return 0
	}
	var sum float64
	count := 0
	for i := 1; i < len(slice); i++ {
		a, errA := strconv.ParseFloat(slice[i-1].Value, 64)
		b, errB := strconv.ParseFloat(slice[i].Value, 64)
		if errA != nil || errB != nil {
			continue
		}
		sum += b - a
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func ptr(f float64) *float64         { return &f }
func ptrStr(s string) *string        { return &s }
func ptrInt(i int) *int              { return &i }
func ptrTime(t time.Time) *time.Time { return &t }
