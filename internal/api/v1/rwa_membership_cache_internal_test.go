package v1

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

// `membership.built_at` is the membership set's own age; the envelope's `as_of` is the response instant and must not
// stand in for it. The tests pin that the field tracks the rebuild, not the request, and that a set past its lifetime
// says so.

func rwaAgeTestServer() *Server {
	return &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// built_at must date the REBUILD and not move when the set is read again. A field advancing on
// every read would be `as_of` under another name and would report a frozen cache as fresh.
func TestRWAMembershipSet_DatesTheRebuildNotTheRead(t *testing.T) {
	s := rwaAgeTestServer()
	built := time.Now().UTC().Add(-3 * time.Minute)
	s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
	s.rwaAt = built

	first := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if first == nil {
		t.Fatal("a built set published no date: the response cannot say how old the thing it describes is")
	}
	if got := first.BuiltAt.Time(); !got.Equal(built) {
		t.Errorf("built_at = %s, want the rebuild's instant %s", got, built)
	}

	time.Sleep(20 * time.Millisecond)
	second := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if !second.BuiltAt.Time().Equal(first.BuiltAt.Time()) {
		t.Errorf("built_at moved between two reads of ONE set (%s then %s) — "+
			"a field that advances per request is the envelope's as_of under "+
			"another name, and reports a frozen cache as continuously fresh",
			first.BuiltAt.Time(), second.BuiltAt.Time())
	}
	if first.Stale {
		t.Error("a set three minutes into a ten-minute lifetime reported itself stale")
	}
}

// The serve states, which are the point: a lapsed set is served either
// way, and only one of the two needs anybody to do anything.
func TestRWAMembershipSet_SeparatesARebuildComingFromNothingComing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		failedAgo   time.Duration // 0 = no failure recorded
		wantStale   bool
		wantFailure bool
		reading     string
	}{
		{
			name:    "inside its lifetime",
			age:     time.Minute,
			reading: "fresh: nothing to say and nothing to do",
		},
		{
			name:      "lapsed, a rebuild is coming",
			age:       rwaMembershipTTL + time.Minute,
			wantStale: true,
			reading: "expected and transient — the cache serves the lapsed set " +
				"rather than making a request wait for the rescan",
		},
		{
			name:        "lapsed, and nothing is replacing it",
			age:         rwaMembershipTTL + time.Minute,
			failedAgo:   30 * time.Second,
			wantStale:   true,
			wantFailure: true,
			reading:     "the last attempt to replace this set FAILED: the state to act on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rwaAgeTestServer()
			built := time.Now().UTC().Add(-tc.age)
			s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
			s.rwaAt = built
			// Gapped out, so the read under test serves rather than
			// kicking a rebuild that would move the clock underneath it.
			s.rwaAttemptAt = time.Now()
			if tc.failedAgo != 0 {
				s.rwaFailedAt = time.Now().UTC().Add(-tc.failedAgo)
			}

			got := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
			if got == nil {
				t.Fatalf("no date published; a reader cannot reach %q", tc.reading)
			}
			if got.Stale != tc.wantStale {
				t.Errorf("stale = %v, want %v — %s", got.Stale, tc.wantStale, tc.reading)
			}
			if (got.RebuildFailedAt != nil) != tc.wantFailure {
				t.Errorf("rebuild_failed_at present = %v, want %v — without it a set "+
					"nothing is replacing renders identically to one a rebuild is "+
					"about to replace, and only the first needs anybody to act",
					got.RebuildFailedAt != nil, tc.wantFailure)
			}
		})
	}
}

// A successful rebuild must CLEAR the failure marker. The field answers
// "is the latest thing to have happened a failure", not "has one ever
// happened" — an alarm that never clears is one nobody reads twice.
func TestRWAMembershipSet_ASuccessfulRebuildClearsTheFailure(t *testing.T) {
	s := rwaAgeTestServer()
	s.Sep1Cache = &stubAgeSep1Reader{}
	s.rwaFailedAt = time.Now().UTC().Add(-time.Minute)

	s.refreshRWAMembership(make(chan struct{}))

	set := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if set == nil {
		t.Fatal("the rebuild completed and left the set undated")
	}
	if set.RebuildFailedAt != nil {
		t.Errorf("a successful rebuild left rebuild_failed_at at %s: the marker "+
			"reports the LATEST outcome, not the worst one ever seen",
			set.RebuildFailedAt.Time())
	}
	if set.Stale {
		t.Error("a set built moments ago reported itself stale")
	}
	if set.BuiltAt.IsZero() {
		t.Error("the rebuild did not stamp the set with its own instant")
	}
}

// And a FAILED rebuild must record one, so the loud state is reachable
// from the path that actually produces it rather than only from a
// hand-set field.
func TestRWAMembershipSet_AFailedRebuildRecordsIt(t *testing.T) {
	s := rwaAgeTestServer()
	s.Sep1Cache = &stubAgeSep1Reader{err: context.DeadlineExceeded}
	built := time.Now().UTC().Add(-rwaMembershipTTL - time.Minute)
	s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
	s.rwaAt = built

	s.refreshRWAMembership(make(chan struct{}))
	s.rwaAttemptAt = time.Now() // gap the read below, not the rebuild above

	set := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if set == nil {
		t.Fatal("the last good set was served undated")
	}
	if !set.BuiltAt.Time().Equal(built) {
		t.Errorf("built_at = %s, want the LAST GOOD set's instant %s — a failed "+
			"rebuild must not re-date the set it failed to replace",
			set.BuiltAt.Time(), built)
	}
	if set.RebuildFailedAt == nil {
		t.Fatal("a failed rebuild published nothing: the set is lapsed, nothing " +
			"is replacing it, and the response says only that it is stale")
	}
	if !set.Stale {
		t.Error("the lapsed set did not report itself stale")
	}
}

// stubAgeSep1Reader makes a rebuild succeed or fail on demand; its set is empty because these tests only care about WHEN
// the set was built.
type stubAgeSep1Reader struct{ err error }

func (s *stubAgeSep1Reader) GetIssuerSep1Cached(
	context.Context, string,
) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (s *stubAgeSep1Reader) BoundSep1Currencies(
	context.Context, timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	if s.err != nil {
		return nil, timescale.Sep1BoundCensus{}, s.err
	}
	return nil, timescale.Sep1BoundCensus{}, nil
}

// A rebuild where one arm's read FAILED must not become the served set nor clear the failure stamp; caching on "either
// arm answered" would serve a half-empty set as fresh. Only wired-and-failed refuses a cache write, not an unwired arm
// ([TestRefreshRWAMembership_UnwiredArmStillCaches]).

// stubRWAContractArm answers the contract arm's directory scan. Empty
// by default: what matters here is that the arm ANSWERED, not what it
// found.
type stubRWAContractArm struct {
	entries []timescale.DirectoryEntry
	err     error
}

func (s *stubRWAContractArm) DirectoryRecognisedContracts(
	context.Context, []string,
) ([]timescale.DirectoryEntry, timescale.DirectoryRWACensus, error) {
	if s.err != nil {
		return nil, timescale.DirectoryRWACensus{}, s.err
	}
	return s.entries, timescale.DirectoryRWACensus{
		Entries:             len(s.entries),
		Contracts:           len(s.entries),
		ContractsRecognised: len(s.entries),
	}, nil
}

func (s *stubRWAContractArm) DirectoryRecognisedIssuersWithoutAsset(
	context.Context, []string, int,
) ([]timescale.DirectoryEntry, error) {
	return nil, nil
}

// unwiredSep1Reader satisfies [Sep1CachedReader] but not [Sep1BoundCurrencyReader]: a deployment that never wired the
// attestation scan, which is configuration rather than an outage.
type unwiredSep1Reader struct{}

func (unwiredSep1Reader) GetIssuerSep1Cached(
	context.Context, string,
) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

// seedDatedRWACache plants a last-good set that carries its own build
// instant, so the served set can be asked whether a failed rebuild
// re-dated it.
func seedDatedRWACache(s *Server, age time.Duration) time.Time {
	at := time.Now().UTC().Add(-age)
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	s.rwaCache = &rwaMembership{
		available: true,
		refusals:  map[string]int{},
		members: []rwaMember{{
			code:   "USTRY",
			issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		}},
		builtAt: at,
	}
	s.rwaAt = at
	return at
}

// The classic arm fails while the contract arm answers: the last good
// set stays, and the response says a rebuild failed.
func TestRefreshRWAMembership_FailedClassicArmDoesNotOverwrite(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	reader.mu.Lock()
	reader.err = context.DeadlineExceeded
	reader.mu.Unlock()
	s := rwaCacheTestServer(reader)
	s.RWAContracts = &stubRWAContractArm{}
	builtAt := seedDatedRWACache(s, rwaMembershipTTL+time.Minute)

	s.cachedRWAMembership(context.Background())
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	close(reader.release)
	waitForRWAFlight(t, s)

	after := s.cachedRWAMembership(context.Background())
	if len(after.members) != 1 || after.members[0].code != "USTRY" {
		t.Fatalf("a failed classic scan overwrote the served set with a half-empty one: %+v", after.members)
	}
	set := rwaMembershipSetOf(after)
	if set == nil {
		t.Fatal("the served set carries no build instant")
	}
	if got := time.Time(set.BuiltAt); !got.Equal(builtAt) {
		t.Fatalf("the served set was re-dated to %s; it was built at %s", got, builtAt)
	}
	if !set.Stale {
		t.Fatal("a set kept past its lifetime by a failed rebuild published stale=false")
	}
	if set.RebuildFailedAt == nil {
		t.Fatal("a failed rebuild published no rebuild_failed_at: " +
			"a lapsed set nothing is replacing reads as one about to be replaced")
	}
	if failed := time.Time(*set.RebuildFailedAt); time.Since(failed) > time.Minute {
		t.Fatalf("rebuild_failed_at is %s, not the failure that just happened", failed)
	}
}

// Mirror case: the contract directory scan fails while the classic scan answers with an empty set, and caching that
// would blank the served set just as surely.
func TestRefreshRWAMembership_FailedContractArmDoesNotOverwrite(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	s.RWAContracts = &stubRWAContractArm{err: context.DeadlineExceeded}
	seedDatedRWACache(s, rwaMembershipTTL+time.Minute)

	s.cachedRWAMembership(context.Background())
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	close(reader.release)
	waitForRWAFlight(t, s)

	after := s.cachedRWAMembership(context.Background())
	if len(after.members) != 1 || after.members[0].code != "USTRY" {
		t.Fatalf("a failed contract scan overwrote the served set: %+v", after.members)
	}
	set := rwaMembershipSetOf(after)
	if set == nil || set.RebuildFailedAt == nil {
		t.Fatalf("a failed contract scan published no rebuild_failed_at: %+v", set)
	}
}

// An arm that was never wired must still cache; treating it as a failure would rebuild on every request for a deployment
// with only one reader.
func TestRefreshRWAMembership_UnwiredArmStillCaches(t *testing.T) {
	s := rwaCacheTestServer(unwiredSep1Reader{})
	s.RWAContracts = &stubRWAContractArm{}

	s.PrewarmRWA(context.Background())

	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	if s.rwaCache == nil {
		t.Fatal("an unwired classic arm was treated as a failed one: nothing was cached")
	}
	if !s.rwaFailedAt.IsZero() {
		t.Fatal("an unwired arm stamped a rebuild failure: that is a configuration statement, not an outage")
	}
}

// The funnel must say the classic arm was not measured, as it does for the other two: its
// stages then read zero, and a narrowing of zeros would assert that no issuer publishes a
// real-world-asset attestation, a finding out of a scan that never ran.
func TestRWAFunnelBasis_NamesAnUnmeasuredClassicArm(t *testing.T) {
	const unmeasuredSaid = "`classic` arm was NOT MEASURED"

	measured := rwaFunnelOf(
		rwaMembership{available: true, refusals: map[string]int{}},
		rwaCatalogueJoin{}, 0, 0, 0, nil,
	)
	if strings.Contains(measured.Basis, unmeasuredSaid) {
		t.Fatal("a measured classic arm was reported as unmeasured")
	}

	unmeasured := rwaFunnelOf(
		rwaMembership{refusals: map[string]int{}},
		rwaCatalogueJoin{}, 0, 0, 0, nil,
	)
	if !strings.Contains(unmeasured.Basis, unmeasuredSaid) {
		t.Fatalf("an unmeasured classic arm is not named in the funnel basis: %s", unmeasured.Basis)
	}
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
