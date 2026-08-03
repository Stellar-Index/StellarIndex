package clickhouse

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAccountStateCache(t *testing.T) {
	t.Parallel()
	c := newAccountStateCache()

	if _, ok, _ := c.get("G1"); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.put("G1", AccountState{Exists: true, Balance: 42}, time.Now())
	got, ok, fresh := c.get("G1")
	if !ok || !fresh || got.Balance != 42 {
		t.Fatalf("get after put = (%+v, %t, %t), want fresh balance-42 hit", got, ok, fresh)
	}

	// Expiry: a stale entry is STILL SERVED (ok=true) with fresh=false —
	// staleness is the caller's judgment (route-sweep 2026-07-30: a hard
	// miss past the 30s TTL kept whale accounts on a near-permanent 503).
	c.put("G2", AccountState{Exists: true, Balance: 7}, time.Now().Add(-2*AccountStateCacheTTL))
	got, ok, fresh = c.get("G2")
	if !ok || fresh || got.Balance != 7 {
		t.Errorf("expired entry = (%+v, %t, %t), want stale-but-served", got, ok, fresh)
	}
}

// TestAccountStateCacheBounded — the cache never exceeds its cap; the
// oldest entry is evicted on overflow.
func TestAccountStateCacheBounded(t *testing.T) {
	t.Parallel()
	c := newAccountStateCache()
	base := time.Now()
	for i := 0; i < accountStateCacheMax+50; i++ {
		// Stagger cachedAt so eviction is deterministic (oldest-first).
		c.put(string(rune(i)), AccountState{Balance: int64(i)}, base.Add(time.Duration(i)*time.Millisecond))
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > accountStateCacheMax {
		t.Errorf("cache holds %d entries, want <= %d", n, accountStateCacheMax)
	}
}

// TestAccountStateCacheNilSafe — a nil cache (zero-value reader in tests)
// is a permanent miss, never a panic.
func TestAccountStateCacheNilSafe(t *testing.T) {
	t.Parallel()
	var c *accountStateCache
	if _, ok, _ := c.get("G"); ok {
		t.Error("nil cache reported a hit")
	}
	c.put("G", AccountState{}, time.Now()) // must not panic
}

// TestAccountStateCached_SaturationReturnsDistinctSentinel pins recon-R3:
// when the shared detached-refresh gate is FULL, a cold-miss account-state
// read must return the DISTINCT, retryable ErrRefreshSaturated sentinel —
// NOT the generic errAccountStateRefreshFailed that a real scan failure
// returns — so the API handler can map the transient backpressure to a
// retryable 503 while a genuine failure stays a 500. The gate is pre-saturated
// here, so refreshAccountState's TryAcquire fails and no scan (which would need
// a live ClickHouse conn) ever runs.
//
// Red without the fix: pre-fix, refreshAccountState returned only the channel
// and the cold-miss path returned errAccountStateRefreshFailed regardless of
// why the cache stayed empty, so errors.Is(err, ErrRefreshSaturated) is false
// and this test fails.
func TestAccountStateCached_SaturationReturnsDistinctSentinel(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(1),
	}
	// Saturate the single gate slot so the cold-miss refresh is SKIPPED.
	if !r.refreshGate.TryAcquire() {
		t.Fatal("could not acquire the only gate slot to set up saturation")
	}

	st, stale, err := r.AccountStateCached(context.Background(),
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	if !errors.Is(err, ErrRefreshSaturated) {
		t.Fatalf("saturated cold miss err = %v, want ErrRefreshSaturated (recon-R3)", err)
	}
	// Must NOT masquerade as the generic scan-failure sentinel — that one
	// stays on the 500 path.
	if errors.Is(err, errAccountStateRefreshFailed) {
		t.Fatal("saturation error must be distinct from errAccountStateRefreshFailed, else it maps to 500")
	}
	if st.Exists || stale {
		t.Fatalf("saturated miss returned state=%+v stale=%t, want zero value + not-stale", st, stale)
	}
}

// A NON-OWNER waiter that joined a flight the owner then
// saturation-skipped must also see ErrRefreshSaturated: pre-fix it woke on
// the closed channel, found no cache entry, and fell through to the
// 500-class errAccountStateRefreshFailed for pure backpressure (cold audit
// 2026-08-03). The owner publishes the outcome on the flight entry before
// end() closes done.
func TestAccountStateCached_NonOwnerSeesSaturation(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(1),
	}
	if !r.refreshGate.TryAcquire() {
		t.Fatal("could not acquire the only gate slot to set up saturation")
	}
	const account = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	// Become the flight owner FIRST (simulating the non-owner's race
	// window: it joined between the owner's begin and end), then run the
	// cold-miss path as the non-owner and let the "owner" saturate-skip.
	fl, owner := r.stateFlight.begin(account)
	if !owner {
		t.Fatal("test setup: expected flight ownership")
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := r.AccountStateCached(context.Background(), account)
		done <- err
	}()
	// Give the non-owner a moment to join the flight, then end it the way
	// refreshAccountState's saturation path does.
	time.Sleep(10 * time.Millisecond)
	fl.saturated = true
	r.stateFlight.end(account, fl)

	err := <-done
	if !errors.Is(err, ErrRefreshSaturated) {
		t.Fatalf("non-owner err = %v, want ErrRefreshSaturated (backpressure must be retryable for every waiter)", err)
	}
}

func TestPerKeyFlight(t *testing.T) {
	t.Parallel()
	f := newPerKeyFlight()
	ch, owner := f.begin("k")
	if !owner {
		t.Fatal("first begin did not get ownership")
	}
	if _, o2 := f.begin("k"); o2 {
		t.Error("second begin for same key also got ownership")
	}
	// A different key is independent.
	if _, o3 := f.begin("other"); !o3 {
		t.Error("begin for a different key was denied ownership")
	}
	f.end("k", ch)
	if _, o4 := f.begin("k"); !o4 {
		t.Error("begin after end did not get ownership")
	}
}
