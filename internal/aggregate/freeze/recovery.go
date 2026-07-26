package freeze

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// OpenFreezeLister enumerates the (asset, quote) pairs that the
// durable mirror still records as firing. Implemented by
// `internal/storage/timescale.FreezeEventSink.ListOpen`.
type OpenFreezeLister interface {
	ListOpen(ctx context.Context) ([]OpenFreezePair, error)
}

// OpenFreezePair mirrors `timescale.OpenFreeze` to keep the freeze
// package free of a hard dependency on the storage adapter.
type OpenFreezePair struct {
	Asset canonical.Asset
	Quote canonical.Asset
}

// Recoverer closes a firing freeze row, stamping recovered_at=now
// on the open `freeze_events` row for (asset, quote). Implemented
// by `internal/storage/timescale.FreezeEventSink.MarkRecovered`.
type Recoverer interface {
	MarkRecovered(ctx context.Context, asset, quote canonical.Asset) error
}

// Recovery is the freeze-recovery worker. It periodically lists
// every still-open `freeze_events` row, checks whether the Redis
// marker for (asset, quote) is still alive, and stamps
// recovered_at on the durable row when the marker is gone.
//
// This is the inverse half of the freeze pipeline: the orchestrator
// writes a Redis marker + INSERTs an open `freeze_events` row when
// a freeze fires; the marker has a TTL (typically a few minutes);
// when the underlying anomaly clears, the orchestrator stops
// refreshing the marker and the TTL elapses; the recovery worker
// notices and closes the durable row so the explorer /anomalies
// timeline shows a finished freeze rather than a forever-firing one.
//
// Why poll Redis instead of subscribing to keyspace expiry events?
// Redis keyspace notifications are off by default and operators
// typically don't enable them in production (significant CPU
// overhead). A 60-second poll loop is cheap (one Redis MGET per
// minute against the small set of currently-firing pairs) and
// matches the fact that the API's freshness SLO is also minute-
// scale — sub-minute recovery latency on the explorer timeline
// would be wasted precision.
type Recovery struct {
	cache    RedisCache
	lister   OpenFreezeLister
	closer   Recoverer
	ladder   LadderStore
	grace    time.Duration
	interval time.Duration
	logger   *slog.Logger
}

// RecoveryOptions tunes a [Recovery] worker.
type RecoveryOptions struct {
	// Interval between polls. Default 60s — matches the API
	// freshness SLO and keeps Redis-side load minimal even when
	// many pairs are firing concurrently.
	Interval time.Duration
	// Logger receives structured logs at INFO (recovery events) and
	// WARN (Redis or postgres failures). Default = slog.Default().
	Logger *slog.Logger

	// Ladder is the durable ADR-0019 ladder (migration 0119). Wiring it is
	// what stops this worker from DESTROYING the ladder it is supposed to
	// coexist with.
	//
	// This worker's whole trigger is "the Redis marker is gone", which since
	// 0119 is ambiguous: it means either the freeze ended (the case this
	// worker exists for) or Redis lost the marker (the case the durable
	// ladder exists for). Closing the row is not a neutral act in the second
	// case — `recovered_at IS NULL` is the exact predicate
	// [LadderStore.LoadLadder] tests, so stamping it deletes the rehydrate's
	// evidence AND records on /v1/anomalies that the freeze recovered
	// normally. On an aggregator restart after a flush this worker's
	// immediate first tick (see [Recovery.Run]) beats the orchestrator's
	// first tick essentially always, because the orchestrator computes VWAPs
	// before it evaluates any freeze.
	//
	// Nil = pre-0119 behaviour: every marker-miss closes its row.
	Ladder LadderStore

	// LadderGrace is how far past a durable hold's expiry the ladder is
	// still treated as live. MUST match the [Writer]'s — the two are reading
	// the same fact from opposite directions. Zero → [DefaultLadderGrace].
	LadderGrace time.Duration
}

// NewRecovery constructs a recovery worker. cache + lister + closer
// are all required — passing nil panics, since a recovery worker
// without one of its three legs is a no-op pretending to be useful.
func NewRecovery(
	cache RedisCache,
	lister OpenFreezeLister,
	closer Recoverer,
	opts RecoveryOptions,
) *Recovery {
	if cache == nil {
		panic("freeze: NewRecovery: cache must not be nil")
	}
	if lister == nil {
		panic("freeze: NewRecovery: lister must not be nil")
	}
	if closer == nil {
		panic("freeze: NewRecovery: closer must not be nil")
	}
	if opts.Interval <= 0 {
		opts.Interval = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.LadderGrace <= 0 {
		opts.LadderGrace = DefaultLadderGrace
	}
	return &Recovery{
		cache:    cache,
		lister:   lister,
		closer:   closer,
		ladder:   opts.Ladder,
		grace:    opts.LadderGrace,
		interval: opts.Interval,
		logger:   opts.Logger.With("component", "freeze-recovery"),
	}
}

// Run drives the recovery loop until ctx is cancelled. Returns
// the context error on shutdown — caller's WaitGroup-style worker
// pattern can `_ = r.Run(ctx)` and move on.
func (r *Recovery) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	// Run once immediately so the explorer doesn't wait one full
	// interval after aggregator restart for stale-row cleanup.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick performs one recovery sweep: list open rows, check Redis,
// close the rows whose marker is gone. Errors are logged + counted;
// never propagated (a failed sweep is recoverable on the next tick).
func (r *Recovery) tick(ctx context.Context) {
	// Time the whole sweep — recorded against the outcome label
	// at every exit point so operators chart per-outcome p95/p99.
	// Sweep latency scales with open-row count (each row = one
	// Redis GET + maybe one Postgres MarkRecovered).
	start := time.Now()

	open, err := r.lister.ListOpen(ctx)
	if err != nil {
		r.logger.Warn("list open freezes", "err", err)
		obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues("error").Inc()
		obs.AnomalyFreezeRecoverySweepDurationSeconds.WithLabelValues("error").Observe(time.Since(start).Seconds())
		return
	}
	if len(open) == 0 {
		obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues("ok").Inc()
		obs.AnomalyFreezeRecoverySweepDurationSeconds.WithLabelValues("ok").Observe(time.Since(start).Seconds())
		return
	}
	var recovered, stillFiring, heldByLadder, errs int
	for _, p := range open {
		key := cachekeys.Freeze(p.Asset, p.Quote)
		_, err := r.cache.Get(ctx, key.String()).Bytes()
		switch {
		case errors.Is(err, redis.Nil):
			// Marker gone. Since migration 0119 that is AMBIGUOUS: either
			// the freeze ended (close the row — this worker's whole job) or
			// Redis lost the marker (leave it; the orchestrator's next tick
			// rehydrates the ladder from this very row). Ask the durable
			// ladder which one it is before destroying the evidence.
			if r.ladderStillHolds(ctx, p) {
				heldByLadder++
				continue
			}
			// Freeze cleared. Close the durable row.
			if err := r.closer.MarkRecovered(ctx, p.Asset, p.Quote); err != nil {
				r.logger.Warn("MarkRecovered failed",
					"asset", p.Asset.String(),
					"quote", p.Quote.String(),
					"err", err)
				errs++
				continue
			}
			recovered++
			obs.AnomalyFreezeRecoveredTotal.Inc()
			r.logger.Info("freeze recovered",
				"asset", p.Asset.String(),
				"quote", p.Quote.String())
		case err != nil:
			r.logger.Warn("Redis Get failed during recovery sweep",
				"key", key,
				"err", fmt.Errorf("recovery: %w", err))
			errs++
		default:
			stillFiring++
		}
	}
	r.logger.Debug("recovery sweep complete",
		"open", len(open),
		"recovered", recovered,
		"still_firing", stillFiring,
		"held_by_ladder", heldByLadder,
		"errs", errs)
	outcome := "ok"
	if errs > 0 {
		outcome = "partial"
	}
	obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues(outcome).Inc()
	obs.AnomalyFreezeRecoverySweepDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// ladderStillHolds reports whether the pair's DURABLE ladder says this
// freeze is still running, i.e. whether the missing marker is Redis's fault
// rather than the freeze's end.
//
// Fail-safe direction is deliberately "close it" (the pre-0119 answer):
//
//   - no ladder store wired → pre-0119 behaviour exactly;
//   - store read failed → do not strand an open row forever on a Postgres
//     blip. The cost of closing wrongly is bounded (the orchestrator
//     re-freezes on the pair's own signal if the anomaly is live), whereas
//     never closing leaves /v1/anomalies showing a finished freeze as
//     permanently firing, which is this worker's entire reason to exist;
//   - ladder present but its hold has lapsed beyond the grace → the
//     aggregator has been down longer than the freeze's own hold, so nobody
//     is coming to rehydrate it. Close it.
//
// Uses the SAME [LadderStillLive] predicate [Writer.LoadState] uses, because
// the two are reading one fact from opposite directions and a divergence
// re-opens the finding this guard exists to close.
func (r *Recovery) ladderStillHolds(ctx context.Context, p OpenFreezePair) bool {
	if r.ladder == nil {
		return false
	}
	st, ok, err := r.ladder.LoadLadder(ctx, p.Asset, p.Quote)
	if err != nil {
		r.logger.Warn("durable ladder read failed during recovery sweep — closing the row on the pre-0119 rule",
			"asset", p.Asset.String(), "quote", p.Quote.String(), "err", err)
		return false
	}
	if !ok || !LadderStillLive(st, r.grace, time.Now()) {
		return false
	}
	r.logger.Info("freeze marker missing but the durable hold is still live — leaving the row OPEN for the orchestrator to rehydrate",
		"asset", p.Asset.String(),
		"quote", p.Quote.String(),
		"hold_until", st.HoldUntil,
		"extensions_used", st.ExtensionsUsed,
		"escalated", st.Escalated)
	return true
}
