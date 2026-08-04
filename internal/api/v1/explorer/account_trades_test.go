package explorer

import (
	"testing"
	"time"
)

// TestAccountTradesGate_ShedsRatherThanHoldingThePool pins that the route
// cannot occupy more than its share of the shared Postgres pool.
//
// ListAccountTrades is 9.5-11.1s on r1 for any account without a full page
// of recent trades (taker/maker are not compression segmentby keys, so the
// partial indexes exist on 2 of 249 chunks). PoolMaxOpenConns is 25 for the
// whole API, so ~3.1 req/s of that would hold all of it — 3% of what the
// anonymous rate limiter permits, i.e. the limiter cannot see it. The gate
// is what bounds it (cold audit 2026-08-04).
func TestAccountTradesGate_ShedsRatherThanHoldingThePool(t *testing.T) {
	if cap(accountTradesGate) == 0 {
		t.Fatal("accountTradesGate is unbuffered — every request would serialise")
	}
	if cap(accountTradesGate) >= 25 {
		t.Errorf("gate cap %d does not bound the 25-connection serving pool",
			cap(accountTradesGate))
	}
	// Fill it, then prove a further acquire sheds within the wait rather
	// than blocking for the read deadline.
	for i := 0; i < cap(accountTradesGate); i++ {
		accountTradesGate <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(accountTradesGate); i++ {
			<-accountTradesGate
		}
	}()

	start := time.Now()
	select {
	case accountTradesGate <- struct{}{}:
		<-accountTradesGate
		t.Fatal("acquired a slot from a full gate")
	case <-time.After(accountTradesGateWait):
	}
	if elapsed := time.Since(start); elapsed >= explorerReadTimeout {
		t.Errorf("shed after %v — must shed well inside the %v read deadline",
			elapsed, explorerReadTimeout)
	}
}
