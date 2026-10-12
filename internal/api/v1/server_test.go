package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// stubCheck is a ReadyChecker returning a configurable error. An optional `flight` joins probes to a rendezvous so
// tests can count probes in flight (TestReadyz_ProbesRunInParallel). `critical` defaults true; false exercises the
// degraded-but-serving path.
type stubCheck struct {
	name     string
	err      error
	flight   *probeFlight
	critical bool
}

func (s *stubCheck) Ping(ctx context.Context) error {
	if s.flight != nil {
		if err := s.flight.arrive(ctx); err != nil {
			return err
		}
	}
	return s.err
}
func (s *stubCheck) Name() string   { return s.name }
func (s *stubCheck) Critical() bool { return s.critical }

// probeRendezvousBailout bounds how long one probe waits for siblings that never come. Only a
// serialised handler reaches it, so it is a hang guard, not a budget. It sits above
// computeReadyz's 2 s shared deadline, which cancels a stuck round first.
const probeRendezvousBailout = 5 * time.Second

// probeFlight is the rendezvous the readiness stubs share: each probe reports itself in flight
// and waits until `width` are in flight together, so the peak is forced rather than raced past.
// maxFlight is what a test asserts on.
type probeFlight struct {
	width     int
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	gate      chan struct{}
	opened    sync.Once
}

func newProbeFlight(width int) *probeFlight {
	return &probeFlight{width: width, gate: make(chan struct{})}
}

// arrive records one probe in flight and blocks until every sibling is beside it. A handler that
// pings checkers sequentially never assembles them, so the first waits alone and the peak is 1.
func (f *probeFlight) arrive(ctx context.Context) error {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxFlight.Load()
		if n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	if int(n) >= f.width {
		f.opened.Do(func() { close(f.gate) })
	}
	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(probeRendezvousBailout):
		return errors.New("probe waited alone at the rendezvous")
	}
}

func newTestServer(t *testing.T, checks ...v1.ReadyChecker) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{ReadyChecks: checks})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	var env struct {
		Data struct {
			Status string `json:"status"`
			Uptime string `json:"uptime"`
		} `json:"data"`
		Flags map[string]bool `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Status != "ok" {
		t.Errorf("status = %q", env.Data.Status)
	}
	if env.Data.Uptime == "" {
		t.Error("uptime should be non-empty")
	}
}

func TestReadyz_AllChecksPass(t *testing.T) {
	ts := newTestServer(t,
		&stubCheck{name: "postgres", critical: true},
		&stubCheck{name: "redis", critical: false},
	)
	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatalf("GET /v1/readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
			Checks []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"checks"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data.Status != "ok" {
		t.Errorf("status = %q", env.Data.Status)
	}
	if len(env.Data.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(env.Data.Checks))
	}
	for _, c := range env.Data.Checks {
		if !c.OK {
			t.Errorf("check %s should be OK", c.Name)
		}
	}
}

// TestReadyz_CriticalFailureReturns503: a failing critical check (Postgres) returns 503 with status="unready" and
// the stale flag. Only critical failures take a backend out of HAProxy's pool.
func TestReadyz_CriticalFailureReturns503(t *testing.T) {
	ts := newTestServer(t,
		&stubCheck{name: "postgres", critical: true, err: errors.New("postgres: connection refused")},
		&stubCheck{name: "redis", critical: false},
	)
	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatalf("GET /v1/readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"status":"unready"`) {
		t.Errorf("body should report unready when critical check fails: %s", body)
	}
	assertReadyzErrorRedacted(t, body, "postgres", "postgres: connection refused")
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("body should set stale flag: %s", body)
	}
}

// TestReadyz_NonCriticalFailureReturns200Degraded pins that a Redis-only failure yields 200 with
// status="degraded". A 503 would make HAProxy drain every healthy API backend during a Redis
// outage even though the Timescale fallback keeps the surface serving.
func TestReadyz_NonCriticalFailureReturns200Degraded(t *testing.T) {
	ts := newTestServer(t,
		&stubCheck{name: "postgres", critical: true},
		&stubCheck{name: "redis", critical: false, err: errors.New("redis: connection refused")},
	)
	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatalf("GET /v1/readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (non-critical failure must keep backend in HAProxy pool)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"status":"degraded"`) {
		t.Errorf("body should report degraded: %s", body)
	}
	assertReadyzErrorRedacted(t, body, "redis", "redis: connection refused")
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("body should set stale flag: %s", body)
	}
}

// TestReadyz_ProbesRunInParallel pins computeReadyz's fan-out: all ReadyCheckers ping at once, so a round costs the slowest
// dependency, not the sum (three serial 500 ms checks would eat 1.5 s of 2 s, past the ~1 s LB probe timeout). Asserts peak
// probes in flight (stubs wait for siblings), not wall clock, so it catches partial fan-out and does not flake.
func TestReadyz_ProbesRunInParallel(t *testing.T) {
	const probes = 3
	flight := newProbeFlight(probes)
	ts := newTestServer(t,
		&stubCheck{name: "a", critical: true, flight: flight},
		&stubCheck{name: "b", critical: true, flight: flight},
		&stubCheck{name: "c", critical: true, flight: flight},
	)

	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := flight.maxFlight.Load(); got != probes {
		t.Fatalf("peak probes in flight = %d, want %d — readyz did not hold every checker in flight at once, so a round costs more than its slowest dependency", got, probes)
	}
	// Every probe returns nil once the rendezvous clears, so a
	// non-200 here is the round failing for some reason other than
	// the fan-out.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestVersion(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/version")
	if err != nil {
		t.Fatalf("GET /v1/version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data map[string]string `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data["version"] == "" {
		t.Error("version should be non-empty")
	}
	if env.Data["build_date"] == "" {
		t.Error("build_date should be non-empty")
	}
	// Commit + Dirty come from runtime/debug.BuildInfo: typically "unknown" under `go test` (no -buildvcs), populated
	// under `go build -buildvcs=true`. Either way the keys must be present and non-empty.
	if env.Data["commit"] == "" {
		t.Error("commit should be non-empty (defaults to 'unknown' if VCS unavailable)")
	}
	if env.Data["dirty"] == "" {
		t.Error("dirty should be non-empty")
	}
	if env.Data["go_version"] == "" {
		t.Error("go_version should be non-empty (runtime.Version())")
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/nonsense")
	if err != nil {
		t.Fatalf("GET /v1/nonsense: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	var p struct {
		Type, Title, Detail, Instance string
		Status                        int
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("body not problem+json: %v", err)
	}
	if p.Status != 404 {
		t.Errorf("status field = %d, want 404", p.Status)
	}
	if p.Title != "Not found" {
		t.Errorf("title = %q, want %q", p.Title, "Not found")
	}
	if p.Instance != "/v1/nonsense" {
		t.Errorf("instance = %q, want /v1/nonsense", p.Instance)
	}
}

func TestUnknownRoute_CacheControlIsNoStore(t *testing.T) {
	// /v1/assets carries `public, max-age=60, s-maxage=300`. The catch-all 404 for a malformed id
	// MUST NOT inherit it, or a CDN would cache the transient 404 and replay it.
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/assets/X-malformed-id")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a 404", got)
	}
}

func TestMethodMismatch_CacheControlIsNoStore(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/assets", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a 405", got)
	}
}

func TestRootReturnsWelcomeEnvelope(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var env struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("body not envelope JSON: %v", err)
	}
	if env.Data["name"] != "stellar-index" {
		t.Errorf("name = %q, want stellar-index", env.Data["name"])
	}
	if env.Data["docs"] == "" {
		t.Error("docs should be non-empty")
	}
}

func TestMethodMismatch(t *testing.T) {
	// /v1/healthz is GET-only; POST should be 405 with problem+json
	// (the envelope404Middleware translates the mux's text/plain
	// default into the same wire shape as our other error responses).
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for POST /healthz", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	var p struct {
		Title  string
		Status int
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("body not problem+json: %v", err)
	}
	if p.Title != "Method not allowed" {
		t.Errorf("title = %q, want %q", p.Title, "Method not allowed")
	}
	if p.Status != 405 {
		t.Errorf("status field = %d, want 405", p.Status)
	}
}

func TestMiddlewareStackAppliedEndToEnd(t *testing.T) {
	// Hit a real endpoint and assert the full stack fired:
	// RequestID preserves client values + mints when absent.
	ts := newTestServer(t)

	// 1. Client-supplied X-Request-ID is preserved verbatim.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-ID", "test-trace-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != "test-trace-abc" {
		t.Errorf("client-supplied X-Request-ID not preserved: %q", got)
	}

	// 2. Absent header → middleware mints 32-char hex ID.
	resp2, err := http.Get(ts.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if id := resp2.Header.Get("X-Request-ID"); len(id) != 32 {
		t.Errorf("minted X-Request-ID len = %d, want 32 hex chars", len(id))
	}
}

// assertReadyzErrorRedacted: /v1/readyz is unauthenticated and exempt from the anonymous limiter, so a failed
// check must name the dependency and point at the server log without echoing the driver error (endpoint, and
// from pgx user and database).
func assertReadyzErrorRedacted(t *testing.T, body, check, rawErr string) {
	t.Helper()
	if strings.Contains(body, rawErr) {
		t.Errorf("readyz echoed the raw %s error %q to an unauthenticated caller: %s", check, rawErr, body)
	}
	var env struct {
		Data struct {
			Checks []struct {
				Name  string `json:"name"`
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode readyz body: %v", err)
	}
	for _, c := range env.Data.Checks {
		if c.Name != check {
			continue
		}
		if c.OK || !strings.Contains(c.Error, "see the API server log") {
			t.Errorf("check %s = {ok:%v error:%q}, want ok=false with the fixed server-log hint", check, c.OK, c.Error)
		}
		return
	}
	t.Errorf("readyz body has no %s check: %s", check, body)
}

func readAll(resp *http.Response) (string, error) {
	b, err := ioReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Thin shim so the test file doesn't import io directly — keeps
// the imports list short + greppable.
var ioReadAll = func(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, fmt.Errorf("read: %w", err)
		}
	}
}

// TestRobotsTxt pins that the API origin disallows crawler indexing of /v1/* JSON (indexable
// content lives on the companion subdomains). Without the handler, GET gets Cloudflare's managed
// robots.txt while HEAD 404s, which makes a missing handler visible.
func TestRobotsTxt(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/robots.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Disallow: /") {
		t.Errorf("body missing Disallow directive: %q", body)
	}
	if !strings.Contains(string(body), "User-agent: *") {
		t.Errorf("body missing User-agent directive: %q", body)
	}
	if !strings.Contains(string(body), "Sitemap: https://stellarindex.io/sitemap.xml") {
		t.Errorf("body missing Sitemap pointer: %q", body)
	}
}

// TestMetricsLoopbackOnly_NonLoopbackReturns404 pins the Go-layer gate behind Caddy: /metrics from
// a non-loopback RemoteAddr must 404, covering a stale Caddyfile. It calls ServeHTTP directly with
// a public RemoteAddr because httptest.Server connects via 127.0.0.1 and would always pass.
func TestMetricsLoopbackOnly_NonLoopbackReturns404(t *testing.T) {
	srv := v1.New(v1.Options{})
	handler := srv.Handler()

	for _, remoteAddr := range []string{
		"203.0.113.42:54321", // public IPv4 (RFC 5737 docs example)
		"198.51.100.1:1234",  // another docs-only IPv4
		"[2001:db8::1]:443",  // IPv6 docs example
	} {
		t.Run(remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (non-loopback /metrics must 404)", rec.Code)
			}
		})
	}
}

// TestMetricsLoopbackOnly_LoopbackPasses pins the inverse —
// loopback callers (the local Prometheus scraper) must still
// receive the metrics body.
func TestMetricsLoopbackOnly_LoopbackPasses(t *testing.T) {
	srv := v1.New(v1.Options{})
	handler := srv.Handler()

	for _, remoteAddr := range []string{
		"127.0.0.1:54321",
		"127.5.5.5:1234", // any 127/8 address counts as loopback
		"[::1]:443",
	} {
		t.Run(remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (loopback /metrics must pass)", rec.Code)
			}
			body, _ := io.ReadAll(rec.Body)
			if !strings.Contains(string(body), "go_goroutines") {
				t.Errorf("body missing standard Prometheus content (go_goroutines)")
			}
		})
	}
}

// TestSecurityTxt pins the RFC 9116 disclosure metadata at /.well-known/security.txt, where researchers scanning
// the API origin expect to find the disclosure email.
func TestSecurityTxt(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/.well-known/security.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"Contact: mailto:security@stellarindex.io",
		"Expires: ",
		"Canonical: https://stellarindex.io/.well-known/security.txt",
		"Policy: https://github.com/Stellar-Index/StellarIndex/blob/main/SECURITY.md",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestReadyz_SchemaMismatchDrainsBackend: a schema critical checker reporting an applied head one
// below the binary's expectation makes /v1/readyz return 503 naming the schema failure. Postgres
// itself is up, so the 503 is attributable solely to the schema/binary mismatch.
func TestReadyz_SchemaMismatchDrainsBackend(t *testing.T) {
	stale := fakeSchemaReader{version: v1.ExpectedSchemaVersion - 1}
	ts := newTestServer(t,
		&stubCheck{name: "postgres", critical: true},
		v1.NewSchemaVersionChecker(stale),
	)
	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatalf("GET /v1/readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (schema mismatch must drain the backend)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"status":"unready"`) {
		t.Errorf("body should report unready on schema mismatch: %s", body)
	}
	if !strings.Contains(body, "schema/binary mismatch") {
		t.Errorf("body should name the schema/binary mismatch: %s", body)
	}
}
