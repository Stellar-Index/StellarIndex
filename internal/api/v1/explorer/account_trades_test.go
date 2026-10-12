package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// blockingTradesReader blocks every ListAccountTrades call on `release`, records the peak of
// concurrently in-flight scans and signals each entry on `entered`. It proves the handler acquires
// accountTradesGate: a bounded handler never exceeds cap(accountTradesGate) scans in flight.
type blockingTradesReader struct {
	entered   chan struct{}
	release   chan struct{}
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (r *blockingTradesReader) ListAccountTrades(_ context.Context, _ string, _ int, _ timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, time.Time, error) {
	n := r.inFlight.Add(1)
	for {
		m := r.maxFlight.Load()
		if n <= m || r.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	r.entered <- struct{}{}
	<-r.release
	r.inFlight.Add(-1)
	return nil, time.Time{}, nil
}

// getAccountTradesFrom serves one /trades request from remoteAddr, the
// caller identity an anonymous request is keyed on.
func getAccountTradesFrom(h *Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/trades", nil)
	req.SetPathValue("g_strkey", validTestAccount)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	h.AccountTrades(w, req)
	return w
}
