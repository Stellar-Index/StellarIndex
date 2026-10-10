package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingTradesReader blocks every ListAccountTrades call on `release`,
// recording the peak number of concurrently in-flight scans and signalling
// each entry on `entered`. It is the instrument for proving the handler
// actually ACQUIRES accountTradesGate: a bounded handler can never
// have more than cap(accountTradesGate) scans in flight at once.
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
