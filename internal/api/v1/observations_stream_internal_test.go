package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// gatedObservationsReader counts LatestTradePerSource scans, tracks how many run at once, and holds each until gate closes.
type gatedObservationsReader struct {
	HistoryReader
	gate     chan struct{}
	started  chan struct{}
	calls    atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func newGatedObservationsReader() *gatedObservationsReader {
	return &gatedObservationsReader{gate: make(chan struct{}), started: make(chan struct{}, 1024)}
}

func (g *gatedObservationsReader) LatestTradePerSource(ctx context.Context, _ canonical.Pair, _ string) ([]canonical.Trade, error) {
	g.calls.Add(1)
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.started <- struct{}{}
	select {
	case <-g.gate:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func observationsTickServer(hist HistoryReader) *Server {
	return &Server{History: hist, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func observationsTestPair(t *testing.T, base string) canonical.Pair {
	t.Helper()
	p, err := canonical.NewPair(mustAsset(t, base), mustAsset(t, "fiat:USD"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// scansPerCompute is how many storage scans one compute of pair costs (one per alias spelling).
func scansPerCompute(t *testing.T, pair canonical.Pair) int64 {
	t.Helper()
	g := newGatedObservationsReader()
	close(g.gate)
	if _, err := observationsTickServer(g).computeObservations(context.Background(), pair, "", ""); err != nil {
		t.Fatalf("computeObservations: %v", err)
	}
	return g.calls.Load()
}

// Connection caps do not bound scan load when every connection scans on its own ticker: 50 subscribers of one pair
// must cost the database one compute per tick, not 50.
func TestObservationsStreamTick_SubscribersOfOneKeyShareAScan(t *testing.T) {
	pair := observationsTestPair(t, "crypto:BTC")
	perCompute := scansPerCompute(t, pair)
	g := newGatedObservationsReader()
	s := observationsTickServer(g)

	const subscribers = 50
	var wg sync.WaitGroup
	results := make([]observationsTick, subscribers)
	errs := make([]error, subscribers)
	for i := range subscribers {
		wg.Go(func() {
			results[i], errs[i] = s.observationsStreamTick(context.Background(), pair, "", "", 5)
		})
	}
	<-g.started
	time.Sleep(50 * time.Millisecond) // let every subscriber reach the shared flight
	close(g.gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
		if !results[i].at.Equal(results[0].at) {
			t.Errorf("subscriber %d as_of = %v, want the shared scan's %v", i, results[i].at, results[0].at)
		}
	}
	if got := g.calls.Load(); got != perCompute {
		t.Errorf("%d subscribers cost %d scans, want one compute (%d)", subscribers, got, perCompute)
	}

	// A tick landing right after reuses the result and reports the scan's own time.
	again, err := s.observationsStreamTick(context.Background(), pair, "", "", 5)
	if err != nil || !again.at.Equal(results[0].at) || g.calls.Load() != perCompute {
		t.Errorf("tick right after: err=%v at=%v scans=%d, want the shared result reused", err, again.at, g.calls.Load())
	}
}

// Distinct keys cannot be shared, so they queue for a fixed number of slots; a tick that gets none within its interval is skipped.
func TestObservationsStreamTick_DistinctKeysQueueForSlots(t *testing.T) {
	g := newGatedObservationsReader()
	s := observationsTickServer(g)
	s.observationsTicks.concurrency = 1
	s.streamSecondFor = 20 * time.Millisecond
	held := observationsTestPair(t, "crypto:BTC")
	perCompute := scansPerCompute(t, held)

	done := make(chan error, 1)
	go func() {
		_, err := s.observationsStreamTick(context.Background(), held, "", "", 5)
		done <- err
	}()
	<-g.started

	const others = 8
	bases := []string{"crypto:ETH", "crypto:SOL", "crypto:ADA", "crypto:DOT", "crypto:LTC", "crypto:XRP", "crypto:BNB", "crypto:TRX"}
	var wg sync.WaitGroup
	errs := make([]error, others)
	for i := range others {
		wg.Go(func() {
			_, errs[i] = s.observationsStreamTick(context.Background(), observationsTestPair(t, bases[i]), "", "", 1)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, errObservationsTickShed) {
			t.Errorf("key %s with the slot held: err = %v, want a shed tick", bases[i], err)
		}
	}
	if got := g.calls.Load(); got != perCompute {
		t.Errorf("scans with one slot held = %d, want only the holder's %d", got, perCompute)
	}
	if peak := g.peak.Load(); peak > perCompute {
		t.Errorf("peak concurrent scans = %d, want at most one compute (%d)", peak, perCompute)
	}

	close(g.gate)
	if err := <-done; err != nil {
		t.Fatalf("slot holder: %v", err)
	}
	if _, err := s.observationsStreamTick(context.Background(), observationsTestPair(t, bases[0]), "", "", 1); err != nil {
		t.Errorf("tick after the slot freed: %v", err)
	}
}

// The subscriber whose tick starts the scan may disconnect; the others sharing it must still get the result.
func TestObservationsStreamTick_LeaderDisconnectKeepsTheSharedScan(t *testing.T) {
	g := newGatedObservationsReader()
	s := observationsTickServer(g)
	pair := observationsTestPair(t, "crypto:BTC")

	leaderCtx, hangUp := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := s.observationsStreamTick(leaderCtx, pair, "", "", 5)
		leaderDone <- err
	}()
	<-g.started
	followerDone := make(chan error, 1)
	go func() {
		_, err := s.observationsStreamTick(context.Background(), pair, "", "", 5)
		followerDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	hangUp()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader after disconnect: err = %v, want context.Canceled", err)
	}
	close(g.gate)
	if err := <-followerDone; err != nil {
		t.Errorf("follower after the leader disconnected: %v", err)
	}
}

type panickingObservationsReader struct{ HistoryReader }

func (panickingObservationsReader) LatestTradePerSource(context.Context, canonical.Pair, string) ([]canonical.Trade, error) {
	panic("boom")
}

// The scan runs on goroutines no recoverer wraps; a panic there must become a skipped emit, not a dead process.
func TestObservationsStreamTick_ScanPanicIsAnError(t *testing.T) {
	s := observationsTickServer(panickingObservationsReader{})
	if _, err := s.observationsStreamTick(context.Background(), observationsTestPair(t, "crypto:BTC"), "", "", 1); err == nil {
		t.Fatal("a panicking scan returned no error")
	}
}
