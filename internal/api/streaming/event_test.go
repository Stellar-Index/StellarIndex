package streaming

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWriteStream_EmitsRetryInPrelude pins that writeStream emits `retry: 5000` before the `:connected` prelude and
// the first event, so a client that disconnects at once still learns the reconnect backoff.
func TestWriteStream_EmitsRetryInPrelude(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	ch := make(chan Event, 1)
	ch <- Event{ID: "1", Type: "test", Data: []byte(`{}`)}
	close(ch)

	writeStream(rec, req, ch, StreamOptions{HeartbeatInterval: time.Hour})

	body := rec.Body.String()
	const wantPrelude = "retry: 5000\n\n:connected\n\n"
	if !strings.HasPrefix(body, wantPrelude) {
		t.Fatalf("writeStream body = %q, want prefix %q", body, wantPrelude)
	}
	if idx := strings.Index(body, "id: 1\n"); idx < len(wantPrelude) {
		t.Fatalf("first event frame did not follow the retry prelude: body = %q", body)
	}
}

// TestGenerator_NeverDuplicates pins that an ID is never returned twice. A counter masked to 16 bits would wrap at 65
// 536 IDs/ms and re-issue IDs, so 70 000 calls in one millisecond must advance the synthetic millis instead.
func TestGenerator_NeverDuplicates(t *testing.T) {
	t.Parallel()

	const N = 70_000
	g := &Generator{}
	// Pre-seed a future millis so wall-clock now() stays smaller and every Next() takes the counter-bump or overflow
	// path.
	const futureMs = uint64(1<<47 - 1) // far enough that time.Now never beats it
	g.state.Store(futureMs << 16)

	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		id := g.Next()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestGenerator_StrictlyIncreasing pins the wire contract that lexicographic order is chronological order; a lower ID
// would break snapshotAfter's `id > lastEventID` filter and Last-Event-ID resume.
func TestGenerator_StrictlyIncreasing(t *testing.T) {
	t.Parallel()

	const N = 100_000
	g := &Generator{}
	const futureMs = uint64(1<<47 - 1)
	g.state.Store(futureMs << 16)

	prev := ""
	for i := 0; i < N; i++ {
		id := g.Next()
		if i > 0 && id <= prev {
			t.Fatalf("ID at iteration %d (%s) is not strictly greater than previous (%s)", i, id, prev)
		}
		prev = id
	}
}

// TestGenerator_ConcurrentNoDuplicates — under concurrent calls
// from many goroutines, every ID is still unique. The atomic-CAS
// loop is the contract that makes this safe; this test pins it.
func TestGenerator_ConcurrentNoDuplicates(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	const perGoroutine = 2_000
	const total = goroutines * perGoroutine

	g := &Generator{}
	results := make(chan string, total)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				results <- g.Next()
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]struct{}, total)
	for id := range results {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID under concurrency: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != total {
		t.Errorf("got %d unique IDs, want %d", len(seen), total)
	}
}
