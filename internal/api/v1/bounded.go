// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "sync"

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
func forEachBounded(n, limit int, fn func(i int)) {
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
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
