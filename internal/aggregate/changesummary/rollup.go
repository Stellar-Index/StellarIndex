// Package changesummary computes the multi-window delta strip
// every entity on the explorer renders.
//
// Periodic worker reads the source table for each (entity_type,
// entity_id), computes h1/h24/d7/d30 deltas + ATH/ATL + streak +
// acceleration, and writes one row to the change_summary_5m
// hypertable. Every list view + every detail page on the explorer
// reads from this in O(1) — without it, every render would do
// N+1 queries against prices_1m.
//
// See migrations/0022_create_change_summary_5m.up.sql for the
// table shape and docs/architecture/explorer-data-inventory.md
// §6.1 + §9.6 for the design.
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
)

// Row is the storage-neutral shape the worker hands to the Sink.
// The aggregator binary translates this to timescale.ChangeSummaryRow
// at the boundary; changesummary stays free of a storage import to
// avoid the cycle (storage owns the read-side Point type via the
// PriceSource adapter).
//
// SCALE. Every *Value field is the RAW prices_1m ratio the PriceSource
// returned — deliberately NOT dex-nonstandard-decimals normalised. The
// sink ratchets ATHValue / ATLValue against the stored row (GREATEST /
// LEAST), and an asset is confirmed non-7-decimals only after it has
// been trading, so normalising here would change a row's scale mid-life
// and pin the ratchet to an extreme from the old scale permanently. The
// table stays raw, like the CAGG it is derived from, and the reader
// normalises: internal/api/v1's changeSummaryResponse. Any NEW reader of
// change_summary_5m must do the same. The *DeltaPct fields, the streak
// and the acceleration are scale-free.
type Row struct {
	EntityType   string
	EntityID     string
	RefreshedAt  time.Time
	CurrentValue string

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

// PriceSource is the read seam — what the worker queries to compute
// deltas. Implemented by timescale.Store via TimedVWAPs1mForChangeSummary.
//
// The worker passes a window of [from, to) and expects the source to
// return CLOSED buckets only (ADR-0015), ordered oldest-first, each
// timestamped by its bucket end.
type PriceSource interface {
	TimedVWAPs1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]TimedValue, error)
}

// TimedValue is one (timestamp, value) data point. value is decimal
// as a string to dodge float precision issues during delta math —
// the worker parses to float64 only at the moment of computing %
// change, accepting the rounding because deltas are display-grade.
type TimedValue struct {
	At    time.Time
	Value string
}

// Sink is the write seam — where computed summaries land. The
// aggregator binary wires an adapter that translates Row to the
// timescale row type and calls UpsertChangeSummary.
type Sink interface {
	UpsertChangeSummary(ctx context.Context, row Row) error
}

// Worker periodically refreshes change_summary_5m for a configured
// set of (entity_type, entity_id, pair) tuples.
//
// Run via [Worker.Run]; caller cancels via context.
type Worker struct {
	source   PriceSource
	sink     Sink
	logger   *slog.Logger
	interval time.Duration
	clock    func() time.Time

	// entities is the working set. Each one maps an
	// (entity_type, entity_id) coordinate to the canonical pair
	// whose VWAP drives the entity's deltas. An entity with no
	// single canonical pair — a protocol summing across pools, an
	// ingest source summing across assets — cannot be expressed
	// here at all, and no other worker computes one, so 'protocol'
	// and 'source' rows do not exist on any deployment. The API
	// surface says so rather than promising them (see
	// allowedChangeSummaryEntityTypes in internal/api/v1/changes.go).
	entities []Entity
}

// Entity binds an (entity_type, entity_id) tuple to the source pair
// whose 1-minute VWAP series drives the deltas.
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

// New constructs a Worker. Returns an error if required fields are
// missing.
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

// Run blocks until ctx is cancelled, refreshing every interval.
// Returns nil on context cancellation; never returns an error
// (per-entity failures log + continue so one bad pair doesn't kill
// the whole worker).
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()

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
// failures log + continue — a single broken pair must not block
// the rest of the working set.
func (w *Worker) refresh(ctx context.Context) {
	now := w.clock().UTC()
	// Look back 30 days + a small buffer to capture the current
	// observation that anchors d30 deltas.
	from := now.Add(-30 * 24 * time.Hour).Add(-1 * time.Hour)

	for _, ent := range w.entities {
		if err := w.refreshOne(ctx, ent, from, now); err != nil {
			w.logger.Debug("change-summary refresh",
				"type", ent.Type, "id", ent.ID, "err", err)
		}
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

// computeSummary derives the full Row from a sorted (oldest-first)
// series of TimedValues. Pulled out of refreshOne so it's directly
// testable without a fake PriceSource.
//
// Empty series is rejected upstream — by the time we land here,
// len(series) >= 1.
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

	// ATH / ATL across the full series, compared with big.Rat so a price
	// with more significant digits than float64 carries (see GH #602)
	// never gets truncated on the way into the ratcheted ath_value /
	// atl_value columns. Note this is "30d ATH" not all-time — the
	// worker only fetches 30d of history. A future pass that wants true
	// all-time can switch the query to a 1-day-bucketed CAGG covering
	// the full hypertable.
	athValue, athAt := current.Value, current.At
	atlValue, atlAt := current.Value, current.At
	athRat, seedOK := new(big.Rat).SetString(current.Value)
	if seedOK {
		atlRat := new(big.Rat).Set(athRat)
		for _, p := range series {
			v, ok := new(big.Rat).SetString(p.Value)
			// Skip unparseable or zero points explicitly. A zero (or
			// unparseable value) mid-series must not become the ATL:
			// the previous `|| atlValue == 0` reset corrupted ATL
			// whenever a single bad/zero point appeared ([100,5,0,90]
			// yielded ATL=90 instead of 5).
			if !ok || v.Sign() == 0 {
				continue
			}
			if v.Cmp(athRat) > 0 {
				athRat, athValue, athAt = v, p.Value, p.At
			}
			if v.Cmp(atlRat) < 0 {
				atlRat, atlValue, atlAt = v, p.Value, p.At
			}
		}
	}
	row.ATHValue = ptrStr(athValue)
	row.ATHAt = ptrTime(athAt)
	row.ATLValue = ptrStr(atlValue)
	row.ATLAt = ptrTime(atlAt)

	row.StreakDirection, row.StreakDays = computeStreak(series)
	row.Acceleration = computeAcceleration(series)

	return row
}

// valueAt returns the most-recent observation whose timestamp is
// at-or-before target. ok=false when no observation reaches that far
// back, or when that observation is older than notBefore (a baseline
// that stale describes a different period). series is assumed sorted
// oldest-first.
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

// deltaPct = (current - past) / past * 100. Returns 0 on past=0 or an
// unparseable leg (avoids divide-by-zero; a delta against a zero
// baseline is undefined and we'd rather leak a fresh entity's NULL
// than stamp Inf%). *DeltaPct is a percentage, not money — display-grade
// float64 rounding here is accepted by design (GH #602's fix keeps the
// underlying *Value fields exact; only this ratio stays float).
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

// computeAcceleration returns 'increasing' / 'flat' / 'decreasing'
// describing whether the MAGNITUDE of recent per-step moves is bigger
// or smaller than the older moves — i.e. is momentum building. The
// label is direction-agnostic: a steepening downtrend and a steepening
// uptrend both report "increasing".
//
// We compare the absolute mean per-step delta of the last quarter
// against the previous quarter with a ±5% deadband. Comparing the
// signed deltas directly (the previous implementation) inverted for
// negative trends — a steady downtrend's `last < prev*0.95` branch
// fired and mislabelled it "decreasing" (and a steepening downtrend,
// where last is MORE negative than prev, was labelled "decreasing"
// when momentum was in fact building). Taking magnitudes first fixes
// both.
//
// Defensive: returns empty string if series too short for the
// comparison. Caller treats empty as NULL.
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
