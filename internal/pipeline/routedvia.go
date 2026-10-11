package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// Router attribution sweep. The projector writes the same-tx soroswap `trades` rows (ADR-0031/0032) while the router call rides the dispatcher, so the two race and an inline UPDATE would miss.
// A trailing-window sweep is race-free, first-wins and idempotent (TagTradesRoutedVia), and self-heals projector lag within the lookback;
// older misses are caught by `stellarindex-ops tag-routed-via`.

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

// RunRoutedViaTagger sweeps the trailing lookback every interval, tagging same-tx soroswap trades via TagTradesRoutedVia, and sweeps once more on shutdown; interval/lookback <= 0 select defaults.
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
