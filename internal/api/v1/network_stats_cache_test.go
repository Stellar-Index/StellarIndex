package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// countingNetStatsUpstream returns a fixed value (or err) and counts calls; a non-nil gate holds each call until it closes.
type countingNetStatsUpstream struct {
	mu    sync.Mutex
	val   timescale.NetworkStats
	err   error
	gate  chan struct{}
	calls int
}

func (u *countingNetStatsUpstream) GetNetworkStats(_ context.Context) (timescale.NetworkStats, error) {
	u.mu.Lock()
	u.calls++
	gate := u.gate
	u.mu.Unlock()
	if gate != nil {
		<-gate
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.val, u.err
}

func (u *countingNetStatsUpstream) set(err error, gate chan struct{}) {
	u.mu.Lock()
	u.err, u.gate = err, gate
	u.mu.Unlock()
}

type netStatsServe struct {
	stale, degraded bool
	asOf            time.Time
	cacheControl    string
}

func getNetworkStats(t *testing.T, srv *Server) netStatsServe {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/network/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
		AsOf time.Time `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	// flags.degraded never reaches the wire; writeEnvelopeStatus turns it into no-store.
	cc := rec.Header().Get("Cache-Control")
	return netStatsServe{env.Flags.Stale, cc == "no-store", env.AsOf, cc}
}

// waitIdle waits for the cache's detached refresh to settle.
func waitIdle(t *testing.T, c *CachedNetworkStatsReader) {
	t.Helper()
	for range 200 {
		c.mu.Lock()
		idle := c.flight == nil
		c.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("refresh never settled")
}

// An entry just past its ttl, served while a healthy refresh runs, is within the bound: not stale, not degraded, cacheable,
// with its real fill time as as_of. Only a failed refresh or one past its fill budget makes the served copy stale.
func TestNetworkStats_SWRServeStaleOnlyPastBound(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	up := &countingNetStatsUpstream{val: timescale.NetworkStats{MarketsCount24h: 7}}
	c := NewCachedNetworkStatsReader(up, 30*time.Second)
	c.now = clk.now
	srv := New(Options{NetworkStats: c})

	filledAt := clk.now()
	if got := getNetworkStats(t, srv); got.stale || got.degraded {
		t.Fatalf("cold fill served %+v, want fresh", got)
	}

	gate := make(chan struct{})
	up.set(nil, gate)
	clk.advance(31 * time.Second)
	got := getNetworkStats(t, srv)
	if got.stale || got.degraded || got.cacheControl == "no-store" {
		t.Errorf("serve 1s past ttl with a refresh running = %+v, want not stale, not degraded, cacheable", got)
	}
	if !got.asOf.Equal(filledAt) {
		t.Errorf("as_of = %s, want the fill time %s", got.asOf, filledAt)
	}

	clk.advance(cacheFillBudget)
	if got := getNetworkStats(t, srv); !got.stale || !got.degraded || got.cacheControl != "no-store" {
		t.Errorf("serve with the refresh past its fill budget = %+v, want stale, degraded, no-store", got)
	}

	up.set(errors.New("upstream down"), nil)
	close(gate)
	waitIdle(t, c)
	up.set(nil, nil)
	if got := getNetworkStats(t, srv); !got.stale || !got.degraded || got.cacheControl != "no-store" {
		t.Errorf("last-good serve after a failed refresh = %+v, want stale, degraded, no-store", got)
	}
	waitIdle(t, c)
	if got := getNetworkStats(t, srv); got.stale || got.degraded {
		t.Errorf("serve after a successful refresh = %+v, want fresh", got)
	}
}
