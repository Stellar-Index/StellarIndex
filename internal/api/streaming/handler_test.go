package streaming_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// readSSEFrames blocks until `want` complete frames have been read, or `timeout` elapses. A frame
// is one non-comment SSE event ended by a blank line; the raw text (minus the trailing blank
// line) is returned so tests can assert on `id:`/`event:`/`data:` directly.
func readSSEFrames(t *testing.T, body io.Reader, want int, timeout time.Duration) []string {
	t.Helper()
	br := bufio.NewReader(body)
	frames := make([]string, 0, want)
	deadline := time.Now().Add(timeout)
	var current strings.Builder
	for time.Now().Before(deadline) && len(frames) < want {
		// Use SetReadDeadline-equivalent: rely on the test killing
		// the underlying connection at timeout. ReadString here is
		// blocking but the httptest.Server cleanup handles teardown.
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		// SSE comment line — keepalive or :connected — skipped.
		if strings.HasPrefix(line, ":") {
			continue
		}
		// retry: is a standalone prelude field (no data line), so it
		// never dispatches a client-visible event; skip it like a
		// comment rather than let it seed a bogus first "frame".
		if strings.HasPrefix(line, "retry:") {
			continue
		}
		if line == "\n" {
			if current.Len() > 0 {
				frames = append(frames, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
	}
	return frames
}

// TestStream_DeliversLiveEvent — start a stream handler, publish
// one event, observe it on the wire as an SSE frame.
func TestStream_DeliversLiveEvent(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 5 * time.Second,
		})
	}))
	defer srv.Close()

	// Subscribe via HTTP first (so the connection is established
	// and ready for live events), then publish.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// Allow the handler's Subscribe to land before publishing.
	// Without this, hub.Publish can race with the subscribe-then-
	// listen path and the event slips through to the buffer alone.
	time.Sleep(50 * time.Millisecond)

	hub.Publish("topic", "price_update", []byte(`{"p":"0.12"}`))

	frames := readSSEFrames(t, resp.Body, 1, 2*time.Second)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	frame := frames[0]
	if !strings.Contains(frame, "id: ") {
		t.Errorf("frame missing id: %q", frame)
	}
	if !strings.Contains(frame, "event: price_update\n") {
		t.Errorf("frame missing event line: %q", frame)
	}
	if !strings.Contains(frame, `data: {"p":"0.12"}`) {
		t.Errorf("frame missing data: %q", frame)
	}
}

// TestStream_HeartbeatFires — with a fast heartbeat interval and no
// events, the handler still writes keepalive comments so the
// connection stays alive across reverse proxies.
func TestStream_HeartbeatFires(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 100 * time.Millisecond,
		})
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	keepaliveCount := 0
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ":keepalive") {
			keepaliveCount++
			if keepaliveCount >= 3 {
				return // got the heartbeats we expected
			}
		}
	}
	if keepaliveCount < 3 {
		t.Errorf("heartbeats observed in 750ms = %d, want at least 3 at 100ms cadence", keepaliveCount)
	}
}

// TestStream_LastEventIDHeaderResume — set Last-Event-ID on the
// request and observe replay of buffered events with greater IDs.
func TestStream_LastEventIDHeaderResume(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 5 * time.Second,
		})
	}))
	defer srv.Close()

	id1 := hub.Publish("topic", "x", []byte("first"))
	hub.Publish("topic", "x", []byte("second"))
	hub.Publish("topic", "x", []byte("third"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Last-Event-ID", id1) // resume after first
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp.Body, 2, 2*time.Second)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (replay of second + third)", len(frames))
	}
	if !strings.Contains(frames[0], "data: second") {
		t.Errorf("first replayed frame = %q, want second", frames[0])
	}
	if !strings.Contains(frames[1], "data: third") {
		t.Errorf("second replayed frame = %q, want third", frames[1])
	}
}

// TestStream_LastEventIDQueryFallback — clients that can't set the
// Last-Event-ID header (some browser EventSource bootstraps) can
// pass it as ?last_event_id= instead.
func TestStream_LastEventIDQueryFallback(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 5 * time.Second,
		})
	}))
	defer srv.Close()

	id1 := hub.Publish("topic", "x", []byte("first"))
	hub.Publish("topic", "x", []byte("second"))

	resp, err := srv.Client().Get(srv.URL + "?last_event_id=" + id1)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp.Body, 1, 2*time.Second)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (replay of second)", len(frames))
	}
	if !strings.Contains(frames[0], "data: second") {
		t.Errorf("frame = %q, want second", frames[0])
	}
}

// TestStream_ContextCancelEndsStream pins that Stream returns promptly once the request context cancels (client
// disconnect).
func TestStream_ContextCancelEndsStream(t *testing.T) {
	hub := streaming.NewHub(0)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 5 * time.Second,
		})
		close(done)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}

	// Cancel the client request → server-side ctx fires → handler
	// returns. Must complete inside a generous window.
	cancel()
	_ = resp.Body.Close()

	select {
	case <-done:
		// handler returned
	case <-time.After(2 * time.Second):
		t.Error("Stream did not return after client disconnect")
	}
}

// TestStream_KeepsRouteCacheControl pins that the SSE writer keeps the per-route policy set by the v1 CacheControl
// middleware (a bare `no-cache` would make /v1/price/stream's no-store storable and drop `private`); with none set,
// its fallback forbids storage.
func TestStream_KeepsRouteCacheControl(t *testing.T) {
	for _, preset := range []string{"no-store", "private, no-cache, must-revalidate", "private, no-store", ""} {
		t.Run(preset, func(t *testing.T) {
			hub := streaming.NewHub(0)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if preset != "" {
					w.Header().Set("Cache-Control", preset)
				}
				streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{HeartbeatInterval: 5 * time.Second})
			}))
			defer srv.Close()
			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatalf("GET stream: %v", err)
			}
			defer resp.Body.Close()
			want := preset
			if want == "" {
				want = "no-store"
			}
			if got := resp.Header.Get("Cache-Control"); got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
		})
	}
}

// TestStream_RevalidateFailureEndsStream pins that credential, key-policy and quota checks rerun at each heartbeat, so
// a stream admitted before its key was revoked ends instead of streaming until the client hangs up.
func TestStream_RevalidateFailureEndsStream(t *testing.T) {
	var revoked atomic.Bool
	var checks atomic.Int32
	hub := streaming.NewHub(0)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 50 * time.Millisecond,
			Revalidate: func(*http.Request) bool {
				checks.Add(1)
				return !revoked.Load()
			},
		})
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	// Two heartbeats pass while the credential is still good.
	for keepalives := 0; keepalives < 2; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended while the credential was valid: %v", err)
		}
		if line == ":keepalive\n" {
			keepalives++
		}
	}
	revoked.Store(true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream kept running after Revalidate reported the credential revoked")
	}
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), ":revoked\n\n") {
		t.Errorf("stream did not end with the :revoked comment; tail = %q", rest)
	}
	if checks.Load() < 3 {
		t.Errorf("Revalidate ran %d times, want one per heartbeat (>= 3)", checks.Load())
	}
}

// TestStream_MaxLifetimeEndsStreamAndResumes pins that MaxLifetime ends the stream even while heartbeats and events
// flow (a client that never reads never blocks a write), and that the reconnect resumes from Last-Event-ID.
func TestStream_MaxLifetimeEndsStreamAndResumes(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 20 * time.Millisecond,
			MaxLifetime:       200 * time.Millisecond,
		})
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	hub.Publish("topic", "price_update", []byte(`{"n":1}`))

	ended := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		ended <- b
	}()
	var body string
	select {
	case b := <-ended:
		body = string(b)
	case <-time.After(2 * time.Second):
		t.Fatal("stream outlived MaxLifetime")
	}
	if !strings.HasSuffix(body, ":reconnect\n\n") {
		t.Errorf("stream did not end with the :reconnect comment; tail = %q", body)
	}
	var lastID string
	for _, line := range strings.Split(body, "\n") {
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			lastID = id
		}
	}
	if lastID == "" {
		t.Fatalf("no event delivered before the lifetime ended; body = %q", body)
	}

	hub.Publish("topic", "price_update", []byte(`{"n":2}`))
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Last-Event-ID", lastID)
	resp2, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer resp2.Body.Close()
	frames := readSSEFrames(t, resp2.Body, 1, 2*time.Second)
	if len(frames) != 1 || !strings.Contains(frames[0], `data: {"n":2}`) {
		t.Errorf("reconnect with Last-Event-ID replayed %q, want the missed {\"n\":2} event", frames)
	}
}

// TestStream_PerIPConcurrentCap pins that one client IP holds at most MaxStreamsPerIP concurrent SSE connections and
// the (N+1)th gets 503; without the cap it would open with 200.
func TestStream_PerIPConcurrentCap(t *testing.T) {
	const capN = 3

	streaming.SetMaxStreamsPerIP(capN)
	defer streaming.SetMaxStreamsPerIP(0)
	// Collapse every httptest connection (distinct ephemeral ports) into
	// ONE per-IP bucket so the cap is exercised deterministically.
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "test-client" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long heartbeat + no publishes → the stream just sits open,
		// holding its slot until the client disconnects.
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	// Each successful GET returns only AFTER the handler wrote its 200
	// header — which happens AFTER the per-IP slot is acquired. So once
	// Do() returns 200, the slot is held.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	held := make([]*http.Response, 0, capN)
	for i := 0; i < capN; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status = %d, want 200", i, resp.StatusCode)
		}
		held = append(held, resp)
	}

	// The (N+1)th connection from the same IP must be refused.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap stream status = %d, want 429 (per-IP cap)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Releasing a held stream must free its slot: disconnect one, then a
	// fresh connection is accepted again.
	_ = held[0].Body.Close()
	// Poll until the server-side release lands (async on ctx cancel).
	deadline := time.Now().Add(2 * time.Second)
	for streaming.ActiveStreamsForIP("test-client") >= capN && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("reopen-after-release status = %d, want 200 (slot should have freed)", resp2.StatusCode)
	}
	_ = resp2.Body.Close()

	// Cleanup remaining held streams.
	cancel()
	for _, r := range held[1:] {
		_ = r.Body.Close()
	}
}

// TestStream_PerIPCapAggregatesIPv6Slash64 pins that the per-IP SSE cap keys IPv6 clients on their /64, so a client
// rotating addresses within a delegated /64 still meets it: with capN=3 the 4th connection must be rejected.
func TestStream_PerIPCapAggregatesIPv6Slash64(t *testing.T) {
	const capN = 3

	streaming.SetMaxStreamsPerIP(capN)
	defer streaming.SetMaxStreamsPerIP(0)

	// Hand out a fresh address within 2001:db8:1234:5678::/64 for each
	// connection — simulating a client that rotates its address inside
	// its delegated prefix.
	var next uint32
	streaming.SetStreamClientIPResolver(func(*http.Request) string {
		next++
		return fmt.Sprintf("2001:db8:1234:5678::%x", next)
	})
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	held := make([]*http.Response, 0, capN)
	for i := 0; i < capN; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status = %d, want 200", i, resp.StatusCode)
		}
		held = append(held, resp)
	}

	// The (capN+1)th connection uses yet another address in the SAME
	// /64 and must still be refused — the addresses differ, but the
	// masked bucket key doesn't.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap stream (different address, same /64) status = %d, want 429", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The diagnostic takes an address as the resolver produced it and
	// reports that address's bucket, not an unmasked miss.
	if got := streaming.ActiveStreamsForIP("2001:db8:1234:5678::1"); got != capN {
		t.Errorf("ActiveStreamsForIP(first address) = %d, want %d (its /64 bucket)", got, capN)
	}

	cancel()
	for _, r := range held {
		_ = r.Body.Close()
	}
}

// TestStream_PerIPCapDisabled confirms cap 0 is a no-op: many streams
// from one IP all open (the global cap still governs total volume).
func TestStream_PerIPCapDisabled(t *testing.T) {
	streaming.SetMaxStreamsPerIP(0)
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "one-ip" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d with per-IP cap disabled: status = %d, want 200", i, resp.StatusCode)
		}
		defer resp.Body.Close() //nolint:revive // held open until test teardown
	}
}
