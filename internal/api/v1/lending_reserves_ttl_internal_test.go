package v1

import (
	"context"
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
// lendingReservesTTL; a good build keeps the full TTL.
func TestLendingReserves_DegradedBuildExpiresSooner(t *testing.T) {
	for name, tc := range map[string]struct {
		degraded  bool
		wantStale bool
	}{
		"degraded": {degraded: true, wantStale: true},
		"good":     {degraded: false, wantStale: false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{})
			build := func(context.Context) (poolReserves, error) {
				return poolReserves{flags: Flags{Degraded: tc.degraded}}, nil
			}
			if _, _, _, err := s.lendingReserves.get(context.Background(), "pool_reserves", "P", build); err != nil {
				t.Fatal(err)
			}
			ageLendingReserves(s, lendingReservesDegradedTTL+time.Second)
			if _, _, stale, ok := s.lendingReserves.peek("pool_reserves", "P", build); !ok || stale != tc.wantStale {
				t.Errorf("peek after %v: ok=%v stale=%v, want stale=%v", lendingReservesDegradedTTL+time.Second, ok, stale, tc.wantStale)
			}
		})
	}
}
