package main

import (
	"context"
	"log/slog"
	"time"
)

// refreshWithTimeout runs one refresh bounded by timeout and logs a failure
// under msg; a failed refresh keeps the cache's previous snapshot.
func refreshWithTimeout(ctx context.Context, refresh func(context.Context) error, timeout time.Duration, logger *slog.Logger, msg string) {
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := refresh(refreshCtx); err != nil {
		logger.Warn(msg, "err", err)
	}
}

// runRefreshLoop calls refreshWithTimeout on every interval tick until ctx
// ends. timeout must stay below interval so refreshes never stack.
func runRefreshLoop(ctx context.Context, refresh func(context.Context) error, interval, timeout time.Duration, logger *slog.Logger, msg string) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// select picks at random when both are ready; never start a
			// refresh after cancellation.
			if ctx.Err() != nil {
				return
			}
			refreshWithTimeout(ctx, refresh, timeout, logger, msg)
		}
	}
}
