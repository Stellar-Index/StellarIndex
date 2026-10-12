package v1_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// streamDrainCeiling is what an SSE handler must beat between BeginStreamDrain and the body ending. The writer
// returns on its next select, so it is generous; it exists to fail loudly instead of hanging if wiring is dropped.
const streamDrainCeiling = 2 * time.Second

// TestStreamHandlersEndOnTheShutdownDrain runs the drain through the real v1 server and routes.
// The streaming package's tests prove the writer honours a Drain; this proves handlers hand it the server's Drain.
// A handler building StreamOptions inline still compiles and passes other tests but stalls a restart.
func TestStreamHandlersEndOnTheShutdownDrain(t *testing.T) {
	cases := []struct {
		name string
		path string
		opts v1.Options
	}{
		{
			name: "ledger",
			path: "/v1/ledger/stream",
			opts: v1.Options{Cursors: &advancingCursorsReader{ledger: 1000}},
		},
		{
			// The Hub-driven writer, as distinct from the ledger
			// stream's per-connection producer: both reach writeStream,
			// by different entry points.
			name: "price",
			path: "/v1/price/stream?asset=native&quote=fiat:USD",
			opts: v1.Options{Hub: streaming.NewHub(8)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(tc.opts)
			ts := httptest.NewServer(srv.Handler())
			// Registered BEFORE the stream is opened, so cleanup's LIFO order closes the client
			// body first. httptest's Close blocks until every request returns and the SSE
			// handler returns only once its client is gone; the other order deadlocks on FAIL.
			t.Cleanup(ts.Close)

			done := openTestStream(t, ts.URL+tc.path)

			srv.BeginStreamDrain()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("stream body ended with %v — want a cleanly terminated body", err)
				}
			case <-time.After(streamDrainCeiling):
				t.Fatalf("%s did not end within %v of BeginStreamDrain — "+
					"the handler is not passing the server's drain to the SSE writer",
					tc.path, streamDrainCeiling)
			}
		})
	}
}

// TestStreamDrainDoesNotAffectOrdinaryHandlers pins the scope: BeginStreamDrain must be invisible to non-stream
// routes. It fails if the drain becomes an http.Server.BaseContext from the process root, which would cancel
// ordinary in-flight requests at SIGTERM.
func TestStreamDrainDoesNotAffectOrdinaryHandlers(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	srv.BeginStreamDrain()

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, ts.URL+"/v1/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/healthz after the drain began: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the stream drain must not touch ordinary routes", resp.StatusCode)
	}
}

// openTestStream opens an SSE connection, waits for the `:connected` preamble so it is genuinely established and
// non-idle, and returns a channel with the error from draining the body (nil for a clean end).
func openTestStream(t *testing.T, url string) <-chan error {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by t.Cleanup
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}

	// writeStream sends the retry: reconnection hint (and its
	// terminating blank line) ahead of :connected; skip past both to
	// find the connection marker itself.
	br := bufio.NewReader(resp.Body)
	var line string
	for i := 0; i < 3; i++ {
		line, err = br.ReadString('\n')
		if err != nil {
			t.Fatalf("read preamble: %v", err)
		}
		if line == "\n" || strings.HasPrefix(line, "retry:") {
			continue
		}
		break
	}
	if !strings.HasPrefix(line, ":connected") {
		t.Fatalf("preamble = %q, want :connected", line)
	}

	done := make(chan error, 1)
	go func() {
		_, readErr := io.ReadAll(br)
		done <- readErr
	}()
	return done
}

// TestStreamEndpointsKeepTheRouteCacheControl asserts the Cache-Control a client receives on a stream through the
// full middleware stack: the CacheControl per-route policy, not the SSE writer's fallback. The writer must not
// overwrite it with a bare `no-cache`, which permits storage and drops `private`.
func TestStreamEndpointsKeepTheRouteCacheControl(t *testing.T) {
	cases := []struct {
		name, path, want string
		opts             v1.Options
	}{
		{"ledger", "/v1/ledger/stream", "private, no-store", v1.Options{Cursors: &advancingCursorsReader{ledger: 1000}}},
		{"price", "/v1/price/stream?asset=native&quote=fiat:USD", "no-store", v1.Options{Hub: streaming.NewHub(8)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(v1.New(tc.opts).Handler())
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want the route policy %q", got, tc.want)
			}
		})
	}
}
