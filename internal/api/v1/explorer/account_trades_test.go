package explorer

import (
	"testing"
	"time"
)

// Pins that the route cannot take more than its share of the shared 25-connection Postgres pool: ListAccountTrades takes
// ~10s without a full page of recent trades (taker/maker aren't segmentby keys), so an anonymous trickle could hold it
// all.
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
