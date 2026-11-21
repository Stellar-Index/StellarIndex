package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The window runs before the listener is up and must stay inside the first deploy health probe (health_grace_seconds); both readers
// dial concurrently.
var (
	clickhouseBootDialBudget     = 7 * time.Second
	clickhouseBootDialBackoffMin = time.Second
	clickhouseBootDialBackoffMax = 15 * time.Second
	// The last attempt starts at least this long before the deadline, so it
	// can complete a local connect + handshake instead of starting expired.
	clickhouseBootDialMinAttempt = time.Second
)

var (
	errDialAbandoned = errors.New("clickhouse dial abandoned at the boot window")
	errDialPanicked  = errors.New("clickhouse dial panicked")
)

// openLakeReaders waits up to deadline for ClickHouse, then builds any reader whose dial gave up over the driver's lazy pool anyway.
// The lake seams are captured by value, so a nil reader would stay nil until restart; a pooled one re-dials on its next query.
// An error is left only when the reader cannot be built at all (its configuration).
func openLakeReaders(ctx context.Context, logger *slog.Logger, addr, user, pass string, deadline time.Time) (
	supply *clickhouse.SupplyReader, supplyErr error, explorer *clickhouse.ExplorerReader, explorerErr error,
) {
	supply, supplyErr, explorer, explorerErr = dialLakeReadersAtBoot(ctx, logger, addr, deadline,
		func(ctx context.Context) (*clickhouse.SupplyReader, error) {
			return clickhouse.NewSupplyReaderAuth(ctx, addr, user, pass)
		},
		func(ctx context.Context) (*clickhouse.ExplorerReader, error) {
			return clickhouse.NewExplorerReaderAuth(ctx, addr, user, pass)
		})
	if supplyErr != nil {
		logger.Warn("clickhouse unreachable through the boot window; supply reader re-dials per query", "addr", addr, "err", supplyErr)
		supply, supplyErr = clickhouse.OpenSupplyReaderAuth(addr, user, pass)
	}
	if explorerErr != nil {
		logger.Warn("clickhouse unreachable through the boot window; explorer reader re-dials per query", "addr", addr, "err", explorerErr)
		explorer, explorerErr = clickhouse.OpenExplorerReaderAuth(addr, user, pass)
	}
	return supply, supplyErr, explorer, explorerErr
}

// dialLakeReadersAtBoot dials the supply and explorer readers concurrently,
// each with the whole window.
func dialLakeReadersAtBoot[S, E io.Closer](
	ctx context.Context,
	logger *slog.Logger,
	addr string,
	deadline time.Time,
	dialSupply func(context.Context) (S, error),
	dialExplorer func(context.Context) (E, error),
) (supply S, supplyErr error, explorer E, explorerErr error) {
	var wg sync.WaitGroup
	supplyErr = errDialPanicked
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer recoverBackgroundWorker(logger, "clickhouse-boot-dial-supply")
		supply, supplyErr = dialClickHouseAtBoot(ctx, logger, "supply", addr, deadline, dialSupply)
	}()
	explorer, explorerErr = dialClickHouseAtBoot(ctx, logger, "explorer", addr, deadline, dialExplorer)
	wg.Wait()
	return supply, supplyErr, explorer, explorerErr
}

// dialClickHouseAtBoot calls dial until it succeeds, ctx is cancelled, or
// no attempt can start with clickhouseBootDialMinAttempt left before
// deadline, returning the last error on failure. It returns by deadline.
func dialClickHouseAtBoot[T io.Closer](ctx context.Context, logger *slog.Logger, what, addr string, deadline time.Time, dial func(context.Context) (T, error)) (T, error) {
	backoff := clickhouseBootDialBackoffMin
	var lastDialErr error
	for attempt := 1; ; attempt++ {
		r, err := dialAttempt(ctx, logger, deadline, dial)
		if errors.Is(err, errDialAbandoned) && lastDialErr != nil {
			err = errors.Join(err, lastDialErr)
		} else if err != nil {
			lastDialErr = err
		}
		if err == nil {
			if attempt > 1 {
				logger.Info("clickhouse reachable after boot retries", "reader", what, "addr", addr, "attempts", attempt)
			}
			return r, nil
		}
		wait := min(backoff, time.Until(deadline)-clickhouseBootDialMinAttempt)
		if ctx.Err() != nil || wait < 0 {
			return r, err
		}
		if attempt == 1 {
			logger.Warn("clickhouse not reachable at boot; retrying", "reader", what, "addr", addr, "err", err, "until", deadline.UTC().Format(time.RFC3339))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return r, err
		case <-t.C:
		}
		backoff = min(backoff*2, clickhouseBootDialBackoffMax)
	}
}

// dialAttempt returns by deadline even when dial does not: clickhouse-go bounds connect and handshake by DialTimeout each, not ctx. An
// abandoned dial finishes in the background and a late success is closed.
func dialAttempt[T io.Closer](ctx context.Context, logger *slog.Logger, deadline time.Time, dial func(context.Context) (T, error)) (T, error) {
	attemptCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	type result struct {
		r   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		res := result{err: errDialPanicked}
		defer func() { done <- res }()
		defer recoverBackgroundWorker(logger, "clickhouse-boot-dial")
		res.r, res.err = dial(attemptCtx)
	}()
	select {
	case res := <-done:
		return res.r, res.err
	case <-attemptCtx.Done():
	}
	select {
	case res := <-done:
		return res.r, res.err
	default:
	}
	go func() {
		defer recoverBackgroundWorker(logger, "clickhouse-boot-dial-reap")
		if res := <-done; res.err == nil {
			_ = res.r.Close()
		}
	}()
	var zero T
	return zero, fmt.Errorf("%w: %w", errDialAbandoned, context.Cause(attemptCtx))
}
