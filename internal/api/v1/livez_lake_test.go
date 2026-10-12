package v1_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// /v1/livez/lake is the ADR-0050 §7.3 lake LB probe, exempt from auth and anonymous rate limits, so it must run one lake
// query per round (single-flight) and keep internals out of the public 503 body (err.Error() would leak the ClickHouse
// endpoint).

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

// TestLivezLake_SingleFlightSharesOnePingPerRound — a burst of anonymous probes must cost ONE
// ClickHouse query, not one per request. Without single-flight, every request would run its
// own LakeTipLedger under a 5s timeout with no auth and no rate limit in front of it.
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

// TestLivezLake_RoundRefreshesAfterTTL: the cache must not pin a stale verdict; after the 1s window a fresh ping runs
// and a recovered lake flips 503 to 200.
func TestLivezLake_RoundRefreshesAfterTTL(t *testing.T) {
	check := &countingLakeCheck{err: errors.New("dial tcp 10.9.9.9:9000: connect: connection refused")}
	srv := v1.New(v1.Options{ReadyChecks: []v1.ReadyChecker{check}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/livez/lake")
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first probe status = %d, want 503", resp.StatusCode)
	}

	check.setErr(nil)
	// Age the round just past the TTL rather than sleeping it out.
	srv.AgeLivezLakeCacheForTest(v1.LivezLakeTTL + time.Millisecond)

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

// TestLivezLake_UnreadyBodyDoesNotEchoPingError: the anonymous 503 carries a fixed operator hint, never the driver error
// (a dial error naming the ClickHouse host:port), while the error still reaches the log.
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
