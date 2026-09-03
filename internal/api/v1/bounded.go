// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"log/slog"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// readFanoutConcurrency bounds per-request fan-out in handlers that
// run one DB-bound read per row of a listing (catalogue market caps,
// per-page price fills, slug-expansion market merges). Mirrors
// priceBatchConcurrency's rationale: the underlying reads are
// individually cheap, so the win from parallelism saturates around
// the DB pool size — while an UNbounded per-row fan-out is a
// self-inflicted connection-pool exhaustion vector the moment the
// row count grows (the verified-currency catalogue is hand-curated
// and small today, but nothing in the handler enforces that).
const readFanoutConcurrency = 16

// fanoutRowWorker is the worker label a panicking per-row fan-out
// reports under. One label for every caller of [forEachBounded] is
// deliberate: the label set of stellarindex_worker_panics_total must
// stay bounded, and the stack in the accompanying log line carries the
// handler that fanned out.
const fanoutRowWorker = "api-fanout-row"

// forEachBounded runs fn(i) for every i in [0, n), allowing at most
// `limit` invocations to run concurrently. It blocks until all
// invocations return.
//
// This is the ONLY sanctioned shape for per-row fan-out in handlers:
// every goroutine must write only its own index-keyed slot (disjoint
// memory, no lock) and the semaphore caps concurrent DB round-trips.
// Rows that need skipping are an early `return` inside fn — cheaper
// than special-casing the loop. See lookupPriceBatch in price.go for
// the original pattern this factors out.
//
// A panic in fn(i) is CONTAINED here rather than allowed to kill the
// process. These goroutines are detached from the handler's, so
// middleware.Recoverer — which wraps only the handler goroutine — does
// not cover them, and an unrecovered panic in ANY goroutine terminates
// the whole binary. Containing it leaves row i's slot at its ZERO
// value, which is the same outcome every caller already handles for a
// row whose read failed, and the other n-1 rows still land. logger is
// threaded in (rather than defaulted) so the panic is attributable to
// the deployment's configured sink; it may be nil in tests.
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
