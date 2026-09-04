package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var errAnomalyStoreBroke = errors.New("anomaly store: broke")

// deadAnomalyReader fails every read with a plain storage error — one that
// carries NO deadline signal of its own. That isolation is the point: it
// forces the status decision to come from the request context rather than
// from sniffing the error, which is exactly the distinction writeProblem's
// upgrade rests on.
type deadAnomalyReader struct{}

func (deadAnomalyReader) ListFreezeEvents(context.Context, bool, int) ([]timescale.FreezeEventRow, error) {
	return nil, errAnomalyStoreBroke
}

func (deadAnomalyReader) FreezeReasonCounts(context.Context, int) ([]timescale.FreezeReasonCount, error) {
	return nil, errAnomalyStoreBroke
}

func (deadAnomalyReader) FreezeDailyReasonCounts(context.Context, int) ([]timescale.FreezeDailyReasonCount, error) {
	return nil, errAnomalyStoreBroke
}

func (deadAnomalyReader) CountFiringFreezes(context.Context) (int64, error) {
	return 0, errAnomalyStoreBroke
}

// /v1/anomalies is the archetype of the ~50 handler error paths this
// covers: it hands r.Context() STRAIGHT to the store, so it has no
// per-call context for handlerTimedOut to inspect and no timeout branch
// of its own — the only deadline that can fire on it is the blanket
// middleware.RequestTimeout one, and its sole error path is
// `errors/internal` 500.
//
// That 500 contradicts the rule the rest of the API is written to (a
// server-side deadline is a RETRYABLE 503 — writeLendingReservesTimeout,
// the explorer's writeReadTimeout) and it is the rule the sla-probe
// scores availability_pct on: a 500 books a permanent failure for a
// condition a retry would have cleared, which is what made the 500 on
// /v1/issuers the sole SLA-harness blocker in #34.
func newAnomaliesServer() *Server {
	return &Server{
		anomalies: deadAnomalyReader{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func anomaliesRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil)
}

// expiredDeadline returns req with a request context whose DEADLINE has
// already passed — the state middleware.RequestTimeout leaves behind
// while the client is still connected.
func expiredDeadline(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(req.Context(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return req.WithContext(ctx)
}

func TestWriteProblem_ExpiredRequestDeadlineUpgrades500To503(t *testing.T) {
	rec := httptest.NewRecorder()
	newAnomaliesServer().handleAnomalies(rec, expiredDeadline(t, anomaliesRequest()))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a blown request deadline is retryable capacity, and a 500 "+
			"books it as a permanent availability failure (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (a transient timeout must not be cached and replayed)", cc)
	}
	if ra := rec.Header().Get("Retry-After"); ra != retryAfterRequestTimeout {
		t.Errorf("Retry-After = %q, want %q — a 503 telling the client to retry without saying when "+
			"is read as `retry now`, and the server has just run out of budget", ra, retryAfterRequestTimeout)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, rec.Body.String())
	}
	if p.Type != requestTimeoutType {
		t.Errorf("problem type = %q, want %q — the body must not still say `errors/internal` while "+
			"the status says 503", p.Type, requestTimeoutType)
	}
	if p.Status != http.StatusServiceUnavailable {
		t.Errorf("problem.status = %d, want 503 — the envelope's own status field must match the "+
			"HTTP status, or a client reading the body reaches the opposite conclusion", p.Status)
	}
}

// The other half of the contract: with the request context ALIVE, the
// same storage failure is still a 500. Without this, the upgrade above
// could be satisfied by turning every internal error into a 503 — which
// would hide real faults from the 5xx-class dashboards that separate
// "we are broken" from "we are slow".
func TestWriteProblem_LiveRequestKeepsInternalError500(t *testing.T) {
	rec := httptest.NewRecorder()
	newAnomaliesServer().handleAnomalies(rec, anomaliesRequest())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault on a request with budget left is a real "+
			"internal error (body %s)", rec.Code, rec.Body.String())
	}
}

// A CANCELLED request context is the client-abort case, not a deadline:
// the handler must return silently (clientAborted) and must NOT reach the
// upgrade. Pins the two done-nesses apart at the writeProblem level, the
// same split clientaborted_test.go pins at the predicate level.
func TestWriteProblem_CanceledRequestWritesNothing(t *testing.T) {
	req := anomaliesRequest()
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	rec := httptest.NewRecorder()
	newAnomaliesServer().handleAnomalies(rec, req.WithContext(ctx))

	if body := rec.Body.String(); body != "" {
		t.Errorf("wrote %q to a departed client; want nothing", body)
	}
}
