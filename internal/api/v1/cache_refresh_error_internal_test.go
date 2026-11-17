package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Each cache below keeps serving its last good value when a refresh fails, so the failure must reach the
// stellarindex_api_cache_refresh_failing alert's counter or nothing pages for a value frozen indefinitely.
func refreshErrorDelta(t *testing.T, cache, op string, run func()) float64 {
	t.Helper()
	c := obs.APICacheOpsTotal.WithLabelValues(cache, op, "refresh_error")
	before := testutil.ToFloat64(c)
	run()
	return testutil.ToFloat64(c) - before
}

func TestProtocolRosterRefresh_FailureCountsRefreshError(t *testing.T) {
	stub := &rosterStub{}
	stub.failSource.Store("refresh-error-src")
	s := &Server{Options: Options{ProtocolContracts: stub}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	meta := ProtocolMeta{Name: "refresh-error-src"}
	s.protocolRosterCache.initLocked()
	s.protocolRosterCache.entries[meta.Name] = rosterEntry{count: 7, at: time.Now().Add(-2 * protocolRosterTTL)}

	got := refreshErrorDelta(t, "protocol_roster", "count", func() {
		count, ok := s.cachedRosterCount(context.Background(), meta)
		if !ok || count != 7 {
			t.Fatalf("stale read = %d, %v; want the last good 7", count, ok)
		}
		s.protocolRosterCache.mu.Lock()
		fl := s.protocolRosterCache.flights[meta.Name]
		s.protocolRosterCache.mu.Unlock()
		if fl != nil {
			<-fl.done
		}
	})
	if got != 1 {
		t.Fatalf("refresh_error delta = %v, want 1", got)
	}
}

func TestPoolTokensCache_FailedRefillCountsRefreshError(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	up := &scriptedPoolTokens{resp: map[string][]string{"CPOOL1": {"CA", "CB"}}}
	c := newPoolTokensCache(up)
	c.now = func() time.Time { return now }
	if _, err := c.PoolTokens(context.Background(), "comet"); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	now = now.Add(poolTokensTTL)
	up.err = errors.New("served tier down")

	got := refreshErrorDelta(t, "pool_tokens", "fill", func() {
		if m, err := c.PoolTokens(context.Background(), "comet"); err != nil || len(m) != 1 {
			t.Fatalf("stale read = %v, %v; want the last good map", m, err)
		}
	})
	if got != 1 {
		t.Fatalf("refresh_error delta = %v, want 1", got)
	}
}

type failingFixingLoads struct{ *countingFixings }

func (failingFixingLoads) LoadFXFixingWindow(context.Context, time.Duration, time.Time) ([]timescale.FXFixing, time.Time, error) {
	return nil, time.Time{}, errors.New("served tier down")
}

func TestFXFixingCache_FailedReloadCountsRefreshError(t *testing.T) {
	c := newFXFixingCache(failingFixingLoads{&countingFixings{}}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	c.loadedAt = time.Now().Add(-time.Hour)

	got := refreshErrorDelta(t, "fx_fixings", "load", func() {
		c.mu.Lock()
		c.refreshLocked()
		done := c.flight
		c.mu.Unlock()
		<-done
	})
	if got != 1 {
		t.Fatalf("refresh_error delta = %v, want 1", got)
	}
}

func TestRWAMembershipRebuild_FailureCountsRefreshError(t *testing.T) {
	s := rwaAgeTestServer()
	s.Sep1Cache = &stubAgeSep1Reader{err: context.DeadlineExceeded}
	built := time.Now().UTC().Add(-rwaMembershipTTL - time.Minute)
	s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
	s.rwaAt = built

	got := refreshErrorDelta(t, "rwa", "membership", func() { s.refreshRWAMembership(make(chan struct{})) })
	if got != 1 {
		t.Fatalf("refresh_error delta = %v, want 1", got)
	}
}
