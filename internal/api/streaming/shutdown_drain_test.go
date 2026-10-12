package streaming_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// shutdownTestBudget is the drain budget given to http.Server.Shutdown (production uses 30s); short enough that the
// unwired negative control, which burns the whole budget, stays fast, yet far above a drained stream's cost.
const shutdownTestBudget = 2 * time.Second

// promptDrainCeiling is what a shutdown with an attached stream must beat. Shutdown polls idle connections on a
// backoff capped at 500ms, so the floor is the poll after the handler returns; 500ms tolerates a loaded CI box and
// still fails on the 30s bug.
const promptDrainCeiling = 500 * time.Millisecond

// sseTestServer is a real http.Server (not httptest) serving one
// never-ending SSE endpoint, so the tests exercise the actual
// Shutdown/idle-connection machinery rather than a stand-in.
type sseTestServer struct {
	srv  *http.Server
	addr string
}

// startSSEServer boots a server whose /stream holds an SSE connection open forever. A non-nil drain is registered via
// RegisterOnShutdown and handed to the writer (the wiring under test); nil reproduces the unwired shape.
func startSSEServer(t *testing.T, drain *streaming.Drain) *sseTestServer {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		// An event channel nothing sends on or closes: the connection ends only if the client leaves or the drain
		// fires, like the long-lived browser stream r1 measured.
		ch := make(chan streaming.Event)
		streaming.StreamFromChannel(w, r, ch, streaming.StreamOptions{
			HeartbeatInterval: time.Hour,
			Drain:             drain,
		})
	})
	// A slow NON-stream handler, so the same server can prove the drain
	// leaves ordinary in-flight requests alone.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(750 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("complete"))
		case <-r.Context().Done():
			// Cancelled mid-flight — write nothing so the client sees a
			// truncated response and the assertion fails.
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if drain != nil {
		srv.RegisterOnShutdown(drain.Begin)
	}
	go func() { _ = srv.Serve(ln) }()

	ts := &sseTestServer{srv: srv, addr: ln.Addr().String()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	})
	return ts
}

// openStream connects to /stream and reads the `:connected` preamble,
// so the caller knows the connection is genuinely established (and
// therefore genuinely non-idle) before shutdown is measured.
func openStream(t *testing.T, ts *sseTestServer) (*http.Response, *bufio.Reader) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "http://"+ts.addr+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by t.Cleanup below
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	// writeStream sends the retry: reconnection hint (and its
	// terminating blank line) ahead of :connected; skip past both to
	// find the connection marker itself.
	br := bufio.NewReader(resp.Body)
	var line string
	for i := 0; i < 3; i++ {
		var err error
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
	return resp, br
}

// TestShutdownWithAnAttachedStreamIsPrompt guards the 30s deploy stall: http.Server.Shutdown waits
// for every connection to go IDLE, and an SSE connection never does, so without a shutdown signal
// the stream writer can see, one attached /v1/ledger/stream pins the drain for the full budget.
func TestShutdownWithAnAttachedStreamIsPrompt(t *testing.T) {
	drain := streaming.NewDrain()
	ts := startSSEServer(t, drain)
	openStream(t, ts)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTestBudget)
	defer cancel()

	err := ts.srv.Shutdown(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Shutdown returned %v after %v — want a clean drain", err, elapsed)
	}
	if elapsed > promptDrainCeiling {
		t.Fatalf("drain took %v with one stream attached, want < %v", elapsed, promptDrainCeiling)
	}
	t.Logf("drain with one attached stream: %v", elapsed)
}

// TestShutdownWithoutTheDrainSignalBurnsTheBudget is the negative control: without the drain wiring, the same server,
// handler and client exhaust the budget and Shutdown reports DeadlineExceeded, so the prompt-drain test cannot pass
// for the wrong reason.
func TestShutdownWithoutTheDrainSignalBurnsTheBudget(t *testing.T) {
	ts := startSSEServer(t, nil)
	openStream(t, ts)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTestBudget)
	defer cancel()

	err := ts.srv.Shutdown(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v after %v, want context.DeadlineExceeded — "+
			"an unwired SSE stream is supposed to hold the drain; if it no longer "+
			"does, this control no longer proves anything", err, elapsed)
	}
	if elapsed < shutdownTestBudget {
		t.Fatalf("drain returned after %v, want the whole %v budget", elapsed, shutdownTestBudget)
	}
	t.Logf("drain with one unwired stream: %v (the bug)", elapsed)
}

// TestShutdownWithNoStreamsStaysFast guards the other direction: the
// 0.21s clean-restart case must not get slower. A fix that lengthens
// the common shutdown is not a fix.
func TestShutdownWithNoStreamsStaysFast(t *testing.T) {
	drain := streaming.NewDrain()
	ts := startSSEServer(t, drain)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTestBudget)
	defer cancel()

	err := ts.srv.Shutdown(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Shutdown returned %v after %v", err, elapsed)
	}
	if elapsed > promptDrainCeiling {
		t.Fatalf("drain took %v with no connections at all, want < %v", elapsed, promptDrainCeiling)
	}
	t.Logf("drain with no streams: %v", elapsed)
}

// TestStreamEndsCleanlyOnDrain pins the client-visible shape of a drained stream: a final `:draining` comment frame
// then a clean io.EOF, so an EventSource reconnects normally instead of seeing the proxy's `reading: unexpected EOF`.
func TestStreamEndsCleanlyOnDrain(t *testing.T) {
	drain := streaming.NewDrain()
	ts := startSSEServer(t, drain)
	_, br := openStream(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTestBudget)
	defer cancel()
	go func() { _ = ts.srv.Shutdown(ctx) }()

	// Bounded rather than a bare io.ReadAll: without the drain the body
	// never ends, and a test that HANGS reports nothing useful. Failing
	// on a deadline says which behaviour is missing.
	type tail struct {
		body []byte
		err  error
	}
	read := make(chan tail, 1)
	go func() {
		b, err := io.ReadAll(br)
		read <- tail{body: b, err: err}
	}()

	select {
	case got := <-read:
		if got.err != nil {
			t.Fatalf("read to end of stream: %v — want a cleanly terminated body", got.err)
		}
		if !strings.Contains(string(got.body), ":draining") {
			t.Errorf("tail = %q, want a :draining comment frame before the close", got.body)
		}
	case <-time.After(shutdownTestBudget + time.Second):
		t.Fatalf("the response body never ended — the stream was not closed by the drain")
	}
}

// TestDrainLeavesOrdinaryRequestsAlone pins the decision not to use http.Server.BaseContext, which would cancel every
// in-flight request on SIGTERM: a slow ordinary request must still complete and Shutdown must still wait for it.
func TestDrainLeavesOrdinaryRequestsAlone(t *testing.T) {
	drain := streaming.NewDrain()
	ts := startSSEServer(t, drain)

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodGet, "http://"+ts.addr+"/slow", nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		close(started)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), err: err}
	}()

	<-started
	// Let the request reach the handler before shutdown starts.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTestBudget)
	defer cancel()
	shutdownErr := ts.srv.Shutdown(ctx)

	got := <-done
	if got.err != nil {
		t.Fatalf("slow request failed across shutdown: %v — the drain must not cancel ordinary requests", got.err)
	}
	if got.body != "complete" {
		t.Errorf("slow request body = %q, want %q — the handler was cut short by shutdown", got.body, "complete")
	}
	if shutdownErr != nil {
		t.Errorf("Shutdown err = %v, want nil (it should have waited for the slow request)", shutdownErr)
	}
}
