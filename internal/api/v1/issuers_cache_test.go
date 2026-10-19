package v1

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeIssuersUpstream is the in-memory stub the cache wrapper sits
// in front of. Counts calls per method so tests can assert hit/miss
// behaviour without poking the Prometheus counter.
type fakeIssuersUpstream struct {
	listCalls   atomic.Int64
	lastLimit   atomic.Int64
	getCalls    atomic.Int64
	assetsCalls atomic.Int64

	listDelay time.Duration
	listErr   error

	rows []timescale.IssuerSummary
}

func (f *fakeIssuersUpstream) GetIssuer(_ context.Context, gStrkey string) (timescale.IssuerRow, error) {
	f.getCalls.Add(1)
	return timescale.IssuerRow{GStrkey: gStrkey}, nil
}

func (f *fakeIssuersUpstream) ListIssuerAssets(_ context.Context, _ string) ([]timescale.IssuerAsset, error) {
	f.assetsCalls.Add(1)
	return nil, nil
}

func (f *fakeIssuersUpstream) ListIssuers(ctx context.Context, limit int) ([]timescale.IssuerSummary, error) {
	f.listCalls.Add(1)
	f.lastLimit.Store(int64(limit))
	if f.listDelay > 0 {
		select {
		case <-time.After(f.listDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.rows != nil {
		return f.rows[:min(limit, len(f.rows))], nil
	}
	return []timescale.IssuerSummary{
		{GStrkey: "GA1", HomeDomain: "centre.io", AssetCount: 1, TotalObservationCount: 100},
	}, nil
}

// TestCachedIssuersReader_HitsCachedValue: once warmed, the upstream is not called again within the TTL (cache-aside), sparing
// the second request the 196ms+ HashAggregate scan.
func TestCachedIssuersReader_HitsCachedValue(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 5*time.Minute)
	for i := 0; i < 5; i++ {
		if _, err := c.ListIssuers(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	if got := up.listCalls.Load(); got != 1 {
		t.Errorf("upstream ListIssuers called %d times; want 1 (4 cache hits expected)", got)
	}
}

// TestCachedIssuersReader_RefetchesAfterTTL — after the TTL window
// the next call must hit upstream again.
func TestCachedIssuersReader_RefetchesAfterTTL(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 50*time.Millisecond)
	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return up.listCalls.Load() == 2 }) {
		t.Errorf("upstream ListIssuers called %d times; want 2 (one before + one refresh after TTL)", up.listCalls.Load())
	}
}

// An expired entry is served at once with its old fill time and stale=true while one background refresh runs; a failed
// refresh keeps the entry, so the 3-4 s scan never lands on a visitor once the key has filled.
func TestCachedIssuersReader_ServesStaleWhileRefreshing(t *testing.T) {
	c := NewCachedIssuersReader(nil, 20*time.Millisecond)
	ctx := context.Background()
	rows := func(g string) func(context.Context) ([]timescale.IssuerSummary, error) {
		return func(context.Context) ([]timescale.IssuerSummary, error) {
			return []timescale.IssuerSummary{{GStrkey: g}}, nil
		}
	}
	_, at0, stale, err := c.list.get(ctx, "list_issuers", "k", rows("OLD"))
	if err != nil || stale {
		t.Fatalf("cold fill: stale=%v err=%v", stale, err)
	}
	time.Sleep(30 * time.Millisecond)

	var calls atomic.Int64
	release := make(chan struct{})
	failing := func(context.Context) ([]timescale.IssuerSummary, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("db is down")
	}
	for i := range 2 {
		got, at, stale, err := c.list.get(ctx, "list_issuers", "k", failing)
		if err != nil || !stale || !at.Equal(at0) || len(got) != 1 || got[0].GStrkey != "OLD" {
			t.Fatalf("call %d: got=%+v at=%v stale=%v err=%v; want OLD at %v stale", i, got, at, stale, err, at0)
		}
	}
	close(release)
	flightDone := func() bool {
		c.list.mu.Lock()
		defer c.list.mu.Unlock()
		e := c.list.entries["k"]
		return e != nil && e.flight == nil
	}
	if !waitFor(time.Second, flightDone) {
		t.Fatal("failed refresh dropped the entry or never settled")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("refresh upstream called %d times; want 1", n)
	}

	if got, _, stale, _ := c.list.get(ctx, "list_issuers", "k", rows("NEW")); !stale || got[0].GStrkey != "OLD" {
		t.Fatalf("after failed refresh: got=%+v stale=%v; want OLD stale", got, stale)
	}
	if !waitFor(time.Second, flightDone) {
		t.Fatal("second refresh never settled")
	}
	got, at, stale, err := c.list.get(ctx, "list_issuers", "k", failing)
	if err != nil || stale || !at.After(at0) || got[0].GStrkey != "NEW" {
		t.Fatalf("after refresh: got=%+v at=%v stale=%v err=%v; want NEW fresh", got, at, stale, err)
	}
}

// TestCachedIssuersReader_SingleFlight: concurrent callers during a slow upstream refetch share ONE upstream call, protecting
// against a thundering herd on /v1/issuers after expiry.
func TestCachedIssuersReader_SingleFlight(t *testing.T) {
	up := &fakeIssuersUpstream{listDelay: 100 * time.Millisecond}
	c := NewCachedIssuersReader(up, 5*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ListIssuers(context.Background(), 100); err != nil {
				t.Errorf("call: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := up.listCalls.Load(); got != 1 {
		t.Errorf("upstream ListIssuers called %d times under single-flight; want 1", got)
	}
}

// TestCachedIssuersReader_LimitSweepSharesOneFill — every limit the
// handler accepts is served from one ceiling-sized fill, so sweeping
// `?limit=` cannot turn the cache into one upstream scan per value.
func TestCachedIssuersReader_LimitSweepSharesOneFill(t *testing.T) {
	rows := make([]timescale.IssuerSummary, IssuersListMaxLimit)
	for i := range rows {
		rows[i] = timescale.IssuerSummary{GStrkey: fmt.Sprintf("G%03d", i)}
	}
	up := &fakeIssuersUpstream{rows: rows}
	c := NewCachedIssuersReader(up, 5*time.Minute)

	for limit := 1; limit <= IssuersListMaxLimit; limit++ {
		got, err := c.ListIssuers(context.Background(), limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != limit || got[0].GStrkey != "G000" || got[limit-1].GStrkey != rows[limit-1].GStrkey {
			t.Fatalf("limit=%d: got %d rows, want the first %d of the ceiling page", limit, len(got), limit)
		}
	}
	if got := up.listCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times across a 1..%d limit sweep; want 1", got, IssuersListMaxLimit)
	}
	if got := up.lastLimit.Load(); got != IssuersListMaxLimit {
		t.Errorf("upstream asked for limit=%d; want the ceiling %d", got, IssuersListMaxLimit)
	}

	// A caller appending to its slice must not corrupt the shared entry.
	short, _ := c.ListIssuers(context.Background(), 1)
	_ = append(short, timescale.IssuerSummary{GStrkey: "INJECTED"})
	if again, _ := c.ListIssuers(context.Background(), 2); again[1].GStrkey != "G001" {
		t.Errorf("cached entry mutated through a caller's append: row 1 = %q", again[1].GStrkey)
	}
}

// TestCachedIssuersReader_TTLZeroIsBypass: ttl=0 disables the cache so every call hits upstream, matching the knob on
// CachedSourcesStatsReader/CachedAssetsReader used by integration tests.
func TestCachedIssuersReader_TTLZeroIsBypass(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 0)
	for i := 0; i < 3; i++ {
		_, _ = c.ListIssuers(context.Background(), 100)
	}
	if got := up.listCalls.Load(); got != 3 {
		t.Errorf("upstream called %d times; want 3 (no caching at ttl=0)", got)
	}
}

// TestCachedIssuersReader_ErrorIsNotCached — if upstream errors, the
// cache MUST forget so the next caller retries. Caching the error
// would freeze us into 500-loops after a transient PG hiccup.
func TestCachedIssuersReader_ErrorIsNotCached(t *testing.T) {
	up := &fakeIssuersUpstream{listErr: errors.New("db is down")}
	c := NewCachedIssuersReader(up, 5*time.Minute)

	if _, err := c.ListIssuers(context.Background(), 100); err == nil {
		t.Fatal("first call: want error, got nil")
	}
	up.listErr = nil
	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := up.listCalls.Load(); got != 2 {
		t.Errorf("upstream called %d times; want 2 (error not cached → retry)", got)
	}
}

// TestCachedIssuersReader_PassThroughGetIssuer — GetIssuer is
// pass-through (per-row PK lookup is already sub-ms, caching by
// g_strkey gives no shared-callers win). Every call hits upstream.
func TestCachedIssuersReader_PassThroughGetIssuer(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 5*time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := c.GetIssuer(context.Background(), "GA1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := up.getCalls.Load(); got != 3 {
		t.Errorf("GetIssuer called %d times; want 3 (pass-through)", got)
	}
}

// TestCachedIssuersReader_PassThroughListAssets — ListIssuerAssets
// is pass-through for the same reason as GetIssuer (indexed lookup,
// already cheap).
func TestCachedIssuersReader_PassThroughListAssets(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 5*time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := c.ListIssuerAssets(context.Background(), "GA1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := up.assetsCalls.Load(); got != 3 {
		t.Errorf("ListIssuerAssets called %d times; want 3 (pass-through)", got)
	}
}

// TestCachedIssuersReader_HitMissCounter pins the
// stellarindex_api_cache_ops_total{cache="issuers"} counter for list_issuers. If a refactor drops
// the .Inc() on either branch, the api_cache_miss_rate_high alert silently stops firing for
// /v1/issuers.
func TestCachedIssuersReader_HitMissCounter(t *testing.T) {
	up := &fakeIssuersUpstream{}
	c := NewCachedIssuersReader(up, 5*time.Minute)

	missBefore := readCacheCounter(t, "issuers", "list_issuers", "miss")
	hitBefore := readCacheCounter(t, "issuers", "list_issuers", "hit")

	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		t.Fatal(err)
	}

	if delta := readCacheCounter(t, "issuers", "list_issuers", "miss") - missBefore; delta != 1 {
		t.Errorf("miss counter delta = %v, want 1", delta)
	}
	if delta := readCacheCounter(t, "issuers", "list_issuers", "hit") - hitBefore; delta != 1 {
		t.Errorf("hit counter delta = %v, want 1", delta)
	}
}

// BenchmarkCachedIssuersReader_ListIssuers is an informal perf guard: a cached read should take
// microseconds against a 5ms upstream stub. Not gated in CI; run via `go test -bench .`.
func BenchmarkCachedIssuersReader_ListIssuers(b *testing.B) {
	up := &fakeIssuersUpstream{listDelay: 5 * time.Millisecond}
	c := NewCachedIssuersReader(up, 5*time.Minute)
	// Prime the cache so the loop measures hits, not the cold miss.
	if _, err := c.ListIssuers(context.Background(), 100); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ListIssuers(context.Background(), 100); err != nil {
			b.Fatal(err)
		}
	}
}

// TestScamIdentitySuppression: a flagged, UNVERIFIED issuer's self-declared org identity is the impersonation itself, so a
// counterfeiter with home_domain=lobstr.co must never render as "LOBSTR - SCAM".
func TestScamIdentitySuppression(t *testing.T) {
	// The known counterfeiter from the live incident.
	g := "GBYBVWOOVC4EJVRIF4HMWG5B7POLCS7JRPY5KYR3BCLEK24IJQOGUARD"
	if scamReason(g) == "" {
		t.Fatal("counterfeiter missing from the scam list — the suppression rule has nothing to key on")
	}
	// The rule under test lives inline in both handlers; assert its
	// inputs' invariant here: every scam-list entry has a non-empty
	// category-bearing reason so the UI can derive a badge label.
	for key, entry := range scamIssuers {
		if entry.Reason == "" {
			t.Errorf("scam entry %s has empty reason — badge derivation breaks", key)
		}
	}
}
