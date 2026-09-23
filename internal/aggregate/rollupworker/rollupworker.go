// Package rollupworker holds the ticker-driven control flow shared by the
// aggregator's per-rollup workers (assetcharacterrollup, assetvolrollup,
// protoeventsrollup): an immediate pass on boot, then one pass per tick
// until the context is cancelled. Each caller keeps its own DefaultInterval,
// refresh error handling and metrics in a roll closure; only the loop is
// shared.
package rollupworker

import (
	"context"
	"time"
)

// Run calls roll once immediately, then again on every tick of interval,
// until ctx is cancelled, at which point it returns ctx.Err(). roll is
// expected to handle its own errors (log + metric) rather than return one,
// matching the aggregator worker convention: a transient failure must not
// stop the loop.
func Run(ctx context.Context, interval time.Duration, roll func(context.Context)) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	roll(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			roll(ctx)
		}
	}
}
