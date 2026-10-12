package v1

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// cacheFillBudget bounds a detached fill for caches that had no refresh
// budget of their own. Matches the markets/assets/oracle budgets.
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
	v, err := upstream(ctx)
	settle(v, err)
}
