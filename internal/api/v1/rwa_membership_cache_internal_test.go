package v1

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The membership cache is all of /rwa's latency, so these tests pin WHEN a request waits. Rebuilding the ~10 s scan
// inline on whichever request finds the entry expired makes the route bimodal (same defect as the logo map). No timing
// heuristic: the scan is held open all test, so a read that returns did not wait for the rebuild.

// blockingRWASep1Reader stands in for the scan: it announces start, blocks until released, counts scans, and records
// whether its context was cancelled. Detaching matters because a refresh on the caller's context would die when the
// caller gave up and be restarted, unbounded, by the next one.
type blockingRWASep1Reader struct {
	entered   chan struct{} // one send per call
	release   chan struct{} // test closes this to let the scan finish
	scans     atomic.Int32
	sawCancel atomic.Bool

	mu    sync.Mutex
	bound []timescale.Sep1BoundCurrency
	err   error
}

func newBlockingRWASep1Reader() *blockingRWASep1Reader {
	return &blockingRWASep1Reader{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingRWASep1Reader) GetIssuerSep1Cached(
	context.Context, string,
) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (b *blockingRWASep1Reader) BoundSep1Currencies(
	ctx context.Context, _ timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	b.scans.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		// Only reachable if the rebuild inherited a caller's cancellation.
		b.sawCancel.Store(true)
		return nil, timescale.Sep1BoundCensus{}, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, timescale.Sep1BoundCensus{}, b.err
	}
	return b.bound, timescale.Sep1BoundCensus{
		IssuersWithPayload: 1,
		IssuersDeclaring:   1,
		Entries:            len(b.bound),
		EntriesBound:       len(b.bound),
		EntriesKept:        len(b.bound),
	}, nil
}

func rwaCacheTestServer(reader Sep1CachedReader) *Server {
	return &Server{
		Options: Options{Sep1Cache: reader},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// seedRWACache plants a last-good set aged by `age`, as though a
// rebuild had completed that long ago.
func seedRWACache(s *Server, age time.Duration) {
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	s.rwaCache = &rwaMembership{
		available: true,
		refusals:  map[string]int{},
		members: []rwaMember{{
			code:   "USTRY",
			issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		}},
	}
	s.rwaAt = time.Now().Add(-age)
}

// waitForRWAFlight blocks until no rebuild is in flight.
func waitForRWAFlight(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.rwaMu.Lock()
		done := s.rwaFlight == nil
		s.rwaMu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rebuild never completed")
}

// A request that finds the entry STALE must be served the last good set
// at once, not held for the rebuild. This is the twelve-second page.
func TestCachedRWAMembership_RequestNeverBlocksOnRebuild(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	seedRWACache(s, rwaMembershipTTL+time.Minute)

	// The scan is never released, so returning at all proves the read
	// did not wait for it.
	m := s.cachedRWAMembership(context.Background())
	if len(m.members) != 1 || m.members[0].code != "USTRY" {
		t.Fatalf("stale read served the wrong set: %+v", m.members)
	}

	// And it DID kick the rebuild rather than merely skipping it —
	// serving a stale set forever is the opposite defect.
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("a stale read served the cached set but started no rebuild")
	}
	close(reader.release)
}

// A cache that has NEVER been filled is the one case that still waits.
// An empty set there is not a stale answer — it is the statement that
// no real-world asset exists on Stellar, which is false.
func TestCachedRWAMembership_WaitsWhenNeverBuilt(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)

	returned := make(chan rwaMembership, 1)
	go func() { returned <- s.cachedRWAMembership(context.Background()) }()

	select {
	case <-returned:
		t.Fatal("a cold cache returned before its first build finished: " +
			"an empty set here reads as 'no RWA exists', not as 'not yet known'")
	case <-time.After(250 * time.Millisecond):
	}

	close(reader.release)
	select {
	case m := <-returned:
		if !m.available {
			t.Fatal("the first build completed but its set was not served")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold read never returned after the build completed")
	}
}

// Consecutive stale reads must start ONE rebuild, not one each. Without
// the gap a failing scan becomes a scan storm against the same tables
// that are already struggling.
func TestCachedRWAMembership_GapsRebuildAttempts(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	seedRWACache(s, rwaMembershipTTL+time.Minute)

	for range 5 {
		s.cachedRWAMembership(context.Background())
	}
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	if n := reader.scans.Load(); n != 1 {
		t.Fatalf("five stale reads started %d rebuilds, want 1", n)
	}
	close(reader.release)
}

// The prewarm is what makes the waiter for a cold build a background
// goroutine rather than a visitor, so it has to actually WAIT. A
// prewarm that returns before the build lands warms nothing.
func TestPrewarmRWA_WaitsForTheBuild(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	// With no assets/history readers wired, the two series arms return early behind their handlers' guards; the ~11.5 s
	// membership arm, shared by all three RWA routes, is what the prewarm is asserted to wait for.

	done := make(chan struct{})
	go func() { defer close(done); s.PrewarmRWA(context.Background()) }()

	select {
	case <-done:
		t.Fatal("PrewarmRWA returned before the membership build finished")
	case <-time.After(250 * time.Millisecond):
	}
	close(reader.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PrewarmRWA never returned")
	}

	s.rwaMu.Lock()
	warm := s.rwaCache != nil
	s.rwaMu.Unlock()
	if !warm {
		t.Fatal("PrewarmRWA completed but left the membership cache cold")
	}
}
