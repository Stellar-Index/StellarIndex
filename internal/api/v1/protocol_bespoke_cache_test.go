package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the §2.6b (2026-08-13) contract for the bespoke
// analytics block of /v1/protocols/{name}: a build whose battery is slow,
// failing, or starved keeps the page's visual suite (the last good block)
// instead of dropping it — the CCTP "missing visual suite" report — and
// only a TRUE first-ever miss may compute inline, bounded by its caller's
// context.

// bespokeStub is a countable, controllable ProtocolBespokeReader.
type bespokeStub struct {
	calls atomic.Int32
	fail  atomic.Bool
	block chan struct{} // when non-nil, every build waits for it

	mu   sync.Mutex
	keys []string // (source|category|window) triples, in call order
}

func (s *bespokeStub) BuildProtocolBespoke(_ context.Context, source, category string, windowDays int) (*timescale.BespokeBlock, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.keys = append(s.keys, protocolBespokeCacheKey(source, category, windowDays))
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	if s.fail.Load() {
		return nil, context.DeadlineExceeded // the production failure shape
	}
	return &timescale.BespokeBlock{
		Category: category,
		KPIs:     []timescale.BespokeKPI{{Label: "probe", Value: "1"}},
	}, nil
}

func (s *bespokeStub) builtKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// waitBespokeIdle waits for the detached bespoke build for key to finish.
func waitBespokeIdle(t *testing.T, s *Server, key string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.protocolBespokeCache.mu.Lock()
		_, up := s.protocolBespokeCache.flights[key]
		s.protocolBespokeCache.mu.Unlock()
		if !up {
			return
		}
		select {
		case <-deadline:
			t.Fatal("detached bespoke build never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func bespokeTestKey() string { return protocolBespokeCacheKey("cctp", "bridge", 90) }

func TestCachedBespoke_ColdBuildsOnceThenServesWithoutBlocking(t *testing.T) {
	stub := &bespokeStub{}
	srv := New(Options{ProtocolBespoke: stub})

	blk, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
	if !ok || stale || blk == nil {
		t.Fatalf("cold build: ok=%v stale=%v blk=%v, want a fresh block", ok, stale, blk)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("cold build ran %d batteries, want 1", got)
	}
	waitBespokeIdle(t, srv, bespokeTestKey())

	// Warm: the next build must be served from the cache IMMEDIATELY even
	// though the (unconditional) refresh it kicks is wedged — the whole
	// point of the last-good layer.
	stub.block = make(chan struct{})
	defer close(stub.block)
	done := make(chan struct{})
	go func() {
		defer close(done)
		blk, stale, ok = srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a warm read blocked on the wedged refresh — it must serve the cached block")
	}
	if !ok || stale || blk == nil {
		t.Errorf("warm read: ok=%v stale=%v blk=%v, want the cached block", ok, stale, blk)
	}
}

func TestCachedBespoke_FailedBuildKeepsLastGood(t *testing.T) {
	stub := &bespokeStub{}
	srv := New(Options{ProtocolBespoke: stub})
	if _, _, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); !ok {
		t.Fatal("cold build failed")
	}
	waitBespokeIdle(t, srv, bespokeTestKey())

	// Every subsequent battery dies at its deadline (the logged production
	// failure). The block must keep serving.
	stub.fail.Store(true)
	for range 3 {
		blk, _, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
		if !ok || blk == nil {
			t.Fatalf("failing battery dropped the block: ok=%v blk=%v", ok, blk)
		}
		waitBespokeIdle(t, srv, bespokeTestKey())
	}
	if e, has := srv.protocolBespokeCache.get(bespokeTestKey()); !has || e.blk == nil {
		t.Error("a failing refresh blanked the cached block — old-but-real must survive")
	}
}

// bespokeFakeClock is a stepped time source for the bespoke cache: age
// is a deterministic clock step, never a wall-clock race.
type bespokeFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *bespokeFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *bespokeFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ageBespokeEntry installs a fake clock on srv's bespoke cache, caches blk
// under key, and steps the clock one minute PAST the staleness horizon.
// It returns the clock so a test can step it further.
//
// The caller's stub must be BLOCKED (stub.block non-nil) for the read
// that follows: cachedBespoke kicks its detached refresh BEFORE it reads
// the entry, and an instant battery can re-put a fresh-stamped block
// between the two — on a loaded runner the aged entry was overwritten
// and served as FRESH (CI run 33185801510; 20/48000 under local CPU
// contention). Blocking the battery makes that interleaving impossible.
func ageBespokeEntry(t *testing.T, srv *Server, stub *bespokeStub, key string, blk *timescale.BespokeBlock) *bespokeFakeClock {
	t.Helper()
	if stub.block == nil {
		t.Fatal("ageBespokeEntry: stub.block must be set so the kicked refresh cannot overwrite the aged entry")
	}
	clk := &bespokeFakeClock{t: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	srv.protocolBespokeCache.now = clk.Now
	srv.protocolBespokeCache.put(key, blk)
	clk.Advance(bespokeStaleAfter + time.Minute)
	return clk
}

func TestCachedBespoke_StaleFlaggedPastHorizon(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolBespoke: stub})
	key := bespokeTestKey()
	clk := ageBespokeEntry(t, srv, stub, key, &timescale.BespokeBlock{Category: "bridge"})

	// Exactly AT the horizon is still fresh (the contract is strictly older).
	clk.Advance(-time.Minute)
	if _, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); !ok || stale {
		t.Fatalf("entry exactly at the horizon: ok=%v stale=%v, want fresh", ok, stale)
	}
	clk.Advance(time.Nanosecond)

	blk, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
	if !ok || blk == nil {
		t.Fatalf("aged entry not served: ok=%v blk=%v", ok, blk)
	}
	if !stale {
		t.Error("a block older than bespokeStaleAfter served as FRESH — staleness must be honest")
	}
	// Once the refresh lands, its block is stamped by the same clock and
	// the next read is fresh again.
	close(stub.block)
	waitBespokeIdle(t, srv, key)
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("two reads on one aged entry ran %d batteries, want the single in-flight refresh", got)
	}
	if _, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); !ok || stale {
		t.Errorf("after the refresh landed: ok=%v stale=%v, want fresh", ok, stale)
	}
}

func TestCachedBespoke_SingleFlightCollapsesColdCallers(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolBespoke: stub})

	const callers = 6
	var wg sync.WaitGroup
	oks := make([]bool, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, oks[i] = srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stub.block)
	wg.Wait()

	for i, ok := range oks {
		if !ok {
			t.Fatalf("caller %d got no block", i)
		}
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("%d concurrent cold callers ran %d batteries, want exactly 1", callers, got)
	}
}

// TestCachedBespoke_SaturatedGateSkipsRatherThanQueues pins the
// backpressure contract: a full class gate SKIPS the build (a cold key
// degrades exactly as it did before this cache, the next build re-kicks)
// and a cached key keeps serving.
func TestCachedBespoke_SaturatedGateSkipsRatherThanQueues(t *testing.T) {
	stub := &bespokeStub{}
	srv := New(Options{ProtocolBespoke: stub})
	gate := srv.protocolBespokeCache.refreshGate()
	held := 0
	for gate.TryAcquire() {
		held++
		if held > 1000 {
			t.Fatal("gate never saturates")
		}
	}

	if _, _, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); ok {
		t.Error("cold key under saturation returned a block, want an honest miss")
	}
	if got := stub.calls.Load(); got != 0 {
		t.Fatalf("saturated gate still ran %d batteries", got)
	}

	// A cached key is unaffected: the skipped refresh never touches it.
	srv.protocolBespokeCache.put(bespokeTestKey(), &timescale.BespokeBlock{Category: "bridge"})
	blk, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90)
	if !ok || stale || blk == nil {
		t.Errorf("cached key under saturation: ok=%v stale=%v blk=%v, want the cached block", ok, stale, blk)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("saturated gate ran %d batteries", got)
	}
}

func TestCachedBespoke_NilReaderDegradesAsBefore(t *testing.T) {
	srv := New(Options{})
	if blk, stale, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); ok || stale || blk != nil {
		t.Errorf("no reader wired: ok=%v stale=%v blk=%v, want the pre-cache degradation", ok, stale, blk)
	}
}

// TestBuildProtocolDetail_BespokeSurvivesFailingBattery is the regression
// guard for the grounding incident: once a page has built, a later
// rebuild whose bespoke battery blows its budget must NOT return a view
// without the suite ("protocol bespoke build failed … context deadline
// exceeded" → CCTP page missing its visual suite).
func TestBuildProtocolDetail_BespokeSurvivesFailingBattery(t *testing.T) {
	stub := &bespokeStub{}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	meta, ok := protocolByName("cctp")
	if !ok {
		t.Fatal("cctp missing from registry")
	}

	first := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if first.Bespoke == nil || first.Analytics.Status != protocolAnalyticsOK {
		t.Fatalf("first build: bespoke=%v status=%q, want a healthy suite", first.Bespoke, first.Analytics.Status)
	}
	waitBespokeIdle(t, srv, protocolBespokeCacheKey(meta.Name, meta.Category, protocolActivityWindowDays))

	stub.fail.Store(true)
	again := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if again.Bespoke == nil {
		t.Fatal("a failing battery dropped the bespoke block — the page must keep the last good suite")
	}
	if again.Analytics.Status != protocolAnalyticsOK {
		t.Errorf("status = %q, want %q: a served last-good block inside its horizon is not degradation",
			again.Analytics.Status, protocolAnalyticsOK)
	}
}

// TestBuildProtocolDetail_StaleBespokeIsHonestAndStillComplete: a block
// past the staleness horizon is still SERVED (complete page) but the view
// says so, and such an entry counts as healthy for cache displacement —
// it is not the blank state the guard defends against.
func TestBuildProtocolDetail_StaleBespokeIsHonestAndStillComplete(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	meta, _ := protocolByName("cctp")
	key := protocolBespokeCacheKey(meta.Name, meta.Category, protocolActivityWindowDays)
	ageBespokeEntry(t, srv, stub, key, &timescale.BespokeBlock{Category: meta.Category})

	view := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if view.Bespoke == nil {
		t.Fatal("stale block dropped, want it served")
	}
	if view.Analytics.Status != protocolAnalyticsStale {
		t.Errorf("status = %q, want %q", view.Analytics.Status, protocolAnalyticsStale)
	}
	if !protoDetailEntryHealthy(protoDetailEntry{view: view}) {
		t.Error("a COMPLETE page with a stale bespoke block counted as unhealthy — it would be pinned out of the cache")
	}
	close(stub.block)
	waitBespokeIdle(t, srv, key)
}

// TestHandleProtocolDetail_StaleBespokeSetsEnvelopeFlag pins the wire
// contract on the honesty half: analytics.status "stale" always travels
// with flags.stale, even when the DETAIL entry itself is freshly built —
// the age lives in the bespoke block, and a client must not read a
// last-good suite as live.
func TestHandleProtocolDetail_StaleBespokeSetsEnvelopeFlag(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	meta, _ := protocolByName("cctp")
	key := protocolBespokeCacheKey(meta.Name, meta.Category, protocolActivityWindowDays)
	ageBespokeEntry(t, srv, stub, key, &timescale.BespokeBlock{
		Category: meta.Category,
		KPIs:     []timescale.BespokeKPI{{Label: "probe", Value: "1"}},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code, env := getProtoDetail(t, ts.URL, "cctp")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if env.Data.Bespoke == nil {
		t.Fatal("stale bespoke block dropped — the suite must still render")
	}
	if env.Data.Analytics == nil || env.Data.Analytics.Status != protocolAnalyticsStale {
		t.Errorf("analytics = %+v, want status %q", env.Data.Analytics, protocolAnalyticsStale)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false with analytics.status=stale — the envelope must agree")
	}
	close(stub.block)
	waitBespokeIdle(t, srv, key)
}

// TestPrewarmProtocolDetails_WarmsTheBespokeKeysTheBuildReads is the
// arg-parity guard (feedback_prewarm_handler_drift): the sweep must warm
// exactly the (source, category, window) keys the build path reads — one
// construction site, no drift.
func TestPrewarmProtocolDetails_WarmsTheBespokeKeysTheBuildReads(t *testing.T) {
	oldPause := protocolDetailPrewarmPause
	protocolDetailPrewarmPause = 0
	defer func() { protocolDetailPrewarmPause = oldPause }()

	stub := &bespokeStub{}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	srv.PrewarmProtocolDetails(context.Background())

	want := map[string]bool{}
	for _, meta := range protocolRegistry {
		for _, w := range protocolBespokeWindows {
			key := protocolBespokeCacheKey(meta.Name, meta.Category, w)
			want[key] = true
			waitBespokeIdle(t, srv, key)
		}
	}
	for key := range want {
		if _, has := srv.protocolBespokeCache.get(key); !has {
			t.Errorf("sweep left bespoke key %q cold — prewarm and build disagree on the key", key)
		}
	}
	for _, key := range stub.builtKeys() {
		if !want[key] {
			t.Errorf("sweep built bespoke key %q, which no build path reads", key)
		}
	}
}

// TestCachedBespoke_ColdRespectsCallerDeadline: the ONLY inline wait is a
// true first-ever miss, and it is bounded by the caller's context (the
// build itself keeps running detached, so the next one lands warm).
func TestCachedBespoke_ColdRespectsCallerDeadline(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolBespoke: stub})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, ok := srv.cachedBespoke(ctx, "cctp", "bridge", 90)
	if ok {
		t.Error("cold miss returned a block despite the wedged battery")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cold miss waited %v, want to give up at the caller's deadline", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("ctx err = %v, want the deadline to be what ended the wait", ctx.Err())
	}
	close(stub.block)
	waitBespokeIdle(t, srv, bespokeTestKey())
	// The detached build survived the abandoned wait, so the next read is warm.
	if _, _, ok := srv.cachedBespoke(context.Background(), "cctp", "bridge", 90); !ok {
		t.Error("the detached build did not outlive the request — the retry must land warm")
	}
}
