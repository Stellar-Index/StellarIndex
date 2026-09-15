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

// The membership cache is the whole of /rwa's latency, so these tests
// are about WHEN a request waits rather than about what it gets.
//
// The defect they pin is the one [assets_sep1_images_detached_test.go]
// pins for the logo map, on a surface that had the same shape and no
// test at all. The set is an indexed scan over every issuer-bound SEP-1
// payload (1.18M currency entries on r1) plus the curated-directory
// walk, ~11.5 s, behind a ten-minute TTL — and it was rebuilt INLINE on
// whichever request happened to find the entry expired. On r1
// (2026-09-15) that made the route perfectly bimodal: 39 of 43 requests
// under 1 s, the other 4 over 10 s. With no more than one page load per
// TTL window it is roughly one visitor in ten who waits twelve seconds
// for a page the other nine get in a third of a second.
//
// Like the logo-map tests, the assertions need no timing heuristic: the
// scan is held open for the whole test, so if the read returns at all
// it returned without waiting for the rebuild. Against the pre-fix code
// they do not fail on a threshold — they block until the outer timeout.

// blockingRWASep1Reader stands in for that scan: it announces that it
// started, blocks until the test releases it, counts how many scans a
// sequence of reads actually started, and records whether the context
// it was handed was cancelled while it worked.
//
// That last field is the point of detaching. A refresh that ran on the
// calling request's context would die when the caller gave up, and be
// restarted, unbounded, by the next caller.
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
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		sep1Cache: reader,
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

// The detached rebuild must not inherit the request context that
// happened to trigger it. If it does, the caller giving up kills the
// rebuild, and the entry stays cold for the next caller to try again.
func TestRefreshRWAMembership_DoesNotInheritCallerCancellation(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	seedRWACache(s, rwaMembershipTTL+time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	s.cachedRWAMembership(ctx)
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	cancel() // the caller gives up while the scan is still running

	// Give a context-inheriting rebuild time to die of it.
	time.Sleep(100 * time.Millisecond)
	if reader.sawCancel.Load() {
		t.Fatal("the rebuild inherited the caller's cancellation: " +
			"a request that gives up must not kill the rebuild behind it")
	}
	close(reader.release)
	waitForRWAFlight(t, s)
	if reader.sawCancel.Load() {
		t.Fatal("the rebuild saw the caller's cancellation")
	}
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

// A rebuild that fails must leave the last good set exactly as it was.
// Blanking it would empty the page on a transient read error, on a
// surface whose inputs move on a daily cadence.
func TestRefreshRWAMembership_KeepsLastGoodSetOnFailure(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	reader.mu.Lock()
	reader.err = context.DeadlineExceeded
	reader.mu.Unlock()
	s := rwaCacheTestServer(reader)
	seedRWACache(s, rwaMembershipTTL+time.Minute)

	if m := s.cachedRWAMembership(context.Background()); len(m.members) != 1 {
		t.Fatalf("stale read did not serve the last good set: %+v", m.members)
	}
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	close(reader.release)
	waitForRWAFlight(t, s)

	after := s.cachedRWAMembership(context.Background())
	if len(after.members) != 1 || after.members[0].code != "USTRY" {
		t.Fatalf("a failed rebuild blanked the served set: %+v", after.members)
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
	// No assets/history readers wired, so the two series arms return
	// early behind the same guards their handlers apply. The membership
	// arm — the ~11.5 s one, shared by all three RWA routes — is what
	// this asserts the prewarm waits for.

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
