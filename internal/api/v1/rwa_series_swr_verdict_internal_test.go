package v1

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// rwaSeriesServe is one /v1/rwa series cache under test: seed a cached assembly at age with its failure memory, settle a
// successful build, and serve through the real handler. The registered flight stands for a rebuild still running.
type rwaSeriesServe struct {
	ttl, budget time.Duration
	seed        func(s *Server, age time.Duration, failed bool)
	succeed     func(s *Server)
	serve       func(s *Server, rec *httptest.ResponseRecorder)
}

func rwaSeriesServes() map[string]rwaSeriesServe {
	return map[string]rwaSeriesServe{
		"history": {
			ttl: rwaHistoryTTL, budget: rwaHistoryBudget,
			seed: func(s *Server, age time.Duration, failed bool) {
				at := time.Now().Add(-age)
				s.rwaHistCache = &rwaValueHistory{available: true, builtAt: at, refreshFailed: failed}
				s.rwaHistAt, s.rwaHistFlight = at, closedFlight()
			},
			succeed: func(s *Server) { s.settleRWAValueHistory(rwaValueHistory{available: true, builtAt: time.Now()}, nil) },
			serve: func(s *Server, rec *httptest.ResponseRecorder) {
				s.handleRWAHistory(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/history", nil))
			},
		},
		"premium": {
			ttl: rwaPremiumHistoryTTL, budget: rwaPremiumHistoryBudget,
			seed: func(s *Server, age time.Duration, failed bool) {
				at := time.Now().Add(-age)
				s.rwaPremCache = &rwaPremiumHistory{available: true, builtAt: at, refreshFailed: failed}
				s.rwaPremAt, s.rwaPremFlight = at, closedFlight()
			},
			succeed: func(s *Server) {
				s.settleRWAPremiumHistory(rwaPremiumHistory{available: true, builtAt: time.Now()}, nil)
			},
			serve: func(s *Server, rec *httptest.ResponseRecorder) {
				s.handleRWAPremiumHistory(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/premium-history", nil))
			},
		},
	}
}

func serveRWASeries(t *testing.T, s *Server, c rwaSeriesServe) (stale, noStore bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.serve(s, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Flags Flags `json:"flags"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Flags.Stale, rec.Header().Get("Cache-Control") == "no-store"
}

// A series served just past its TTL while its rebuild runs on time is clean and cacheable; a failed rebuild, or one that has
// run past the budget bound, makes it a last-good copy; a successful build clears the failure.
func TestRWASeries_StaleFollowsTheSWRVerdict(t *testing.T) {
	for name, c := range rwaSeriesServes() {
		t.Run(name, func(t *testing.T) {
			newServer := func() *Server {
				return &Server{Options: Options{AssetsReader: nilAssets{}, OracleHistory: nilOracleHistory{}, MarketHistory: nilMarketHistory{}}}
			}
			justPast := c.ttl + c.budget/2
			for _, tc := range []struct {
				name   string
				age    time.Duration
				failed bool
				want   bool
			}{
				{"just past ttl, rebuild on time", justPast, false, false},
				{"previous rebuild failed, retry running", justPast, true, true},
				{"past the hard bound", c.ttl + c.budget + time.Second, false, true},
			} {
				s := newServer()
				c.seed(s, tc.age, tc.failed)
				if stale, noStore := serveRWASeries(t, s, c); stale != tc.want || noStore != tc.want {
					t.Errorf("%s: stale = %v no-store = %v, want both %v", tc.name, stale, noStore, tc.want)
				}
			}

			s := newServer()
			c.seed(s, justPast, true)
			c.succeed(s)
			// Age the fresh fill past its TTL again: only a cleared failure memory serves it clean.
			s.rwaHistAt, s.rwaPremAt = time.Now().Add(-justPast), time.Now().Add(-justPast)
			s.rwaHistFlight, s.rwaPremFlight = closedFlight(), closedFlight()
			if stale, noStore := serveRWASeries(t, s, c); stale || noStore {
				t.Errorf("after a successful build: stale = %v no-store = %v, want clean", stale, noStore)
			}
		})
	}
}

// A rebuild that panics counts as failed for the next serve.
func TestRWASeries_APanickedRebuildIsFlagged(t *testing.T) {
	s := &Server{Options: Options{AssetsReader: nilAssets{}, OracleHistory: nilOracleHistory{}}}
	c := rwaSeriesServes()["history"]
	c.seed(s, c.ttl+c.budget/2, false)
	runDetachedFill(slog.Default(), "test", time.Second, make(chan struct{}),
		func(_ context.Context) (rwaValueHistory, error) { panic("boom") }, s.settleRWAValueHistory)
	s.rwaHistFlight = closedFlight()
	if stale, noStore := serveRWASeries(t, s, c); !stale || !noStore {
		t.Errorf("after a panicked rebuild: stale = %v no-store = %v, want both true", stale, noStore)
	}
}
