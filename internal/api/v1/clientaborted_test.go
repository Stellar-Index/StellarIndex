package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// clientAborted decides whether a reader error came from a cancelled client request (return
// without writing so HTTPMetrics labels it 499) or from a genuine error (500) or server-side
// deadline (503).
//
// Decision rule: the request context must be done AND cancelled. net/http cancels r.Context()
// with context.Canceled when the peer hangs up, the one state where nobody is left to read a
// response. Any other done-ness is a SERVER-side budget (handler WithTimeout guards or the
// blanket middleware.RequestTimeout, which wraps r.Context()) and must reach the 503 branch.
//
// Pinned cases:
//
//   - err is context.Canceled, request ctx alive → false (server-side cancel, not a client abort)
//   - err is context.DeadlineExceeded, request ctx alive → false (server-side WithTimeout fired;
//     handler should write 503)
//   - request ctx CANCELLED (any err)             → true
//   - request ctx DEADLINE-EXCEEDED (any err)     → false
//   - none of the above                           → false
//
// A regression that re-added the errors.Is chain would swallow server-side timeouts and leave
// the client hanging; a bare `Err() != nil` would turn the request deadline into a bodyless 200.

func TestClientAborted_directContextCanceled_aliveReqCtx(t *testing.T) {
	// A bare context.Canceled with the request context still alive
	// is a server-side cancel, not a client abort. The handler
	// should keep going (and either write 500 or, if it knows the
	// cancel was timer-driven, structure its own response path).
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	if clientAborted(r, context.Canceled) {
		t.Error("clientAborted(context.Canceled, alive ctx) = true, want false")
	}
}

func TestClientAborted_directDeadlineExceeded_aliveReqCtx(t *testing.T) {
	// A bare DeadlineExceeded with the request context alive comes from a cold-path
	// WithTimeout guard. Returning true would skip the handler's 503 branch and send an
	// empty body instead of problem+json.
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	if clientAborted(r, context.DeadlineExceeded) {
		t.Error("clientAborted(context.DeadlineExceeded, alive ctx) = true, want false (server-side deadline must flow to 503 path)")
	}
}

func TestClientAborted_wrappedContextCanceled_aliveReqCtx(t *testing.T) {
	// Wrapping doesn't change the analysis: if r.Context() is alive,
	// the cancel was server-internal, not a client abort.
	wrapped := fmt.Errorf("storage: %w", context.Canceled)
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	if clientAborted(r, wrapped) {
		t.Error("clientAborted(wrapped Canceled, alive ctx) = true, want false")
	}
}

func TestClientAborted_unrelatedError_falseWhenCtxAlive(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	if clientAborted(r, errors.New("disk full")) {
		t.Error("clientAborted(unrelated err, alive ctx) = true, want false")
	}
}

func TestClientAborted_unrelatedError_butCtxDoneViaRequest(t *testing.T) {
	// The request's own context has been cancelled — that's the
	// authoritative "client gone" signal regardless of what the
	// reader returned (downstream may have wrapped the cancel in a
	// custom error type that doesn't satisfy errors.Is).
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	if !clientAborted(r, errors.New("downstream wrapped")) {
		t.Error("clientAborted(any err, cancelled req ctx) = false, want true")
	}
}

func TestClientAborted_nilErrorButCtxDone(t *testing.T) {
	// Edge case: err nil, but req ctx is done. Some readers return
	// (nil, nil) on cancellation rather than the ctx error — the
	// req-ctx check catches them.
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	if !clientAborted(r, nil) {
		t.Error("clientAborted(nil err, cancelled req ctx) = false, want true")
	}
}

func TestClientAborted_reqCtxDeadlineExceeded_isNotAnAbort(t *testing.T) {
	// The request context is done with DEADLINE, not CANCEL: the middleware.RequestTimeout
	// budget expired while the client is still on the wire. Treating it as an abort makes the
	// handler return silently and net/http supply an implicit empty 200, which a caller
	// checking resp.ok renders as fact.
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	ctx, cancel := context.WithTimeout(r.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	r = r.WithContext(ctx)
	if clientAborted(r, context.DeadlineExceeded) {
		t.Error("clientAborted(_, deadline-exceeded req ctx) = true, want false (the request deadline owes the client a 503)")
	}
}

func TestClientAborted_canceledErr_butReqCtxDone_returnsTrue(t *testing.T) {
	// Race case: req ctx and the reader's err both indicate cancel.
	// The req-ctx check is sufficient, regardless of err.
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	if !clientAborted(r, context.Canceled) {
		t.Error("clientAborted(Canceled, cancelled req ctx) = false, want true")
	}
}

// TestClientAborted classifies the cancellation states a handler may observe. clientAborted
// gates the "skip writeProblem, let HTTPMetrics label this 499" path; bare ctx errors with a
// live r.Context() must stay false so server-side deadlines reach the 503 branch.
func TestClientAborted(t *testing.T) {
	t.Run("ctx canceled error with live request ctx", func(t *testing.T) {
		// Bare context.Canceled with the request still alive is a
		// server-internal cancel — not a client abort.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if clientAborted(req, context.Canceled) {
			t.Error("clientAborted(ctx.Canceled, alive req ctx) = true, want false")
		}
	})
	t.Run("deadline exceeded error with live request ctx", func(t *testing.T) {
		// THE bug fix: server-side context.WithTimeout(8s) deadlines
		// must flow through to the handler's 503 path, not get
		// swallowed as a client abort.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if clientAborted(req, context.DeadlineExceeded) {
			t.Error("clientAborted(DeadlineExceeded, alive req ctx) = true, want false (must flow to 503)")
		}
	})
	t.Run("wrapped ctx canceled via request context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		// Even with an unrelated err arg, a done request context is
		// the authoritative "client gone" signal.
		if !clientAborted(req, errors.New("downstream wrapped error")) {
			t.Error("clientAborted with done request context = false, want true")
		}
	})
	t.Run("plain non-context error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if clientAborted(req, errors.New("boom")) {
			t.Error("clientAborted(plain err) = true, want false")
		}
	})
	t.Run("nil error and live context", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if clientAborted(req, nil) {
			t.Error("clientAborted(nil, live ctx) = true, want false")
		}
	})
}
