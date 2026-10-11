// Package rollupworker holds the ticker loop shared by the aggregator's rollup workers (assetcharacterrollup, assetvolrollup, protoeventsrollup):
// an immediate pass on boot, then one per tick until ctx is cancelled. Each caller keeps its own interval, error handling and metrics in its roll closure.
package rollupworker

import (
	"context"
	"time"
)

// Run calls roll immediately, then every interval until ctx is cancelled and returns ctx.Err(); roll handles its own errors so a transient failure never stops the loop.
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
