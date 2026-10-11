package explorer

import (
	"testing"
	"time"
)

// TestAccountTradesGate_ShedsRatherThanHoldingThePool pins that the route cannot occupy more than
// its share of the shared Postgres pool. ListAccountTrades takes ~10s for an account without a
// full page of recent trades (taker/maker are not compression segmentby keys), and the pool is
// 25 connections for the whole API, so a trickle the anonymous rate limiter permits could hold all
// of it. The gate is what bounds it.
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
