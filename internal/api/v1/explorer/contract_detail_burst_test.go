package explorer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestContractDetailCached_ColdBurstDrainsNotSheds: a burst of distinct cold contract keys, larger than both the class share (2) and
// the whole gate (8), is served by queueing for a slot rather than answering 503, and never runs more than the class share at once.
func TestContractDetailCached_ColdBurstDrainsNotSheds(t *testing.T) {
	h, _ := newSWRHandler()
	const n = 10 // 2 slots + 8 waiters: the bounded capacity of one client-keyed class at the default limit
	release := make(chan struct{})
	started := make(chan struct{}, n)
	var inFlight, maxInFlight atomic.Int64
	compute := func(context.Context) (any, error) {
		cur := inFlight.Add(1)
		for prev := maxInFlight.Load(); cur > prev && !maxInFlight.CompareAndSwap(prev, cur); prev = maxInFlight.Load() {
		}
		started <- struct{}{}
		<-release
		inFlight.Add(-1)
		return "payload", nil
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	fire := func(i int) {
		wg.Go(func() {
			_, _, _, errs[i] = h.contractDetailCached(context.Background(), fmt.Sprintf("ev:100:C%d", i), compute)
		})
	}
	// The first two take the class's slots and block; every later key arrives while the gate is full.
	fire(0)
	fire(1)
	<-started
	<-started
	for i := 2; i < n; i++ {
		fire(i)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("cold key %d shed under a burst within the bounded queue: %v", i, err)
		}
	}
	if got := maxInFlight.Load(); got > 2 {
		t.Fatalf("max concurrent computes = %d, want <= 2 (the contract_detail_ev class share)", got)
	}
}
