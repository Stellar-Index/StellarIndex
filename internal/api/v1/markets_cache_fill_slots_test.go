package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCachedMarketsReader_DetachedScansAreCapped: abandoned cold reads of distinct cursors leave at most
// marketsFillSlots scans running; the next uncached page is refused, not queued, until a scan ends.
func TestCachedMarketsReader_DetachedScansAreCapped(t *testing.T) {
	hold := newUpstreamHold(1)
	t.Cleanup(hold.release)
	up := &swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}, hold: hold}
	c := NewCachedMarketsReader(up, time.Minute)
	f := timescale.PoolsFilter{Sources: []string{"aquarius"}}
	order := timescale.MarketsOrderVolume24hDesc

	for i := range marketsFillSlots + 4 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client has already gone; the scan it started has not
		_, _, _ = c.AllPools(ctx, f, fmt.Sprintf("cursor-%d", i), 50, order)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == marketsFillSlots }) {
		t.Fatalf("upstream scans in flight = %d, want %d", up.calls.Load(), marketsFillSlots)
	}

	_, _, err := c.AllPools(context.Background(), f, "cursor-new", 50, order)
	if !errors.Is(err, errMarketsFillBusy) {
		t.Fatalf("uncached page with every slot taken: err = %v, want errMarketsFillBusy", err)
	}
	if got := up.calls.Load(); got != marketsFillSlots {
		t.Fatalf("upstream scans = %d after the refused read, want %d", got, marketsFillSlots)
	}

	hold.release()
	if !waitFor(2*time.Second, func() bool { return len(c.fills) == 0 }) {
		t.Fatalf("fill slots still held after the scans ended: %d", len(c.fills))
	}
	if rows, _, err := c.AllPools(context.Background(), f, "cursor-new", 50, order); err != nil || len(rows) != 1 {
		t.Fatalf("after the scans ended: rows=%d err=%v, want the page", len(rows), err)
	}
}

// TestCachedMarketsReader_StalePageServedWhenNoSlot: an expired page is still served when no slot is free; its refresh
// starts on a later read.
func TestCachedMarketsReader_StalePageServedWhenNoSlot(t *testing.T) {
	up := &swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}}
	c := NewCachedMarketsReader(up, time.Minute)
	f := timescale.PoolsFilter{Sources: []string{"aquarius"}}
	order := timescale.MarketsOrderVolume24hDesc

	if _, _, err := c.AllPools(context.Background(), f, "", 50, order); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { return len(c.fills) == 0 }) {
		t.Fatal("cold fill did not return its slot")
	}
	expireMarketsEntries(c)
	for range marketsFillSlots {
		c.fills <- struct{}{}
	}

	rows, _, err := c.AllPools(context.Background(), f, "", 50, order)
	if err != nil || len(rows) != 1 || rows[0].TradeCount24h != swrPoolsStaleTrades {
		t.Fatalf("stale read with no slot: rows=%+v err=%v, want the stale row", rows, err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream scans = %d, want 1 (no refresh without a slot)", got)
	}

	<-c.fills
	if _, _, err := c.AllPools(context.Background(), f, "", 50, order); err != nil {
		t.Fatalf("stale read with a slot: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("upstream scans = %d, want the refresh to have started", up.calls.Load())
	}
}

func TestMarketsFillBusy_IsARetryable503(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/pools?cursor=x", nil)
	if !marketsFillBusy(rec, req, fmt.Errorf("wrapped: %w", errMarketsFillBusy)) {
		t.Fatal("busy error not recognised")
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d Retry-After=%q, want 503 and 5", rec.Code, rec.Header().Get("Retry-After"))
	}
	if marketsFillBusy(httptest.NewRecorder(), req, errors.New("db down")) {
		t.Fatal("an ordinary read error was answered as busy")
	}
}

// heldOracle answers its first LatestOracleStreams call and parks or fails later ones.
type heldOracle struct {
	panicOnceOracle
	n       atomic.Int64
	hold    *upstreamHold
	failGE2 atomic.Bool
}

func (o *heldOracle) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	n := o.n.Add(1)
	if err := o.hold.park(ctx, n); err != nil {
		return nil, err
	}
	if n >= 2 && o.failGE2.Load() {
		return nil, errors.New("oracle read failed")
	}
	return o.streams, nil
}

func expireRWAReferences(s *Server) {
	s.rwaRefMu.Lock()
	s.rwaRefAt = s.rwaRefAt.Add(-time.Hour)
	s.rwaRefTried = s.rwaRefTried.Add(-time.Hour)
	s.rwaRefMu.Unlock()
}

func rwaReferenceFlightIdle(s *Server) bool {
	s.rwaRefMu.Lock()
	defer s.rwaRefMu.Unlock()
	return s.rwaRefFlight == nil
}

// TestCachedRWAReferences_ExpiredSnapshotIsServedWhileOneRefillRuns: a slow refill must not turn every caller's
// references into "none published"; the cached snapshot is served and exactly one refill runs.
func TestCachedRWAReferences_ExpiredSnapshotIsServedWhileOneRefillRuns(t *testing.T) {
	hold := newUpstreamHold(2)
	t.Cleanup(hold.release)
	oracle := &heldOracle{hold: hold}
	oracle.streams = []canonical.OracleUpdate{refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "104000000", 8, time.Now())}
	s := &Server{Options: Options{Oracle: oracle}, logger: slog.Default()}

	if !s.cachedRWAReferences(context.Background()).available {
		t.Fatal("cold read got no snapshot")
	}
	expireRWAReferences(s)

	for i := range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		got := s.cachedRWAReferences(ctx)
		cancel()
		if !got.available {
			t.Fatalf("read %d during a slow refill got no snapshot", i)
		}
	}
	<-hold.entered
	if got := oracle.n.Load(); got != 2 {
		t.Fatalf("oracle reads = %d, want 2 (one refill for five callers)", got)
	}
}

// TestCachedRWAReferences_FailedRefillIsNotRestartedPerRequest: while the snapshot is served past its TTL, a failing
// read is retried on rwaReferenceRetryAfter, not once per request.
func TestCachedRWAReferences_FailedRefillIsNotRestartedPerRequest(t *testing.T) {
	oracle := &heldOracle{}
	oracle.streams = []canonical.OracleUpdate{refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "104000000", 8, time.Now())}
	s := &Server{Options: Options{Oracle: oracle}, logger: slog.Default()}

	s.cachedRWAReferences(context.Background())
	oracle.failGE2.Store(true)
	expireRWAReferences(s)

	for range 5 {
		if !s.cachedRWAReferences(context.Background()).available {
			t.Fatal("a failed refill dropped the served snapshot")
		}
		if !waitFor(2*time.Second, func() bool { return rwaReferenceFlightIdle(s) }) {
			t.Fatal("refill did not end")
		}
	}
	if got := oracle.n.Load(); got != 2 {
		t.Fatalf("oracle reads = %d, want 2 (one failed refill, then the retry interval)", got)
	}
}
