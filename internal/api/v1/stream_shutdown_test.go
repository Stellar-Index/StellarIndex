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

// streamDrainCeiling is what an SSE handler must beat between
// BeginStreamDrain and the response body ending. The writer returns on
// its next select, so this is generous by orders of magnitude; it
// exists to fail loudly rather than hang when the wiring is dropped.
const streamDrainCeiling = 2 * time.Second

// TestStreamHandlersEndOnTheShutdownDrain runs the drain through the
// REAL v1 server and the real routes.
//
// The streaming package's own tests prove the writer honours a Drain.
// This proves the handlers actually hand the server's Drain to that
// writer — the half that rots silently. A handler that keeps building
// its StreamOptions inline still compiles, still serves, and still
// passes every other test in this package; it just costs a 30s deploy
// stall the first time a client is attached to it during a restart.
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
			// Registered BEFORE the stream is opened, so cleanup's LIFO
			// order closes the client body first. httptest's Close
			// blocks until every outstanding request returns, and the
			// SSE handler returns only once its client is gone — the
			// other order deadlocks the moment this test FAILS, which
			// is exactly when its output matters.
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

// TestStreamDrainDoesNotAffectOrdinaryHandlers pins the SCOPE of the
// signal: BeginStreamDrain must be invisible to every non-stream route.
// This is the assertion that fails if the drain is ever "simplified"
// into an http.Server.BaseContext derived from the process root
// context, which would cancel ordinary in-flight requests at SIGTERM.
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

// openTestStream opens an SSE connection, waits for the `:connected`
// preamble so the connection is genuinely established (and therefore
// genuinely non-idle), and returns a channel carrying the error from
// draining the body to its end — nil for a cleanly terminated body.
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
