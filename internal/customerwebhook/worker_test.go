package customerwebhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fakeStore implements the worker's narrow DeliveryStore
// interface in-memory. Thread-safe so multiple ticks can interact
// with it.
type fakeStore struct {
	mu        sync.Mutex
	pending   []platform.WebhookDelivery
	webhooks  map[uuid.UUID]platform.CustomerWebhook
	delivered map[uuid.UUID]int    // delivery id → response status
	failures  map[uuid.UUID][]fail // delivery id → ordered fail records
	// getErr, when set, is returned by GetWebhook instead of consulting
	// the map — used to simulate a transient store failure (NTF-13).
	getErr error
	// markErr, when set, is returned by MarkAttemptFailed instead of
	// recording — used to simulate a store WRITE failure on a terminal
	// mark (NTF-WH-01).
	markErr error
}

type fail struct {
	msg      string
	status   int
	nextAt   time.Time
	terminal bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		webhooks:  map[uuid.UUID]platform.CustomerWebhook{},
		delivered: map[uuid.UUID]int{},
		failures:  map[uuid.UUID][]fail{},
	}
}

func (s *fakeStore) addWebhook(w platform.CustomerWebhook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhooks[w.ID] = w
}

func (s *fakeStore) enqueue(d platform.WebhookDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, d)
}

func (s *fakeStore) ListPendingDeliveries(_ context.Context, limit int) ([]platform.WebhookDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	// Drain so subsequent ticks don't re-fire on the same row.
	s.pending = nil
	return out, nil
}

func (s *fakeStore) GetWebhook(_ context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return platform.CustomerWebhook{}, s.getErr
	}
	if w, ok := s.webhooks[id]; ok {
		return w, nil
	}
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (s *fakeStore) MarkDelivered(_ context.Context, id uuid.UUID, responseStatus int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered[id] = responseStatus
	return nil
}

func (s *fakeStore) MarkAttemptFailed(_ context.Context, id uuid.UUID, errMsg string, status int, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		// Write failed: the row's outcome is NOT recorded (mirrors a
		// postgres UPDATE that errored before committing).
		return s.markErr
	}
	s.failures[id] = append(s.failures[id], fail{
		msg: errMsg, status: status, nextAt: next, terminal: next.IsZero(),
	})
	return nil
}

func makeWebhook(t *testing.T, url string, enabled bool) (uuid.UUID, []byte) {
	t.Helper()
	id := uuid.New()
	secret := []byte("test-secret-bytes")
	_ = id
	return id, secret
}

func runOneTick(t *testing.T, store *fakeStore, opts customerwebhook.Options) {
	t.Helper()
	// Tests target httptest.NewServer URLs on 127.0.0.1, which the
	// production SSRF-guard would reject. F-1245 (codex
	// audit-2026-05-12): supply a permissive http.Client when
	// callers haven't already, so the test suite can still verify
	// retry / status / signature behaviour against the local
	// server.
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	w := customerwebhook.New(store, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)
}

// TestWorker_DeliversOn2xx — the happy path: webhook URL returns
// 200, MarkDelivered is called with the response code, and the
// HMAC signature header is set.
func TestWorker_DeliversOn2xx(t *testing.T) {
	var (
		gotSignature string
		gotTimestamp string
		gotEventHdr  string
		gotBody      []byte
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-StellarIndex-Signature")
		gotTimestamp = r.Header.Get("X-StellarIndex-Timestamp")
		gotEventHdr = r.Header.Get("X-StellarIndex-Event")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID, secret := makeWebhook(t, ts.URL, true)
	store.addWebhook(platform.CustomerWebhook{
		ID:         webhookID,
		URL:        ts.URL,
		SecretHash: secret,
		Enabled:    true,
	})
	deliveryID := uuid.New()
	payload := []byte(`{"hello":"world"}`)
	store.enqueue(platform.WebhookDelivery{
		ID:            deliveryID,
		WebhookID:     webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       payload,
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if status, ok := store.delivered[deliveryID]; !ok || status != 200 {
		t.Errorf("delivery not marked OK: delivered=%v", store.delivered)
	}

	// CS-055: the signature is over "<timestamp>." + body, and the
	// timestamp is sent in X-StellarIndex-Timestamp so the consumer can
	// bound replay. Verify both.
	if gotTimestamp == "" {
		t.Error("X-StellarIndex-Timestamp header missing")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(gotTimestamp))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSignature != want {
		t.Errorf("signature header = %q, want %q", gotSignature, want)
	}
	if gotEventHdr != string(platform.WebhookEventIncidentSEV1) {
		t.Errorf("event header = %q", gotEventHdr)
	}
	if string(gotBody) != string(payload) {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
}

// TestWorker_5xxRetryThenSchedules — non-2xx (5xx) marks the
// delivery as attempt-failed + schedules a retry (nextAt non-zero).
func TestWorker_5xxRetryThenSchedules(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID, secret := makeWebhook(t, ts.URL, true)
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: ts.URL, SecretHash: secret, Enabled: true,
	})
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType:     string(platform.WebhookEventAnomalyFreeze),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failures[deliveryID]) != 1 {
		t.Fatalf("expected 1 failure record, got %d", len(store.failures[deliveryID]))
	}
	f := store.failures[deliveryID][0]
	if f.status != 503 {
		t.Errorf("recorded status = %d, want 503", f.status)
	}
	if f.terminal {
		t.Errorf("5xx should schedule retry, not terminal")
	}
}

// TestWorker_4xxIsTerminal — 4xx responses don't retry. The
// customer's URL is broken (auth, validation); they need to fix
// it.
func TestWorker_4xxIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID, secret := makeWebhook(t, ts.URL, true)
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: ts.URL, SecretHash: secret, Enabled: true,
	})
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType:     string(platform.WebhookEventDivergenceFiring),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	f := store.failures[deliveryID][0]
	if !f.terminal {
		t.Errorf("4xx must be terminal; failure record: %+v", f)
	}
}

// TestWorker_DisabledWebhookTerminates — when the registry row's
// Enabled=false, the worker silently terminates the delivery
// rather than retry forever.
func TestWorker_DisabledWebhookTerminates(t *testing.T) {
	store := newFakeStore()
	webhookID, secret := makeWebhook(t, "https://wherever.example", false)
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: "https://wherever.example",
		SecretHash: secret, Enabled: false, // disabled
	})
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failures[deliveryID]) != 1 {
		t.Fatalf("expected 1 failure record on disabled webhook, got %d", len(store.failures[deliveryID]))
	}
	if !store.failures[deliveryID][0].terminal {
		t.Errorf("disabled webhook must terminate the delivery")
	}
	if _, ok := store.delivered[deliveryID]; ok {
		t.Errorf("disabled webhook MUST NOT mark delivered")
	}
}

// TestWorker_MissingWebhookTerminates — webhook row was deleted
// between enqueue + delivery. Mark terminal so the queue doesn't
// retry forever.
func TestWorker_MissingWebhookTerminates(t *testing.T) {
	store := newFakeStore()
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: uuid.New(), // not in store.webhooks
		EventType:     string(platform.WebhookEventIncidentResolved),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failures[deliveryID]) != 1 {
		t.Fatalf("expected 1 failure record on missing webhook, got %d", len(store.failures[deliveryID]))
	}
	if !store.failures[deliveryID][0].terminal {
		t.Errorf("missing webhook must terminate the delivery")
	}
}

// TestWorker_EmptySecret_TerminalNoDelivery is the defence-in-depth guard
// for an empty HMAC signing key. signHMACSHA256 with a zero-length secret
// produces a FORGEABLE HMAC (anyone can compute HMAC("", body) for any
// payload), so the worker must refuse to sign/POST and instead mark the
// row terminally failed (a missing secret is a misconfiguration that
// retrying can never repair) — never emit a spoofable signature.
//
// Not reachable via the API today (generateSecret always writes 32
// crypto/rand bytes), but a belt-and-suspenders gap. Pre-fix the worker
// signed with the empty key and POSTed, so the test server would receive
// the request and the row would be marked delivered.
func TestWorker_EmptySecret_TerminalNoDelivery(t *testing.T) {
	var posted int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&posted, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID := uuid.New()
	store.addWebhook(platform.CustomerWebhook{
		ID:         webhookID,
		URL:        ts.URL,
		SecretHash: []byte{}, // empty signing key
		Enabled:    true,
	})
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})

	if n := atomic.LoadInt32(&posted); n != 0 {
		t.Errorf("empty-secret webhook was POSTed %d time(s); must never sign/deliver with an empty key", n)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.delivered[deliveryID]; ok {
		t.Error("empty-secret delivery must not be marked delivered")
	}
	got := store.failures[deliveryID]
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 terminal failure for an empty-secret webhook, got %d", len(got))
	}
	if !got[0].terminal {
		t.Error("empty-secret webhook must fail TERMINALLY (zero next_attempt_at), not retry forever")
	}
}

// errorsIs keeps the errors import live for future expansion.
var _ = errors.Is

// TestWorker_DeliveryDurationMetricRecorded pins the wave-88
// (2026-05-13) latency-histogram wiring: a successful delivery
// produces a sample on
// `stellarindex_customer_webhook_delivery_duration_seconds`
// labelled `outcome="delivered"`. Without this test, a future
// refactor could silently delete the timing call without any
// signal — the existing TestWorker_DeliversOn2xx asserts the
// counter side but not the histogram.
//
// Uses CollectAndCount on the metric's WithLabelValues child so
// the assertion stays independent of bucket-by-bucket values
// (which depend on test-machine performance).
func TestWorker_DeliveryDurationMetricRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID, secret := makeWebhook(t, ts.URL, true)
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: ts.URL, SecretHash: secret, Enabled: true,
	})
	store.enqueue(platform.WebhookDelivery{
		ID:            uuid.New(),
		WebhookID:     webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	// Use obstest.HistogramSampleCount because
	// HistogramVec.WithLabelValues(...) returns a
	// prometheus.Observer (not Collector) — testutil.CollectAndCount
	// can't act on the per-label child directly. The helper sums
	// sample counts across every series matching the (label key,
	// value) pair, equivalent to the wire-format `_count` suffix.
	before := obstest.HistogramSampleCount(t, obs.CustomerWebhookDeliveryDurationSeconds, "outcome", "delivered")
	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})
	after := obstest.HistogramSampleCount(t, obs.CustomerWebhookDeliveryDurationSeconds, "outcome", "delivered")

	if after <= before {
		t.Errorf("delivery duration histogram did not advance: before=%d after=%d", before, after)
	}
}

// TestWorker_TransientGetWebhookError_LeavesDeliveryForRetry is the
// NTF-13 regression.
//
// The failure it encodes: GetWebhook fails with a TRANSPORT error — a
// connection reset, a fail-over, a statement timeout — while the webhook
// row is perfectly intact. Pre-fix the worker treated every GetWebhook
// error identically to "row deleted" and called MarkAttemptFailed with a
// zero next_attempt_at, which removes the row from the pending predicate
// FOREVER. One postgres blip therefore silently and permanently dropped
// a customer's SEV-1 / freeze / divergence notification, with no retry
// and no alert path other than reading the delivery log by hand.
//
// Post-fix only platform.ErrNotFound is terminal; a transient error
// leaves the row untouched so the store's 5-minute claim lease expires
// and the next poll re-delivers it.
func TestWorker_TransientGetWebhookError_LeavesDeliveryForRetry(t *testing.T) {
	store := newFakeStore()
	webhookID := uuid.New()
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: "https://customer.example.com/hook",
		SecretHash: []byte("test-secret-bytes"), Enabled: true,
	})
	// The row exists; the READ is what fails.
	store.getErr = errors.New("read tcp 10.0.0.2:5432: connection reset by peer")

	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.failures[deliveryID]; len(got) != 0 {
		t.Fatalf("delivery was marked failed (%+v) on a TRANSIENT store error; the row must be "+
			"left alone so the claim lease expires and it is retried (NTF-13)", got)
	}
	if _, ok := store.delivered[deliveryID]; ok {
		t.Error("delivery must not be marked delivered when the webhook lookup failed")
	}
}

// TestWorker_MissingWebhook_IsStillTerminal guards the other side of the
// NTF-13 split: a genuinely deleted webhook must still terminate the
// delivery, or the row retries until its attempt budget burns out.
func TestWorker_MissingWebhook_IsStillTerminal(t *testing.T) {
	store := newFakeStore() // no webhook registered → platform.ErrNotFound
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: uuid.New(),
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})

	store.mu.Lock()
	defer store.mu.Unlock()
	got := store.failures[deliveryID]
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 terminal failure record for a deleted webhook, got %d", len(got))
	}
	if !got[0].terminal {
		t.Error("a deleted webhook must fail the delivery TERMINALLY (zero next_attempt_at)")
	}
}

// TestWorker_TerminalMarkWriteError_SurfacesOnMarkErrorCounter pins
// NTF-WH-01: when a TERMINAL MarkAttemptFailed store write fails, the
// worker must not silently discard the error. The row keeps the claim
// lease ListPendingDeliveries set, so a dropped terminal mark re-POSTs
// the same request every lease interval with no attempt-budget advance;
// the only way to catch that loop is the mark_error counter. This drives
// the disabled-webhook terminal path with a failing store and asserts the
// failure lands on mark_error and NOT on the terminal "disabled" outcome
// (which never persisted).
func TestWorker_TerminalMarkWriteError_SurfacesOnMarkErrorCounter(t *testing.T) {
	store := newFakeStore()
	store.markErr = errors.New("UPDATE failed: cannot write to read-only replica")
	webhookID := uuid.New()
	store.addWebhook(platform.CustomerWebhook{
		ID:         webhookID,
		URL:        "https://hooks.example.com/x",
		SecretHash: []byte("test-secret-bytes"),
		Enabled:    false, // → terminal "disabled" path → markTerminal
	})
	deliveryID := uuid.New()
	store.enqueue(platform.WebhookDelivery{
		ID:            deliveryID,
		WebhookID:     webhookID,
		EventType:     string(platform.WebhookEventPriceAlert),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	markErrBefore := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error"))
	disabledBefore := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("disabled"))

	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})

	markErrDelta := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error")) - markErrBefore
	disabledDelta := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("disabled")) - disabledBefore

	if markErrDelta != 1 {
		t.Errorf("a failed terminal MarkAttemptFailed must advance the mark_error counter by 1, got delta %v", markErrDelta)
	}
	if disabledDelta != 0 {
		t.Errorf("the terminal 'disabled' outcome must NOT be counted when its mark write failed, got delta %v", disabledDelta)
	}
}
