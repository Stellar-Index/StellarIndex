// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestForEachBounded_RunsEveryIndexOnce — all n indices execute
// exactly once, in disjoint slots.
func TestForEachBounded_RunsEveryIndexOnce(t *testing.T) {
	const n = 100
	counts := make([]int32, n)
	forEachBounded(n, 7, func(i int) {
		atomic.AddInt32(&counts[i], 1)
	})
	for i, c := range counts {
		if c != 1 {
			t.Errorf("index %d ran %d times, want exactly 1", i, c)
		}
	}
}

// TestForEachBounded_RespectsConcurrencyLimit — the observed peak
// concurrency never exceeds the limit. This is the property the
// helper exists for: per-row DB fan-out must not scale with row
// count (connection-pool exhaustion).
func TestForEachBounded_RespectsConcurrencyLimit(t *testing.T) {
	const (
		n     = 200
		limit = 5
	)
	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		barrier  sync.WaitGroup
	)
	// Hold the first `limit` invocations at a barrier so the peak
	// is actually exercised rather than racing past instantly.
	barrier.Add(1)
	released := atomic.Bool{}
	forEachBounded(n, limit, func(_ int) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if cur == limit && released.CompareAndSwap(false, true) {
			barrier.Done() // full — release everyone
		}
		barrier.Wait()
		inFlight.Add(-1)
	})
	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrency = %d, want <= %d", got, limit)
	}
	if got := peak.Load(); got < limit {
		t.Errorf("peak concurrency = %d, want the limit (%d) to be reached — the barrier should force saturation", got, limit)
	}
}

// TestForEachBounded_DegenerateInputs — zero/negative n is a no-op;
// a non-positive limit degrades to serial, not a panic.
func TestForEachBounded_DegenerateInputs(t *testing.T) {
	ran := false
	forEachBounded(0, 4, func(_ int) { ran = true })
	forEachBounded(-3, 4, func(_ int) { ran = true })
	if ran {
		t.Error("fn ran for n <= 0")
	}
	var count int32
	forEachBounded(3, 0, func(_ int) { atomic.AddInt32(&count, 1) })
	if count != 3 {
		t.Errorf("limit<=0: ran %d of 3", count)
	}
}
