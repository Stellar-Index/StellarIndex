package metadata_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
)

// bodyCapBytes mirrors the unexported metadata.maxBodyBytes (1 MiB) — the
// two guards below are meant to be checked from outside the package, so the
// cap is restated rather than exported just for a test.
const bodyCapBytes = 1 << 20

// TestResolver_RejectsDeclaredOversizedBody pins the guard that fires on an
// HONEST Content-Length: a server declaring a body over the cap must be
// refused before we read a single byte of it.
func TestResolver_RejectsDeclaredOversizedBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(bodyCapBytes+1))
		w.WriteHeader(http.StatusOK)
		// Deliberately short of the declared length: the guard must trip on
		// resp.ContentLength alone, without reading the body.
		_, _ = w.Write([]byte("VERSION=\"2.0.0\"\n"))
	}))
	defer srv.Close()

	r := newLocalResolver(t, srv)
	_, err := r.Resolve(context.Background(), hostOf(t, srv))
	if !errors.Is(err, metadata.ErrTOMLTooLarge) {
		t.Fatalf("Resolve() = %v, want an error wrapping ErrTOMLTooLarge", err)
	}
}

// TestResolver_RejectsStreamedOversizedBody pins the second, independent
// guard: a body that arrives WITHOUT (or despite) an honest Content-Length
// — chunked transfer, or a lying header — must still be capped once actual
// bytes exceed the limit. Without this half, a compressed or chunked
// response could smuggle an unbounded body past the Content-Length check.
func TestResolver_RejectsStreamedOversizedBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length set: net/http switches to chunked transfer and
		// resp.ContentLength on the client side comes back -1, bypassing the
		// first guard entirely.
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}
		chunk := strings.Repeat("a", 64*1024)
		written := 0
		for written <= bodyCapBytes {
			n, err := w.Write([]byte(chunk))
			if err != nil {
				return
			}
			written += n
			flusher.Flush()
		}
	}))
	defer srv.Close()

	r := newLocalResolver(t, srv)
	_, err := r.Resolve(context.Background(), hostOf(t, srv))
	if !errors.Is(err, metadata.ErrTOMLTooLarge) {
		t.Fatalf("Resolve() = %v, want an error wrapping ErrTOMLTooLarge", err)
	}
}
