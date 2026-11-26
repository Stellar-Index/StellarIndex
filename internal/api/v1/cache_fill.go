package v1

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// cacheFillBudget bounds a detached fill for caches that had no refresh
// budget of their own. Matches the markets/assets/oracle budgets. For swrCache it also caps how long past its ttl a value with a
// refresh still running is served unflagged: a refresh that cannot finish inside it is late.
const cacheFillBudget = 30 * time.Second

// errCacheFillPanicked is what a waiter receives when the detached fill it
// joined panicked, so a dead fill never reads as an empty success.
var errCacheFillPanicked = errors.New("api cache: fill panicked")

// runDetachedFill is the goroutine body of a single-flighted cache fill. It takes no caller context on purpose, so one waiter's
// abort cannot decide the answer for others; waiters bound themselves on done and their own ctx. A panic settles as
// errCacheFillPanicked so the key is neither wedged nor served as an empty success.
func runDetachedFill[T any](
	logger *slog.Logger,
	name string,
	budget time.Duration,
	done chan struct{},
	upstream func(context.Context) (T, error),
	settle func(T, error),
) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(logger, name, rec)
			var zero T
			settle(zero, errCacheFillPanicked)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	v, err := upstream(ctx)
	observeFillDuration(name, time.Since(start), err)
	settle(v, err)
}

// observeFillDuration records one fill. name is a fixed per-call-site literal, never a key or asset id.
func observeFillDuration(name string, d time.Duration, err error) {
	result := "ok"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
	case err != nil:
		result = "error"
	}
	obs.APICacheFillDuration.WithLabelValues(name, result).Observe(d.Seconds())
}
