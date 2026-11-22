package wiring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

type readyzCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// probeReadyz runs one /v1/readyz round on a fresh Server (the handler caches a round for 1 s of wall time).
func probeReadyz(t *testing.T, checks []v1.ReadyChecker) (int, string, map[string]readyzCheck) {
	t.Helper()
	srv := v1.New(v1.Options{ReadyChecks: checks})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))
	var env struct {
		Data struct {
			Status string        `json:"status"`
			Checks []readyzCheck `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode readyz body %q: %v", rec.Body.String(), err)
	}
	byName := map[string]readyzCheck{}
	for _, c := range env.Data.Checks {
		byName[c.Name] = c
	}
	return rec.Code, env.Data.Status, byName
}

func newOutageFixture(t *testing.T, dwell time.Duration) (*miniredis.Miniredis, *time.Time, []v1.ReadyChecker) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Unix(1_750_000_000, 0)
	clock := ratelimit.NewDwellClock(dwell, func() time.Time { return now })
	return mr, &now, []v1.ReadyChecker{RedisChecker{RDB: rdb}, RedisOutageChecker{RDB: rdb, Clock: clock}}
}

func TestRedisOutageChecker_DegradedInsideDwellUnreadyPastIt(t *testing.T) {
	mr, now, checks := newOutageFixture(t, 30*time.Second)
	mr.Close()

	code, status, byName := probeReadyz(t, checks)
	if code != http.StatusOK || status != "degraded" {
		t.Fatalf("inside the dwell: got %d %q, want 200 \"degraded\"", code, status)
	}
	if !byName["redis_outage"].OK || byName["redis"].OK {
		t.Fatalf("inside the dwell: want redis down and redis_outage ok, got %+v", byName)
	}

	*now = now.Add(31 * time.Second)
	code, status, byName = probeReadyz(t, checks)
	if code != http.StatusServiceUnavailable || status != "unready" {
		t.Fatalf("past the dwell: got %d %q, want 503 \"unready\"", code, status)
	}
	if got := byName["redis_outage"]; got.OK || got.Error != redisOutageDetail {
		t.Fatalf("past the dwell: redis_outage = %+v, want failed with %q", got, redisOutageDetail)
	}

	// A negative dwell disables the limiters' fail-closed inversion, so it must never drain either.
	mrNeg, nowNeg, negChecks := newOutageFixture(t, -1)
	mrNeg.Close()
	probeReadyz(t, negChecks)
	*nowNeg = nowNeg.Add(time.Hour)
	if code, status, _ := probeReadyz(t, negChecks); code != http.StatusOK || status != "degraded" {
		t.Fatalf("negative dwell: got %d %q, want 200 \"degraded\"", code, status)
	}
}

func TestRedisOutageChecker_RecoversWithoutTraffic(t *testing.T) {
	mr, now, checks := newOutageFixture(t, 30*time.Second)
	mr.Close()
	probeReadyz(t, checks)
	*now = now.Add(31 * time.Second)
	if code, _, _ := probeReadyz(t, checks); code != http.StatusServiceUnavailable {
		t.Fatalf("past the dwell: got %d, want 503", code)
	}

	// Only readiness pings reach the clock: no rate-limited request is made.
	if err := mr.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	*now = now.Add(15 * time.Second)
	code, status, byName := probeReadyz(t, checks)
	if code != http.StatusOK || status != "ok" {
		t.Fatalf("after Redis returns: got %d %q (%+v), want 200 \"ok\"", code, status, byName)
	}
}

func TestReadyChecks_NoOutageCheckerWithoutRedisLimiter(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	if got := RedisOutageReadyChecks(nil, true, 30*time.Second); len(got) != 0 {
		t.Errorf("no Redis: got %d checks, want 0 (limiters run in-process)", len(got))
	}
	if got := RedisOutageReadyChecks(rdb, false, 30*time.Second); len(got) != 0 {
		t.Errorf("no Redis-backed limiter: got %d checks, want 0 (a Redis loss closes no route)", len(got))
	}
	got := RedisOutageReadyChecks(rdb, true, 30*time.Second)
	if len(got) != 1 || got[0].Name() != "redis_outage" || !got[0].Critical() {
		t.Fatalf("Redis + limiter: got %v, want one critical redis_outage checker", got)
	}
}
