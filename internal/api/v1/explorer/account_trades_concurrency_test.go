package explorer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
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

// TestAccountTrades_GateBoundsConcurrentScans pins that the handler must
// acquire accountTradesGate before the expensive per-account scan, so that no
// more than cap(accountTradesGate) scans ever run at once.
//
// It fires cap+1 concurrent requests against a reader that blocks until
// released. With the gate acquired, exactly `cap` reach the reader and the
// extra parks in the gate select; peak in-flight is cap. Against the un-fixed
// handler (gate declared but never acquired) all cap+1 reach the reader at
// once and peak in-flight is cap+1 — this test fails RED.
func TestAccountTrades_GateBoundsConcurrentScans(t *testing.T) {
	gateCap := cap(accountTradesGate)
	if gateCap == 0 {
		t.Fatalf("accountTradesGate is unbuffered")
	}
	total := gateCap + 1

	reader := &blockingTradesReader{
		entered: make(chan struct{}, total),
		release: make(chan struct{}),
	}
	h := newProbeHandler(nil, nil)
	h.Trades = reader

	var wg sync.WaitGroup
	codes := make([]int, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct callers: the per-caller cap must not mask the global one.
			w := getAccountTradesFrom(h, fmt.Sprintf("198.51.100.%d:1234", i+1))
			codes[i] = w.Code
		}(i)
	}

	// Wait until the gate is saturated: gateCap scans have entered the reader.
	for i := 0; i < gateCap; i++ {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d scans entered the reader — deadlock?", i, gateCap)
		}
	}

	// Give the extra request a beat to reach — and, when the gate is
	// acquired, PARK IN — the gate select rather than the reader. This sleep
	// stays well inside accountTradesGateWait (%v) so the extra does not shed;
	// it is served once a slot frees below.
	time.Sleep(150 * time.Millisecond)
	if got := reader.maxFlight.Load(); got > int32(gateCap) {
		t.Errorf("peak concurrent trades scans = %d, want <= gate cap %d — the gate is not acquired (unbounded scan)",
			got, gateCap)
	}

	// Release the in-flight scans; the parked request now takes a freed slot.
	close(reader.release)
	wg.Wait()

	if got := reader.maxFlight.Load(); got != int32(gateCap) {
		t.Errorf("peak concurrent trades scans = %d, want exactly gate cap %d", got, gateCap)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d served %d, want 200 (it should get a slot within the gate wait, not shed)", i, c)
		}
	}
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

// TestAccountTrades_OneScanPerCaller pins that one caller cannot hold more
// than one gate slot: its second concurrent scan sheds at once without
// taking a slot, while another caller is still admitted.
func TestAccountTrades_OneScanPerCaller(t *testing.T) {
	reader := &blockingTradesReader{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	h := newProbeHandler(nil, nil)
	h.Trades = reader

	var wg sync.WaitGroup
	codes := make(map[string]int)
	var mu sync.Mutex
	serve := func(name, addr string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := getAccountTradesFrom(h, addr)
			mu.Lock()
			codes[name] = w.Code
			mu.Unlock()
		}()
	}
	awaitEntry := func(name string) {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never reached the reader", name)
		}
	}

	serve("a1", "198.51.100.1:1111")
	awaitEntry("a1")

	// A's second scan must shed at once; run it aside so a handler that
	// admits it (and parks in the reader) fails rather than hangs.
	second := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		second <- getAccountTradesFrom(h, "198.51.100.1:2222").Code
	}()
	select {
	case code := <-second:
		if code != http.StatusServiceUnavailable {
			t.Errorf("caller A's second concurrent scan served %d, want 503 shed", code)
		}
	case <-time.After(accountTradesGateWait):
		t.Errorf("caller A's second concurrent scan was admitted, want an immediate 503 shed")
	}
	if got := len(accountTradesGate); got != 1 {
		t.Errorf("gate slots held = %d after the per-caller shed, want 1 (only A's first scan)", got)
	}

	serve("b1", "203.0.113.7:1111")
	awaitEntry("b1")

	close(reader.release)
	wg.Wait()
	for _, name := range []string{"a1", "b1"} {
		if codes[name] != http.StatusOK {
			t.Errorf("%s served %d, want 200", name, codes[name])
		}
	}
	if w := getAccountTradesFrom(h, "198.51.100.1:3333"); w.Code != http.StatusOK {
		t.Errorf("caller A after its scan completed served %d, want 200 (slot not released)", w.Code)
	}
}
