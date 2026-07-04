package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
)

// RollupRow is one (day, subject, endpoint) aggregate handed to the
// Timescale sink. Counts are the CUMULATIVE Redis counters for that
// day (not deltas) — the sink upserts with GREATEST() so re-sweeps
// are idempotent and a mid-day Redis flush can never regress a row.
type RollupRow struct {
	Day          string // YYYY-MM-DD UTC
	Subject      string // "key:<id>" / "id:<identifier>"
	Endpoint     string // route pattern, e.g. "/v1/price"
	OK           int64  // status < 400
	ClientErrors int64  // 4xx except 429
	ServerErrors int64  // 5xx
	Throttled    int64  // 429
}

// RollupSink persists a batch of daily aggregates. Production
// wiring is *timescale.Store's UpsertUsageDaily via the adapter in
// cmd/stellarindex-api/main.go; tests use a fake. Implementations
// MUST be idempotent for identical batches (upsert, not insert).
type RollupSink interface {
	UpsertUsageDaily(ctx context.Context, rows []RollupRow) error
}

// DefaultRollupInterval is the sweep cadence. Five minutes keeps
// the dashboard's "today" row at most one tick stale while the
// per-sweep cost stays tiny (one SCAN + one HGETALL per active
// subject-day, then one batched upsert).
const DefaultRollupInterval = 5 * time.Minute

// Rollup is the ticker-driven worker that folds the Redis
// per-endpoint detail hashes into the `usage_daily` Timescale
// hypertable. Runs inside the API binary (the only writer of the
// Redis counters and the only reader of the rollups).
//
// Each sweep covers TODAY + YESTERDAY (UTC): today because it's the
// live day, yesterday so counters written just before midnight are
// re-folded after the boundary and the final day total lands even
// if the last pre-midnight sweep missed them.
type Rollup struct {
	counter  *Counter
	sink     RollupSink
	interval time.Duration
	logger   *slog.Logger
	nowFn    func() time.Time
}

// NewRollup constructs the worker. Returns nil when either
// dependency is missing so callers can gate with a plain nil check
// (mirrors [New]'s nil-Redis posture).
func NewRollup(counter *Counter, sink RollupSink, interval time.Duration, logger *slog.Logger) *Rollup {
	if counter == nil || sink == nil {
		return nil
	}
	if interval <= 0 {
		interval = DefaultRollupInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Rollup{
		counter:  counter,
		sink:     sink,
		interval: interval,
		logger:   logger,
		nowFn:    counter.nowFn,
	}
}

// Run sweeps once immediately, then on every tick until ctx is
// cancelled. Sweep failures log + count in the metric; the worker
// never exits on a transient Redis/Postgres error.
func (r *Rollup) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("usage: nil Rollup")
	}
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		if n, err := r.Sweep(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			r.logger.Warn("usage rollup sweep failed", "err", err)
		} else if n > 0 {
			r.logger.Debug("usage rollup sweep", "rows", n)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep reads today's + yesterday's detail counters out of Redis,
// groups them per (day, subject, endpoint), and upserts the batch
// into the sink. Returns the number of rows upserted. Exposed for
// tests and for a potential ops-side manual re-sweep.
func (r *Rollup) Sweep(ctx context.Context) (int, error) {
	start := time.Now()
	now := r.nowFn().UTC()
	dates := []string{
		now.AddDate(0, 0, -1).Format("2006-01-02"),
		now.Format("2006-01-02"),
	}
	details, err := r.counter.ScanDetail(ctx, dates)
	if err != nil {
		r.observe("scan_error", start)
		return 0, fmt.Errorf("usage rollup: scan: %w", err)
	}
	rows := groupDetails(details)
	if len(rows) == 0 {
		r.observe("ok", start)
		return 0, nil
	}
	if err := r.sink.UpsertUsageDaily(ctx, rows); err != nil {
		r.observe("sink_error", start)
		return 0, fmt.Errorf("usage rollup: upsert %d rows: %w", len(rows), err)
	}
	r.observe("ok", start)
	return len(rows), nil
}

// observe records the paired outcome counter + latency histogram
// (the wave-88/89/90/91 worker convention).
func (r *Rollup) observe(outcome string, start time.Time) {
	obs.UsageRollupSweepsTotal.WithLabelValues(outcome).Inc()
	obs.UsageRollupSweepDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
}

// groupDetails folds per-class detail rows into per-(day, subject,
// endpoint) RollupRows. Unknown classes are ignored (forward-compat:
// an older binary sweeping hashes written by a newer one must not
// misfile counts).
func groupDetails(details []DetailRow) []RollupRow {
	type rollupKey struct{ day, subject, endpoint string }
	grouped := make(map[rollupKey]*RollupRow, len(details))
	order := make([]rollupKey, 0, len(details))
	for _, d := range details {
		k := rollupKey{d.Date, d.Subject, d.Endpoint}
		row, ok := grouped[k]
		if !ok {
			row = &RollupRow{Day: d.Date, Subject: d.Subject, Endpoint: d.Endpoint}
			grouped[k] = row
			order = append(order, k)
		}
		switch d.Class {
		case ClassOK:
			row.OK += d.Count
		case ClassClientError:
			row.ClientErrors += d.Count
		case ClassServerError:
			row.ServerErrors += d.Count
		case ClassThrottled:
			row.Throttled += d.Count
		}
	}
	out := make([]RollupRow, 0, len(order))
	for _, k := range order {
		out = append(out, *grouped[k])
	}
	return out
}
