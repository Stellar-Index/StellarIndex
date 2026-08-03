package clickhouse

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the stale-while-revalidate contract of the archived-pair
// verdict cache (route-sweep 2026-07-29): the ttl-prefix classification
// scan must never run per request — a filled snapshot serves instantly
// (missing keys fail open as TTLUnknown), a stale/incomplete snapshot kicks
// exactly one detached recompute, and a cold cache waits for the detached
// compute bounded by the caller's deadline only.

func TestTTLLivenessCache_ColdFillsAndServes(t *testing.T) {
	var calls atomic.Int32
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		calls.Add(1)
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLLive
		}
		out["k-archived"] = TTLArchived
		return out, nil
	})

	got, err := c.resolve(context.Background(), []string{"k-live", "k-archived"})
	if err != nil {
		t.Fatalf("cold resolve: %v", err)
	}
	if got["k-live"] != TTLLive || got["k-archived"] != TTLArchived {
		t.Fatalf("cold resolve verdicts = %v", got)
	}
	// Warm hit: no recompute.
	if _, err := c.resolve(context.Background(), []string{"k-live"}); err != nil {
		t.Fatalf("warm resolve: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("compute ran %d times, want 1", n)
	}
}

func TestTTLLivenessCache_MissingKeyFailsOpenAndRefreshes(t *testing.T) {
	var calls atomic.Int32
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		calls.Add(1)
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLArchived
		}
		return out, nil
	})
	c.store(map[string]TTLLiveness{"old": TTLLive})

	// "new" is not in the snapshot: it must come back UNKNOWN (kept, never
	// dropped) while a detached recompute covering it is kicked.
	got, err := c.resolve(context.Background(), []string{"old", "new"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["old"] != TTLLive || got["new"] != TTLUnknown {
		t.Fatalf("verdicts = %v, want old=live new=unknown (fail-open)", got)
	}
	waitTTLFlightIdle(t, c)
	if n := calls.Load(); n != 1 {
		t.Fatalf("detached recompute ran %d times, want 1", n)
	}
	// The refreshed snapshot now covers "new".
	got, err = c.resolve(context.Background(), []string{"new"})
	if err != nil || got["new"] != TTLArchived {
		t.Errorf("post-refresh resolve = %v, %v; want new=archived", got, err)
	}
}

// A refresh kicked by a SUBSET caller (?pool= resolves one pair) must not
// evict the rest of the snapshot: store() whole-map-replaces, so before the
// union fix a single-pair refresh wiped every other pair's verdict and the
// archived-pair filter fell open (TTLUnknown→keep) for the whole registry —
// dead pools served as live liquidity (cold audit 2026-08-03).
func TestTTLLivenessCache_SubsetRefreshDoesNotEvictOtherKeys(t *testing.T) {
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			if k == "k-archived" {
				out[k] = TTLArchived
			} else {
				out[k] = TTLLive
			}
		}
		return out, nil
	})
	// Seed a full snapshot holding an archived pair, then age it past the
	// TTL so the next resolve kicks a refresh.
	c.store(map[string]TTLLiveness{"k-archived": TTLArchived, "k-live": TTLLive})
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-2 * TTLVerdictCacheTTL)
	c.mu.Unlock()

	// Subset resolve: the caller asks about ONE pair only.
	if _, err := c.resolve(context.Background(), []string{"k-live"}); err != nil {
		t.Fatalf("subset resolve: %v", err)
	}
	waitTTLFlightIdle(t, c)

	// The archived pair's verdict must survive the subset-kicked refresh.
	got, err := c.resolve(context.Background(), []string{"k-archived"})
	if err != nil {
		t.Fatalf("post-refresh resolve: %v", err)
	}
	if got["k-archived"] != TTLArchived {
		t.Fatalf("k-archived = %v after subset refresh, want TTLArchived (verdict evicted — archived pair now fails open as live)", got["k-archived"])
	}
}

func TestTTLLivenessCache_StaleServedWhileRevalidating(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		calls.Add(1)
		<-block
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLLive
		}
		return out, nil
	})
	c.store(map[string]TTLLiveness{"k": TTLArchived})
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-2 * TTLVerdictCacheTTL) // age it past the TTL
	c.mu.Unlock()

	// Stale snapshot: served IMMEDIATELY (no blocking on the slow compute).
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := c.resolve(context.Background(), []string{"k"})
		if err != nil || got["k"] != TTLArchived {
			t.Errorf("stale resolve = %v, %v; want the stale archived verdict", got, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale resolve blocked on the in-flight recompute")
	}
	close(block)
	waitTTLFlightIdle(t, c)
	if n := calls.Load(); n != 1 {
		t.Errorf("recompute ran %d times, want 1", n)
	}
}

func TestTTLLivenessCache_FailedRefreshKeepsSnapshotAndReportsErr(t *testing.T) {
	var sawErr atomic.Bool
	c := newTTLLivenessCache(func(context.Context, []string) (map[string]TTLLiveness, error) {
		return nil, errors.New("boom")
	})
	c.onErr = func(error) { sawErr.Store(true) }
	c.store(map[string]TTLLiveness{"k": TTLArchived})
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-2 * TTLVerdictCacheTTL)
	c.mu.Unlock()

	got, err := c.resolve(context.Background(), []string{"k"})
	if err != nil || got["k"] != TTLArchived {
		t.Fatalf("stale resolve under failing refresh = %v, %v", got, err)
	}
	waitTTLFlightIdle(t, c)
	if !sawErr.Load() {
		t.Error("refresh failure not reported to onErr")
	}
	// The snapshot survives the failed refresh.
	if got, err := c.resolve(context.Background(), []string{"k"}); err != nil || got["k"] != TTLArchived {
		t.Errorf("snapshot blanked by failed refresh: %v, %v", got, err)
	}
}

func TestTTLLivenessCache_ColdCallerDeadlineDoesNotKillFill(t *testing.T) {
	release := make(chan struct{})
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		<-release
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLArchived
		}
		return out, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.resolve(ctx, []string{"k"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold resolve under a short deadline = %v, want DeadlineExceeded", err)
	}
	// The detached compute outlives the dead caller and fills the cache.
	close(release)
	waitTTLFlightIdle(t, c)
	got, err := c.resolve(context.Background(), []string{"k"})
	if err != nil || got["k"] != TTLArchived {
		t.Errorf("retry after abandoned cold fill = %v, %v; want the landed verdict", got, err)
	}
}

func TestTTLLivenessCache_NilSafeFailsOpen(t *testing.T) {
	var c *ttlLivenessCache
	got, err := c.resolve(context.Background(), []string{"k"})
	if err != nil || got["k"] != TTLUnknown {
		t.Fatalf("nil cache resolve = %v, %v; want fail-open unknown", got, err)
	}
}

func waitTTLFlightIdle(t *testing.T, c *ttlLivenessCache) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		up := c.flight != nil
		c.mu.Unlock()
		if !up {
			return
		}
		select {
		case <-deadline:
			t.Fatal("ttl verdict refresh never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
