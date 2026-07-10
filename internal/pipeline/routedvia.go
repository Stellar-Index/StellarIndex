package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/StellarIndex/stellar-index/internal/sources/soroswap"
	soroswap_router "github.com/StellarIndex/stellar-index/internal/sources/soroswap_router"
)

// Router attribution — the live half of migration 0025's
// trades.routed_via design (BACKLOG #29 Phase B).
//
// Why a periodic sweep instead of tagging at router-persist time:
// the same-tx `trades` rows for a soroswap-router call are written
// by the PROJECTOR (ADR-0031/0032: soroswap trades project from the
// soroban_events landing zone), while the router call itself rides
// the dispatcher's events goroutine. The two writers race — at the
// moment the router row lands, its pair-level trades usually don't
// exist yet, so an inline UPDATE would systematically miss. A
// trailing-window sweep is immune to that ordering, costs the trade
// hot path nothing (one batched UPDATE per tick, first-wins /
// idempotent — see timescale.TagTradesRoutedVia), and re-covers the
// window every tick so projector lag inside the lookback is
// self-healing. Older misses (projector down > lookback) are caught
// by the historical pass: `stellarindex-ops tag-routed-via`.

const (
	// routedViaSweepInterval is how often the sweeper ticks.
	routedViaSweepInterval = time.Minute
	// routedViaSweepLookback is the trailing window each tick
	// re-covers. Must comfortably exceed worst-case routine projector
	// lag; already-tagged rows in the window are no-ops.
	routedViaSweepLookback = 30 * time.Minute
)

// RoutedViaTagger is the storage seam the sweeper writes through.
// *timescale.Store satisfies it via TagTradesRoutedVia.
type RoutedViaTagger interface {
	TagTradesRoutedVia(ctx context.Context, routerName, tradeSource string, from, to time.Time) (int64, error)
}

// RunRoutedViaTagger sweeps the trailing lookback window every
// interval, tagging same-tx soroswap trades via
// timescale.TagTradesRoutedVia — routed_via='soroswap-router' by
// default, or a more specific registered wrapper's name when the
// router call's call_path (migration 0101/0103) identifies one.
// Blocks until ctx cancels (run it in its own goroutine); performs
// one final sweep on shutdown so the last partial window isn't left
// to the next boot's first tick.
//
// interval/lookback <= 0 select the package defaults.
func RunRoutedViaTagger(ctx context.Context, logger *slog.Logger, store RoutedViaTagger, interval, lookback time.Duration) {
	if interval <= 0 {
		interval = routedViaSweepInterval
	}
	if lookback <= 0 {
		lookback = routedViaSweepLookback
	}
	if logger == nil {
		logger = slog.Default()
	}

	sweep := func(sweepCtx context.Context) {
		now := time.Now().UTC()
		tagged, err := store.TagTradesRoutedVia(sweepCtx,
			soroswap_router.SourceName, soroswap.SourceName,
			now.Add(-lookback), now)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("routed-via sweep failed", "err", err)
			return
		}
		if tagged > 0 {
			logger.Info("routed-via sweep tagged trades",
				"router", soroswap_router.SourceName, "tagged", tagged)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Immediate first sweep so a restart doesn't wait a full
	// interval to resume attribution.
	sweep(ctx)
	for {
		select {
		case <-ticker.C:
			sweep(ctx)
		case <-ctx.Done():
			// Final sweep on a short leash. Deliberately NOT derived
			// from ctx — the parent is already cancelled and the whole
			// point is one last flush before exit.
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:contextcheck // parent ctx is cancelled by definition here
			sweep(flushCtx)                                                               //nolint:contextcheck // see above
			cancel()
			return
		}
	}
}
