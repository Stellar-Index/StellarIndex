package v1

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ageLendingReserves moves every pool's reserves fill time back by d.
func ageLendingReserves(s *Server, d time.Duration) {
	c := s.lendingReserves
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.at = e.at.Add(-d)
	}
}

// A degraded pool build expires after lendingReservesDegradedTTL, so it does not hold the protocols build uncached for the full
// lendingReservesTTL; a good build keeps the full TTL. Expiry shows as the refresh it starts, not as flags.stale, which a
// healthy refresh inside the bound does not set.
func TestLendingReserves_DegradedBuildExpiresSooner(t *testing.T) {
	for name, tc := range map[string]struct {
		degraded    bool
		wantRefresh bool
	}{
		"degraded": {degraded: true, wantRefresh: true},
		"good":     {degraded: false, wantRefresh: false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{})
			var builds atomic.Int64
			build := func(context.Context) (poolReserves, error) {
				builds.Add(1)
				return poolReserves{flags: Flags{Degraded: tc.degraded}}, nil
			}
			if _, _, _, err := s.lendingReserves.get(context.Background(), "pool_reserves", "P", build); err != nil {
				t.Fatal(err)
			}
			ageLendingReserves(s, lendingReservesDegradedTTL+time.Second)
			if _, _, _, ok := s.lendingReserves.peek("pool_reserves", "P", build); !ok {
				t.Fatal("peek missed a filled entry")
			}
			refreshed := waitFor(time.Second, func() bool { return builds.Load() == 2 })
			if refreshed != tc.wantRefresh {
				t.Errorf("after %v: refresh started=%v (builds=%d), want %v", lendingReservesDegradedTTL+time.Second, refreshed, builds.Load(), tc.wantRefresh)
			}
		})
	}
}
