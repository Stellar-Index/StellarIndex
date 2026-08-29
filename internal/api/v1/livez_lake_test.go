package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// /v1/livez/lake is the ADR-0050 §7.3 lake-route LB probe. #266 gave it
// readyz's infra exemptions — no auth (middleware/auth.go
// isUnauthenticatedInfraPath) and no anonymous rate limit
// (middleware/ratelimit.go SkipHealthAndMetrics) — which is correct for
// a load-balancer probe but removes every brake an anonymous caller
// would otherwise hit. Two properties have to hold for that to be safe,
// and neither did before #310:
//
//  1. one lake query per ROUND, not per request (readyz's single-flight,
//     which the exemption rationale was copied from but the cache was
//     not), and
//  2. nothing internal in the public 503 body — the handler wrote
//     err.Error(), i.e. the ClickHouse endpoint, to any anonymous caller
//     exactly while the lake was in trouble.

// countingLakeCheck is a "clickhouse" ReadyChecker that counts pings and
// can flip its verdict mid-test. Ping sleeps briefly so concurrent
// probes genuinely overlap the first caller's round.
type countingLakeCheck struct {
	pings atomic.Int64
	sleep time.Duration

	mu  sync.Mutex
	err error
}

func (c *countingLakeCheck) Name() string   { return "clickhouse" }
func (c *countingLakeCheck) Critical() bool { return false }
func (c *countingLakeCheck) Ping(ctx context.Context) error {
	c.pings.Add(1)
	if c.sleep > 0 {
		select {
		case <-time.After(c.sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *countingLakeCheck) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// TestLivezLake_SingleFlightSharesOnePingPerRound — a burst of anonymous
// probes must cost ONE ClickHouse query, not one per request. Pre-#310
// every request ran its own LakeTipLedger under a 5s timeout with no
// auth and no rate limit in front of it, so N unmetered concurrent
// callers were N lake queries.
func TestLivezLake_SingleFlightSharesOnePingPerRound(t *testing.T) {
	const probes = 24
	check := &countingLakeCheck{sleep: 30 * time.Millisecond}
	ts := newTestServer(t, check)

	var wg sync.WaitGroup
	codes := make([]int, probes)
	for i := range probes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/v1/livez/lake")
			if err != nil {
				t.Errorf("probe %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			_, _ = readAll(resp)
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("probe %d: status = %d, want 200", i, code)
		}
	}
	// A follow-up probe inside the 1s window must also be served from
	// the shared round.
	resp, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("follow-up probe: %v", err)
	}
	defer resp.Body.Close()
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("cached body should still report ok: %s", body)
	}
	if got := check.pings.Load(); got != 1 {
		t.Errorf("clickhouse pings = %d after %d concurrent + 1 sequential probe, want 1 "+
			"(single-flight: an unauthenticated, rate-limit-exempt probe must not amplify onto the lake)", got, probes+1)
	}
}

// TestLivezLake_RoundRefreshesAfterTTL — the cache must not pin a stale
// verdict: once the 1s window passes, the next probe runs a fresh ping
// and a recovered lake flips 503 → 200. Companion to the single-flight
// test (that one alone could be satisfied by caching forever).
func TestLivezLake_RoundRefreshesAfterTTL(t *testing.T) {
	check := &countingLakeCheck{err: errors.New("dial tcp 10.9.9.9:9000: connect: connection refused")}
	ts := newTestServer(t, check)

	resp, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first probe status = %d, want 503", resp.StatusCode)
	}

	check.setErr(nil)
	// livezLakeTTL is 1s; wait it out plus slack.
	time.Sleep(1200 * time.Millisecond)

	resp2, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := readAll(resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status after TTL = %d, want 200 (recovered lake must not stay pinned 503): %s",
			resp2.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("body after TTL should report ok: %s", body)
	}
	if got := check.pings.Load(); got != 2 {
		t.Errorf("clickhouse pings = %d, want 2 (one per round)", got)
	}
}

// TestLivezLake_UnreadyBodyDoesNotEchoPingError — the 503 served to an
// anonymous caller carries the fixed operator hint, never the driver
// error. Pre-#310 the handler wrote err.Error() into data.detail, which
// on the real clickhouseChecker (LakeTipLedger) is a dial error naming
// the ClickHouse host:port — published, unauthenticated, precisely
// during a lake outage. The error itself must still reach the LOG:
// scrubbing the wire must not lose the operator's diagnosis.
func TestLivezLake_UnreadyBodyDoesNotEchoPingError(t *testing.T) {
	const pingErr = "dial tcp 10.9.9.9:9000: connect: connection refused"
	logger, logs := captureLogger()
	srv := v1.New(v1.Options{
		Logger:      logger,
		ReadyChecks: []v1.ReadyChecker{&countingLakeCheck{err: errors.New(pingErr)}},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("GET /v1/livez/lake: %v", err)
	}
	defer resp.Body.Close()
	body, _ := readAll(resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"lake-unready"`) {
		t.Errorf("body should report lake-unready: %s", body)
	}
	if strings.Contains(body, pingErr) || strings.Contains(body, "10.9.9.9") {
		t.Errorf("503 body leaked the ClickHouse ping error to an unauthenticated caller: %s", body)
	}
	if !strings.Contains(body, "clickhouse ping failed — see the API server log for the underlying error") {
		t.Errorf("503 body should carry the fixed operator hint: %s", body)
	}
	if !strings.Contains(logs.String(), pingErr) {
		t.Errorf("the underlying ping error must still be logged server-side; log:\n%s", logs.String())
	}
}

// TestLivezLake_AbsentLakeFailsClosed — a deployment with no ClickHouse
// checker wired must 503 (never route lake traffic here). The detail is
// a deployment statement, not an echoed error, so it stays.
func TestLivezLake_AbsentLakeFailsClosed(t *testing.T) {
	ts := newTestServer(t, &stubCheck{name: "postgres", critical: true})

	resp, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("GET /v1/livez/lake: %v", err)
	}
	defer resp.Body.Close()
	body, _ := readAll(resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"lake-absent"`) {
		t.Errorf("body should report lake-absent: %s", body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}
