package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestWriteJSON_DefaultEnvelopeShape pins the wire shape every
// 2xx handler relies on: 200 status, application/json
// Content-Type, AsOf populated, Sources omitted when empty,
// Flags present (zero-valued).
func TestWriteJSON_DefaultEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	before := time.Now().UTC()
	writeJSON(rec, map[string]any{"k": "v"}, Flags{})
	after := time.Now().UTC()

	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var got Envelope
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got.AsOf.Time().Before(before) || got.AsOf.Time().After(after) {
		t.Errorf("AsOf %v outside [%v, %v]", got.AsOf, before, after)
	}
	if got.Sources != nil {
		t.Errorf("Sources = %v, want nil (omitempty)", got.Sources)
	}
	if got.Pagination != nil {
		t.Errorf("Pagination = %v, want nil (omitempty)", got.Pagination)
	}
	// Flags zero-value MUST appear on the wire (no omitempty on
	// Stale/ReducedRedundancy/Triangulated/DivergenceWarning) so
	// clients can rely on the field always being present.
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal envelope: %v", err)
	}
	if !contains(body, []byte(`"flags":{`)) {
		t.Errorf("envelope body %q missing flags object", body)
	}
}

// TestWriteJSON_WithSourcesIncluded — when sources are supplied,
// they appear on the wire (non-omitempty path).
func TestWriteJSON_WithSourcesIncluded(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, "data", Flags{}, "binance", "kraken", "soroswap")

	var got Envelope
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Sources) != 3 {
		t.Fatalf("len(Sources) = %d, want 3", len(got.Sources))
	}
	if got.Sources[0] != "binance" || got.Sources[1] != "kraken" || got.Sources[2] != "soroswap" {
		t.Errorf("Sources = %v, want [binance kraken soroswap]", got.Sources)
	}
}

// TestWriteEnvelope_PreservesAsOf checks that a pre-set AsOf is
// honoured (writeEnvelopeStatus only fills in when zero). Handlers
// with their own clock — bucket-end timestamps, observed_at carry-
// forward — depend on this.
func TestWriteEnvelope_PreservesAsOf(t *testing.T) {
	custom := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := httptest.NewRecorder()
	writeEnvelope(rec, Envelope{
		Data: "x",
		AsOf: WireTime(custom),
	})

	var got Envelope
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.AsOf.Time().Equal(custom) {
		t.Errorf("AsOf = %v, want %v (writeEnvelope must preserve pre-set value)", got.AsOf, custom)
	}
}

// TestWriteEnvelope_FillsZeroAsOf — when AsOf is zero on the way
// in, writeEnvelopeStatus stamps it with now(). Mirrors what
// writeJSON does so handlers that pass an Envelope but forget to
// set AsOf still produce a valid response.
func TestWriteEnvelope_FillsZeroAsOf(t *testing.T) {
	rec := httptest.NewRecorder()
	before := time.Now().UTC()
	writeEnvelope(rec, Envelope{Data: "x"})
	after := time.Now().UTC()

	var got Envelope
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AsOf.IsZero() {
		t.Fatal("AsOf is zero — writeEnvelope should have filled it")
	}
	if got.AsOf.Time().Before(before) || got.AsOf.Time().After(after) {
		t.Errorf("AsOf %v outside [%v, %v]", got.AsOf, before, after)
	}
}

// TestWriteEnvelopeStatus_RespectsExplicitStatus — the 201
// path for /v1/account/keys POST relies on this. Adding a regression
// test pins it the 200→201 status.
func TestWriteEnvelopeStatus_RespectsExplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEnvelopeStatus(rec, http.StatusCreated, Envelope{Data: "k"})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
}

// TestWriteEnvelope_PreservesPagination checks that list endpoints
// (assets / markets / sources / oracle/latest) get their Pagination
// cursor through the envelope intact.
func TestWriteEnvelope_PreservesPagination(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEnvelope(rec, Envelope{
		Data:       []string{"a", "b"},
		AsOf:       WireTime(time.Unix(1700000000, 0).UTC()),
		Pagination: &Pagination{Next: "cursor-xyz"},
	})

	var got Envelope
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Pagination == nil || got.Pagination.Next != "cursor-xyz" {
		t.Errorf("Pagination = %+v, want {Next: cursor-xyz}", got.Pagination)
	}
}

// TestWriteProblem_RFC9457Shape pins the error wire contract per
// docs/reference/api-design.md §5: type/title/status mandatory,
// detail + instance + request_id present when populated.
func TestWriteProblem_RFC9457Shape(t *testing.T) {
	// Run inside RequestID middleware so RequestIDFrom returns a
	// non-empty value the writeProblem path will then echo back.
	var captured *Problem
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/test",
			"Test error",
			http.StatusBadRequest,
			"a thing went wrong",
		)
	})
	h := middleware.RequestID(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/test?x=1", nil)
	req.Header.Set("X-Request-ID", "fixed-id-1234")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	var p Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	captured = &p
	if captured.Type != "https://api.stellarindex.io/errors/test" {
		t.Errorf("Type = %q", captured.Type)
	}
	if captured.Title != "Test error" {
		t.Errorf("Title = %q", captured.Title)
	}
	if captured.Status != http.StatusBadRequest {
		t.Errorf("Status = %d", captured.Status)
	}
	if captured.Detail != "a thing went wrong" {
		t.Errorf("Detail = %q", captured.Detail)
	}
	if captured.Instance != "/v1/test?x=1" {
		t.Errorf("Instance = %q, want /v1/test?x=1", captured.Instance)
	}
	if captured.RequestID != "fixed-id-1234" {
		t.Errorf("RequestID = %q, want fixed-id-1234", captured.RequestID)
	}
}

// TestWriteProblem_401SetsWWWAuthenticate pins the RFC 7235 §3.1
// guarantee that every 401 advertises a Bearer challenge so
// programmatic clients can discover the accepted auth scheme.
// The header is also tested at the auth-middleware layer; this
// covers the handler-level writeProblem path used by /v1/account/*
// for not-yet-authenticated requests.
func TestWriteProblem_401SetsWWWAuthenticate(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required",
			http.StatusUnauthorized,
			"sign in",
		)
	})
	h := middleware.RequestID(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	if got == "" {
		t.Fatal("WWW-Authenticate header missing on 401")
	}
	if !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want it to advertise Bearer scheme", got)
	}
}

// TestWriteProblem_NonAuthDoesNotSetWWWAuthenticate guards the
// inverse: a 400 / 404 / 500 / 503 problem must NOT emit
// WWW-Authenticate (RFC 7235's MUST applies to 401 only). An unconditional
// helper would be wrong; this pin keeps the conditional in
// place.
func TestWriteProblem_NonAuthDoesNotSetWWWAuthenticate(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, r, "https://api.stellarindex.io/errors/x", "x", status, "x")
		})
		h := middleware.RequestID(inner)
		req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("status %d: WWW-Authenticate = %q, want empty", status, got)
		}
	}
}

func TestHandlerTimedOut(t *testing.T) {
	t.Run("err wraps DeadlineExceeded", func(t *testing.T) {
		ctx := context.Background()
		if !handlerTimedOut(ctx, context.DeadlineExceeded) {
			t.Error("handlerTimedOut(live ctx, DeadlineExceeded) = false, want true")
		}
	})
	t.Run("call ctx deadline fired, err is a pg cancel", func(t *testing.T) {
		// The key case: the driver returns its own
		// `canceling statement due to user request` error string
		// after our context.WithTimeout fires. errors.Is misses it
		// because the driver's PgError doesn't wrap context.DeadlineExceeded;
		// the per-call context Err() is the authoritative signal.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
		defer cancel()
		cancelErr := errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)")
		if !handlerTimedOut(ctx, cancelErr) {
			t.Error("handlerTimedOut(deadline-passed ctx, pg cancel) = false, want true")
		}
	})
	t.Run("call ctx canceled (not deadlined), arbitrary err", func(t *testing.T) {
		// context.Canceled (e.g. an explicit cancel()) is NOT a
		// timeout — the handler shouldn't 503 on this branch.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if handlerTimedOut(ctx, errors.New("downstream cancelled")) {
			t.Error("handlerTimedOut(canceled-not-timed-out ctx) = true, want false")
		}
	})
	t.Run("everything healthy", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if handlerTimedOut(ctx, errors.New("storage broke")) {
			t.Error("handlerTimedOut(live ctx, plain err) = true, want false")
		}
	})
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func TestWriteEnvelope_DegradedIsNoStoreAndNeverOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		degraded bool
		want     string
	}{
		{"healthy keeps the route band", false, "public, max-age=60, s-maxage=300"},
		{"degraded overrides the band", true, "no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := middleware.CacheControl(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, []string{}, Flags{Degraded: tc.degraded})
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pools", nil))
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
			if strings.Contains(rec.Body.String(), "degraded") {
				t.Errorf("internal marker leaked onto the wire: %s", rec.Body.String())
			}
		})
	}
}

func TestWriteProblem_OmitsReasonMember(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	writeProblem(rec, req, "https://api.stellarindex.io/errors/price-not-found", "x", http.StatusNotFound, "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["reason"]; ok {
		t.Errorf("non-withheld problem carries reason: %v", body["reason"])
	}
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
// /v1/issuers the sole SLA-harness blocker before the clientAborted
// guard shipped.
func newAnomaliesServer() *Server {
	return &Server{
		Options: Options{Anomalies: deadAnomalyReader{}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
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
