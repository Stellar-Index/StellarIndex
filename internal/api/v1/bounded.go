// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"log/slog"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// readFanoutConcurrency bounds per-request fan-out in handlers that
// run one DB-bound read per row of a listing. Mirrors priceBatchConcurrency: the reads are
// individually cheap, so parallelism saturates near the DB pool size, while an unbounded
// per-row fan-out exhausts the connection pool as soon as the row count grows.
const readFanoutConcurrency = 16

// fanoutRowWorker is the worker label a panicking per-row fan-out
// reports under. One shared label keeps the label set of stellarindex_worker_panics_total
// bounded; the stack in the log line identifies the handler that fanned out.
const fanoutRowWorker = "api-fanout-row"

// forEachBounded runs fn(i) for every i in [0, n), allowing at most
// `limit` invocations to run concurrently. It blocks until all
// invocations return.
//
// This is the only sanctioned shape for per-row fan-out in handlers: each goroutine writes
// only its own index-keyed slot (disjoint memory, no lock) and the semaphore caps concurrent
// DB round-trips. Skip a row with an early return inside fn.
//
// A panic in fn(i) is contained here. These goroutines are detached from the handler's, so
// middleware.Recoverer does not cover them and an unrecovered panic would terminate the whole
// binary. Row i's slot stays at its zero value, the outcome callers already handle for a
// failed read, and the other rows still land. logger is passed in so the panic reaches the
// deployment's configured sink; it may be nil in tests.
func forEachBounded(logger *slog.Logger, n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			// Registered FIRST so it unwinds LAST: the semaphore
			// release and wg.Done below must both run before the
			// panic is swallowed, or a panic deadlocks wg.Wait().
			defer wg.Done()
			defer worker.Recover(logger, fanoutRowWorker)
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
