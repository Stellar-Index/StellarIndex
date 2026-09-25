package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIdempotencyStore_PutSweepsExpiredEntries proves an idempotency
// key that's used exactly once (never replayed) does not live in the
// store forever: once its TTL has passed, a LATER put() on a
// different key must evict it, not just leave it resident until the
// process restarts.
func TestIdempotencyStore_PutSweepsExpiredEntries(t *testing.T) {
	store := NewIdempotencyStore(time.Minute)
	now := time.Now()
	store.now = func() time.Time { return now }

	store.put("account-1:key-a", http.StatusCreated, http.Header{}, []byte("a"))
	store.put("account-1:key-b", http.StatusCreated, http.Header{}, []byte("b"))

	if got := len(store.entries); got != 2 {
		t.Fatalf("entries after two puts = %d, want 2", got)
	}

	// Advance well past the TTL. Neither key-a nor key-b is ever
	// looked up again (no replay), which is exactly the
	// create-then-never-replay pattern that leaked unbounded before
	// the fix.
	now = now.Add(2 * time.Minute)

	store.put("account-1:key-c", http.StatusCreated, http.Header{}, []byte("c"))

	if got := len(store.entries); got != 1 {
		t.Fatalf("entries after expiry sweep = %d, want 1 (only the fresh key-c); "+
			"stale keys from a one-shot idempotency key must not survive past their TTL", got)
	}
	if _, ok := store.entries["account-1:key-c"]; !ok {
		t.Fatalf("expected key-c to remain in the store")
	}
}

// TestIdempotency_RetryWhileOriginalInFlight_DoesNotRunTwice is the
// late-commit shape: the client times out and retries while the first
// request is still inside the handler. The retry must not run the
// handler again (a second mint); it gets a retryable 409, and once the
// original finishes a further retry replays the original response.
func TestIdempotency_RetryWhileOriginalInFlight_DoesNotRunTwice(t *testing.T) {
	var runs atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	h := Idempotency(NewIdempotencyStore(time.Minute), func(*http.Request) string { return "acct-1" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := runs.Add(1)
			if n == 1 {
				close(entered)
				<-release
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, "minted-%d", n)
		}))
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/account/keys", nil)
		req.Header.Set(IdempotencyKeyHeader, "retry-1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	firstDone := make(chan *httptest.ResponseRecorder)
	go func() { firstDone <- send() }()
	<-entered

	retry := send()
	if retry.Code != http.StatusConflict {
		t.Fatalf("retry during in-flight original: status = %d body %q, want 409", retry.Code, retry.Body.String())
	}
	if !strings.Contains(retry.Body.String(), "idempotency-key-in-flight") || retry.Header().Get("Retry-After") == "" {
		t.Errorf("in-flight 409 must be the idempotency-key-in-flight problem with Retry-After; got %q", retry.Body.String())
	}

	close(release)
	first := <-firstDone
	if first.Code != http.StatusCreated || first.Body.String() != "minted-1" {
		t.Fatalf("original: status = %d body %q, want 201 minted-1", first.Code, first.Body.String())
	}

	replay := send()
	if replay.Body.String() != "minted-1" || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("post-completion retry: body %q replayed=%q, want the original minted-1 replayed",
			replay.Body.String(), replay.Header().Get("Idempotency-Replayed"))
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("handler ran %d times for one Idempotency-Key, want 1", got)
	}
}

// TestIdempotency_FailedOriginalReleasesClaim: a non-2xx original is not
// cached and must not leave the key stuck in flight, or a corrected
// retry would 409 until the process restarts.
func TestIdempotency_FailedOriginalReleasesClaim(t *testing.T) {
	var runs atomic.Int32
	h := Idempotency(NewIdempotencyStore(time.Minute), func(*http.Request) string { return "acct-1" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if runs.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
		}))
	for i, want := range []int{http.StatusBadRequest, http.StatusCreated} {
		req := httptest.NewRequest(http.MethodPost, "/v1/account/keys", nil)
		req.Header.Set(IdempotencyKeyHeader, "retry-2")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("attempt %d: status = %d, want %d", i+1, w.Code, want)
		}
	}
}
