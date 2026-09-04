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

// clientAborted is the helper handlers use to decide whether a
// reader-returned error came from a cancelled client request
// (return without writing — let HTTPMetrics label the request
// 499) vs. a genuine internal error (write 500) or a server-side
// deadline (write 503).
//
// Decision rule: the request context must be done AND cancelled.
// net/http cancels r.Context() with context.Canceled when the peer
// hangs up, so that is the one state in which nobody is left to read
// a response. Every other done-ness is a SERVER-side budget expiring
// with the client still waiting, and must flow through to the 503
// timeout-response branch: the cold-path WithTimeout guards inside
// handlers (#1082, #1099-#1105), and the blanket
// middleware.RequestTimeout deadline, which since C3-102 wraps
// r.Context() itself.
//
// The cases this test pins:
//
//   - err is context.Canceled, request ctx alive → false
//     (server-side cancel, e.g. internal context.WithCancel; not
//     a client abort)
//   - err is context.DeadlineExceeded, request ctx alive → false
//     (server-side WithTimeout fired; handler should write 503)
//   - request ctx CANCELLED (any err)             → true
//   - request ctx DEADLINE-EXCEEDED (any err)     → false
//   - none of the above                           → false
//
// Without these tests, a regression that re-added the errors.Is
// chain would silently swallow server-side timeouts and leave the
// client hanging without a response — and one that went back to a
// bare `Err() != nil` would turn the request deadline into a
// bodyless HTTP 200.

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
	// THE bug fix: a bare context.DeadlineExceeded with the request
	// context still alive comes from one of our cold-path
	// context.WithTimeout(8s) guards (#1082, #1099-#1105). Returning
	// true here would short-circuit the handler before its 503
	// timeout-response branch fires — the client would get an empty
	// body instead of a structured problem+json.
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
	// The request context itself is done — but with DEADLINE, not
	// CANCEL. That is the middleware.RequestTimeout budget expiring
	// (it wraps r.Context()), and the client is still on the wire.
	//
	// Treating it as an abort is what made
	// /v1/lending/pools/{pool}/reserves answer HTTP 200 with
	// content-length 0 after 15.1s: the handler returned silently and
	// net/http supplied the implicit 200. A caller checking resp.ok
	// then renders an empty pool as fact.
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
