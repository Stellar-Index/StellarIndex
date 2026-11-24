package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The membership set served just past its TTL while its rebuild runs on time is clean; a failed previous rebuild or an
// overdue one flags it.
func TestRWAMembership_StaleFollowsTheSWRVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		age    time.Duration
		failed bool
		want   bool
	}{
		{"just past ttl, rebuild on time", rwaMembershipTTL + time.Minute, false, false},
		{"previous rebuild failed, retry running", rwaMembershipTTL + time.Minute, true, true},
		{"past the hard bound", rwaMembershipTTL + rwaMembershipBudget + time.Second, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newBlockingRWASep1Reader()
			s := rwaCacheTestServer(reader)
			seedRWACache(s, tc.age)
			if tc.failed {
				s.rwaFailedAt = time.Now().UTC().Add(-2 * rwaMembershipRetryGap)
			}
			m := s.cachedRWAMembership(context.Background())
			t.Cleanup(func() { close(reader.release); waitForRWAFlight(t, s) })
			if f := m.serveFlags(); m.servedStale != tc.want || f.Stale != tc.want {
				t.Errorf("servedStale = %v flags = %+v, want stale %v", m.servedStale, f, tc.want)
			}
		})
	}
}

// After a successful rebuild a set served just past its TTL with the next rebuild running is clean again.
func TestRWAMembership_ASuccessfulRebuildClearsTheStaleVerdict(t *testing.T) {
	s := rwaAgeTestServer()
	s.Sep1Cache = &stubAgeSep1Reader{}
	s.rwaFailedAt = time.Now().UTC().Add(-time.Minute)
	s.refreshRWAMembership(make(chan struct{}))

	s.rwaMu.Lock()
	s.rwaAt, s.rwaFlight = time.Now().Add(-(rwaMembershipTTL + time.Minute)), closedFlight()
	got := s.stampServeStateLocked(*s.rwaCache)
	s.rwaMu.Unlock()
	if got.servedStale {
		t.Error("a set whose last rebuild succeeded is flagged stale while the next one runs on time")
	}
}

type panickingSep1Reader struct{ stubAgeSep1Reader }

func (panickingSep1Reader) BoundSep1Currencies(context.Context, timescale.Sep1CurrencyFilter) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	panic("boom")
}

// A rebuild that panics is recorded as failed, so the set it leaves in place is flagged on the next serve.
func TestRWAMembership_APanickedRebuildIsAFailure(t *testing.T) {
	s := rwaAgeTestServer()
	s.Sep1Cache = &panickingSep1Reader{}
	s.refreshRWAMembership(make(chan struct{}))
	if s.rwaFailedAt.IsZero() {
		t.Error("a panicked rebuild left no failure behind")
	}
}

// The assets view follows the same verdict: clean just past expiry while its rebuild runs, flagged after a failed build or
// past the bound, clean again after a successful one.
func TestRWAAssetsView_StaleFollowsTheSWRVerdict(t *testing.T) {
	seed := func(overdue time.Duration, err error) *Server {
		s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.rwaViewCache = &rwaAssetsBuild{builtAt: time.Now().Add(-rwaAssetsViewTTL - overdue)}
		s.rwaViewExpires = time.Now().Add(-overdue)
		s.rwaViewErr = err
		s.rwaViewFlight = make(chan struct{}) // a rebuild already running, never finishing here
		return s
	}
	check := func(name string, s *Server, want bool) {
		t.Helper()
		b, err := s.cachedRWAAssetsViewOnce(context.Background(), false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b.flags.Stale != want || b.flags.Degraded != want {
			t.Errorf("%s: flags = %+v, want stale and degraded %v", name, b.flags, want)
		}
	}
	check("just past expiry, rebuild on time", seed(5*time.Second, nil), false)
	check("previous build failed, retry running", seed(5*time.Second, errors.New("down")), true)
	check("past the hard bound", seed(maxHandlerBudget+time.Second, nil), true)

	s := seed(5*time.Second, errors.New("down"))
	s.settleRWAAssetsView(rwaAssetsBuild{builtAt: time.Now()}, nil)
	s.rwaViewExpires, s.rwaViewFlight = time.Now().Add(-5*time.Second), make(chan struct{})
	check("after a successful build", s, false)
}
