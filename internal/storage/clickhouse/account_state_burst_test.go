package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func waitingIn(g *RefreshGate, class string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting[class]
}

// TestAccountStateCached_ColdBurstDrainsNotSheds: distinct cold accounts arriving while the class's slots are busy queue for a slot
// (up to the bounded queue) instead of answering ErrRefreshSaturated; the queue never admits past the class share.
func TestAccountStateCached_ColdBurstDrainsNotSheds(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
	}
	const class = "account_state"
	share := r.refreshGate.classCap(class)
	for range share {
		if !r.refreshGate.TryAcquireClass(class) {
			t.Fatal("setup: could not fill the class share")
		}
	}
	n := classWaitersPerSlot * share // the bounded queue, exactly full
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, _, err := r.AccountStateCached(context.Background(), fmt.Sprintf("GACCOUNT%d", i))
			errs <- err
		})
	}
	// Wait until every request is parked; a shed answers on errs first.
	deadline := time.Now().Add(5 * time.Second)
	for waitingIn(r.refreshGate, class) < n {
		select {
		case err := <-errs:
			t.Fatalf("a cold read answered while the class was full instead of queueing: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d cold reads queued", waitingIn(r.refreshGate, class), n)
		}
		runtime.Gosched()
	}
	for range share {
		r.refreshGate.ReleaseClass(class)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if errors.Is(err, ErrRefreshSaturated) {
			t.Fatalf("cold read shed within the bounded queue: %v", err)
		}
	}
}

// TestAccountStateCached_StaleSkipsAndMarksWhenSaturated: a stale entry never waits for a slot; with the class full it is served at
// once flagged RefreshFailed.
func TestAccountStateCached_StaleSkipsAndMarksWhenSaturated(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
	}
	for range r.refreshGate.classCap("account_state") {
		if !r.refreshGate.TryAcquireClass("account_state") {
			t.Fatal("setup: could not fill the class share")
		}
	}
	r.stateCache.put("GSTALE", AccountState{Exists: true, Balance: 1}, time.Now().Add(-time.Hour))
	st, stale, err := r.AccountStateCached(context.Background(), "GSTALE")
	if err != nil || !stale || !st.RefreshFailed {
		t.Fatalf("got (stale=%t, refreshFailed=%t, err=%v), want a stale serve flagged RefreshFailed", stale, st.RefreshFailed, err)
	}
}
