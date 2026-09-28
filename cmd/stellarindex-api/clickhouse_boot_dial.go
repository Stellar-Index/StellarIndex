package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// The lake seams are captured by value when the handlers are built, so a
// dial that gives up leaves them nil until restart. The window runs before
// the listener is up, so it must stay well inside the deploy health probe's
// first attempt (health_grace_seconds); After=clickhouse-server.service
// covers a co-located reboot race beyond it. Both readers dial concurrently
// inside the one window, so the listener waits at most
// clickhouseBootDialBudget in total.
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

// dialAttempt returns by deadline even when dial does not: clickhouse-go
// bounds the TCP connect and the handshake by the reader's DialTimeout each,
// not ctx, and an Options.DialContext would cover only the connect. An
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
