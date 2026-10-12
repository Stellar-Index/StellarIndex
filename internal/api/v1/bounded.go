// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"log/slog"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// readFanoutConcurrency bounds per-request fan-out of one DB read per listing row. Mirrors priceBatchConcurrency: reads are
// cheap and saturate near the pool size, while unbounded fan-out exhausts the connection pool.
const readFanoutConcurrency = 16

// fanoutRowWorker is the worker label for a panicking per-row fan-out; one shared label keeps the
// stellarindex_worker_panics_total label set bounded.
const fanoutRowWorker = "api-fanout-row"

// forEachBounded runs fn(i) for i in [0, n) with at most `limit` concurrent, blocking until all return. The only sanctioned per-row fan-out
// shape: each goroutine writes only its own index-keyed slot (no lock). A panic in fn is contained here because these goroutines escape
// middleware.Recoverer and would kill the binary; row i stays zero-valued and other rows land. logger may be nil in tests.
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
