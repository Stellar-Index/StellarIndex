package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineProbe records whether the request context already carries a
// deadline by the time this middleware runs.
type deadlineProbe struct {
	saw       bool
	remaining time.Duration
}

func (p *deadlineProbe) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dl, ok := r.Context().Deadline()
		p.saw = ok
		if ok {
			p.remaining = time.Until(dl)
		}
		next.ServeHTTP(w, r)
	})
}

// TestRequestTimeout_BoundsThePreHandlerStack pins C3-102
// (audit-2026-07-23). RequestTimeout used to be appended just above
// CaptureRoute — the INNERMOST cross-cutting wrapper — so every
// credential/quota/limit middleware (Auth, KeyPolicy,
// RequireEmailVerified, MonthlyQuota, RateLimit, UsageTracker,
// SessionAuth) ran OUTSIDE it. Their Redis/Postgres round-trips were
// bounded only by go-redis's 3s default and the http.Server WriteTimeout
// (which cancels nothing in flight), while the placement comment claimed
// "EVERY handler inherits a deadline".
//
// Auth is the OUTERMOST member of that block and KeyPolicy the innermost
// exposed seam; both must observe a request context that already carries
// the deadline.
func TestRequestTimeout_BoundsThePreHandlerStack(t *testing.T) {
	authProbe, policyProbe := &deadlineProbe{}, &deadlineProbe{}

	srv := New(Options{
		Auth:           authProbe.middleware,
		KeyPolicy:      policyProbe.middleware,
		RequestTimeout: 3 * time.Second,
	})

	rec := httptest.NewRecorder()
	// An existing, always-mounted route — no new registration, so the
	// route↔OpenAPI inventory lint stays satisfied.
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if !authProbe.saw {
		t.Error("the Auth middleware saw NO deadline: the whole credential/quota/limit block " +
			"runs outside RequestTimeout, so its Redis/Postgres I/O is unbounded by it")
	}
	if !policyProbe.saw {
		t.Error("the KeyPolicy middleware saw NO deadline")
	}
	if authProbe.saw && (authProbe.remaining <= 0 || authProbe.remaining > 3*time.Second) {
		t.Errorf("Auth deadline is %v out, want (0, 3s] — it must be the request timeout, not something looser",
			authProbe.remaining)
	}
}

// TestRequestTimeout_StreamingStaysExempt — SSE routes are long-lived by
// design; moving the timeout outward must not start severing them. The
// exemption is path-suffix based, so an unmounted `/stream` path
// exercises it without registering a route.
func TestRequestTimeout_StreamingStaysExempt(t *testing.T) {
	probe := &deadlineProbe{}
	srv := New(Options{Auth: probe.middleware, RequestTimeout: 3 * time.Second})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/anything/stream", nil))
	if probe.saw {
		t.Error("an SSE path inherited a request deadline — a real stream would be severed mid-flight")
	}
}
