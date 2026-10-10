package customerwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// nopStore is a do-nothing DeliveryStore so New() can be exercised in
// internal (white-box) tests that only need to read back the applied
// Options defaults.
type nopStore struct{}

func (nopStore) ListPendingDeliveries(context.Context, int) ([]platform.WebhookDelivery, error) {
	return nil, nil
}

func (nopStore) GetWebhook(context.Context, uuid.UUID) (platform.CustomerWebhook, error) {
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (nopStore) MarkDelivered(context.Context, uuid.UUID, int) error { return nil }

func (nopStore) MarkAttemptFailed(context.Context, uuid.UUID, string, int, time.Time) error {
	return nil
}

// TestBatchLimit_WorstCaseSerialUnderLease is the MEDIUM double-delivery
// guard. The worker drains a batch SERIALLY, and the
// store leases each claimed row for storeLeaseDuration (5m). If the
// worst-case serial batch time (BatchLimit × per-request Timeout) can
// exceed that lease, a second worker re-claims the un-processed tail and
// DOUBLE-delivers. This pins the default BatchLimit × Timeout comfortably
// under the 5-minute lease; a regression that bumps BatchLimit back to
// 100 (100 × 10s = 1000s ≫ 300s) fails here.
func TestBatchLimit_WorstCaseSerialUnderLease(t *testing.T) {
	w := New(nopStore{}, Options{})

	if w.opts.BatchLimit != defaultBatchLimit {
		t.Errorf("default BatchLimit = %d, want %d", w.opts.BatchLimit, defaultBatchLimit)
	}
	worst := time.Duration(w.opts.BatchLimit) * w.opts.HTTPClient.Timeout
	if worst >= storeLeaseDuration {
		t.Fatalf("worst-case serial batch %s (BatchLimit=%d × Timeout=%s) must stay under the %s store lease; "+
			"a batch that outlives its lease is re-claimed by a second worker and double-delivered",
			worst, w.opts.BatchLimit, w.opts.HTTPClient.Timeout, storeLeaseDuration)
	}
}

// TestScheduleRetry_ExponentialBackoff pins the retry schedule (30s, 1m,
// 2m, 4m, … doubling, capped at 1h) and the INFO overflow clamp. Every
// delay is now full-jittered into [cap/2, cap], so we assert band
// membership rather than an exact value. A regression that dropped the
// cap, mis-shifted the exponent, or let `base << n` wrap int64 for a
// large attempt number would push a delay out of band and fail here.
func TestScheduleRetry_ExponentialBackoff(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	w := &Worker{opts: Options{Clock: func() time.Time { return base }}}

	const hour = time.Hour
	cases := []struct {
		attempt int
		cap     time.Duration // deterministic (pre-jitter) backoff ceiling
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 16 * time.Minute},
		{7, 32 * time.Minute}, // last rung below the cap
		{8, hour},             // 64m clamped down to the 1h ceiling
		{28, hour},            // large-but-non-overflowing shift → still clamped
		{30, hour},            // INFO: shift clamp prevents int64 wrap → still 1h
		{100, hour},           // arbitrarily large attempt → clamp holds, never wraps
	}
	for _, tc := range cases {
		lo, hi := tc.cap/2, tc.cap
		// Sample repeatedly: a bug that ignored the attempt number or
		// wrapped to a negative delay would have to land in-band every
		// time to escape detection.
		for i := 0; i < 32; i++ {
			got := w.scheduleRetry(tc.attempt).Sub(base)
			if got < lo || got > hi {
				t.Fatalf("scheduleRetry(%d) delay = %s, want within jitter band [%s, %s]",
					tc.attempt, got, lo, hi)
			}
		}
	}
}

// TestScheduleRetry_Jitter is the LOW thundering-herd guard: the retry
// delay is jittered, so two calls at the SAME attempt land in the
// full-jitter band [delay/2, delay] AND vary across samples. Without
// jitter, scheduleRetry(8) returns exactly 1h every time — the
// distinct-value assertion below would then see one value and fail,
// which is what makes this test non-vacuous.
func TestScheduleRetry_Jitter(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	w := &Worker{opts: Options{Clock: func() time.Time { return base }}}

	const attempt = 8 // 1h rung → widest band [30m, 1h]
	lo, hi := 30*time.Minute, time.Hour

	seen := map[time.Duration]struct{}{}
	const samples = 64
	for i := 0; i < samples; i++ {
		got := w.scheduleRetry(attempt).Sub(base)
		if got < lo || got > hi {
			t.Fatalf("jittered delay %s outside band [%s, %s]", got, lo, hi)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("expected jittered delays to vary across %d samples, got %d distinct value(s) — "+
			"jitter absent (thundering-herd risk)", samples, len(seen))
	}
}

// TestSignHMACSHA256_BindsTimestamp is the replay-defence
// guard: the signature MUST cover "<unix_ts>." + body, not the body
// alone (a body-only signature made a captured delivery replayable
// forever). We recompute the expected HMAC and assert the exact
// value, then assert that a body-only signature differs — proving
// the timestamp is genuinely mixed in.
func TestSignHMACSHA256_BindsTimestamp(t *testing.T) {
	secret := []byte("whsec_test_key")
	ts := int64(1_751_720_400)
	payload := []byte(`{"event":"key.mint","id":"abc"}`)

	got := signHMACSHA256(secret, ts, payload)

	// Independent recomputation of the documented scheme.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}

	// Body-only HMAC (the replayable shape) must NOT match — this is
	// the whole point of the timestamped construction.
	bodyOnly := hmac.New(sha256.New, secret)
	bodyOnly.Write(payload)
	if got == hex.EncodeToString(bodyOnly.Sum(nil)) {
		t.Error("signature equals body-only HMAC; timestamp is not bound (CS-055 regression)")
	}

	// Different timestamp / secret → different signature; determinism holds.
	if got == signHMACSHA256(secret, ts+1, payload) {
		t.Error("changing the timestamp did not change the signature")
	}
	if got == signHMACSHA256([]byte("other"), ts, payload) {
		t.Error("changing the secret did not change the signature")
	}
	if got != signHMACSHA256(secret, ts, payload) {
		t.Error("signature is not deterministic for identical inputs")
	}
}

// TestWorker_retryClassification pins retry classification.
//
// A 4xx must not be terminal on the FIRST attempt: a 429 from any
// endpoint behind a rate-limiting gateway permanently destroyed the
// event — next_attempt_at NULL, dropped out of the pending predicate,
// with no dead-letter, no retry and no alert (the rules match only
// server_error/network_error/exhausted, so client_error is unmonitored).
// A SEV-1 notification coincident with a freeze burst was lost that way.
//
// Conversely 3xx was classified "transient" and retried 15 times over
// ~8h even though redirects are deliberately disabled by the SSRF
// defence, so it could never succeed — and the recorded reason said
// "transient", which is false.
func TestWorker_retryClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		wantRetry bool
		why       string
	}{
		{http.StatusTooManyRequests, true, "429 is the canonical back-off signal"},
		{http.StatusRequestTimeout, true, "408 is temporary by definition"},
		{http.StatusTooEarly, true, "425 means retry later"},
		{http.StatusUnauthorized, false, "401 needs the customer to act"},
		{http.StatusNotFound, false, "404 is a wrong URL"},
		{http.StatusUnprocessableEntity, false, "422 is validation"},
		{http.StatusPermanentRedirect, false, "redirects are not followed, so a 3xx can never succeed"},
		{http.StatusFound, false, "same for a 302"},
	} {
		if got := isRetryable4xx(tc.status); got != tc.wantRetry && tc.status < 300 {
			t.Errorf("isRetryable4xx(%d) = %v, want %v — %s", tc.status, got, tc.wantRetry, tc.why)
		}
		// 3xx must not be classified retryable by the 4xx helper either.
		if tc.status >= 300 && tc.status < 400 && isRetryable4xx(tc.status) {
			t.Errorf("isRetryable4xx(%d) = true, want false — %s", tc.status, tc.why)
		}
	}
}

// TestNew_DefaultClientWiresSSRFGuardAndRefusesRedirects pins the
// WIRING of the delivery-time SSRF defence, not just its
// predicate.
//
// ssrf_test.go tests ssrfGuardedDialContext rigorously — 8 blocked
// ranges, error text asserted. Nothing asserted that the default HTTP
// client actually USES it, so deleting `DialContext:
// ssrfGuardedDialContext` from the transport left the whole repo's
// `go test ./...` green. The webhook URL is
// customer-supplied, and registration-time validation cannot catch DNS
// rebinding between registration and delivery — which is the entire
// reason the delivery-time guard exists.
//
// Also pins CheckRedirect: without it a 302 from a public host to an
// internal one is followed automatically, routing around the dialer.
func TestNew_DefaultClientWiresSSRFGuardAndRefusesRedirects(t *testing.T) {
	w := New(nopStore{}, Options{})
	c := w.opts.HTTPClient
	if c == nil {
		t.Fatal("New did not construct a default HTTP client")
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", c.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("default transport has a nil DialContext — the SSRF guard is not wired, " +
			"so every assertion in ssrf_test.go is decorative")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := tr.DialContext(ctx, "tcp", "169.254.169.254:80")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("dial to the cloud metadata endpoint succeeded — the SSRF guard is not wired")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("dial(169.254.169.254:80) = %v, want an SSRF-defence refusal", err)
	}

	if c.CheckRedirect == nil {
		t.Fatal("default client follows redirects — a 302 from a public host to an internal " +
			"one would route around the dialer")
	}
	if rerr := c.CheckRedirect(nil, nil); !errors.Is(rerr, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", rerr)
	}
}

// TestNew_RejectsOptionsThatBreakTheLeaseInvariant pins that the
// compile-time guard beside the defaults (storeLeaseDuration vs
// defaultBatchLimit/defaultHTTPTimeout/markWriteTimeout) only protects the
// DEFAULT values. A caller-supplied BatchLimit or HTTPClient.Timeout that
// pushes the worst-case serial batch time at or past the store's claim
// lease must be refused at construction, not silently accepted to double-
// deliver in production once the lease expires mid-batch.
func TestNew_RejectsOptionsThatBreakTheLeaseInvariant(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New did not panic on a BatchLimit/Timeout combination that exceeds the store's claim lease")
		}
	}()
	// 100 rows x (10s default timeout + 1s markWriteTimeout) = 1100s,
	// far past the 5-minute (300s) storeLeaseDuration.
	New(nopStore{}, Options{BatchLimit: 100})
}

// TestNew_AcceptsOptionsWithinTheLeaseInvariant is the control: a
// BatchLimit comfortably under the lease must construct cleanly.
func TestNew_AcceptsOptionsWithinTheLeaseInvariant(t *testing.T) {
	w := New(nopStore{}, Options{BatchLimit: 5})
	if w == nil {
		t.Fatal("New returned nil for an in-invariant BatchLimit")
	}
}

// panicOnOneStore is a DeliveryStore whose GetWebhook panics for one
// specific webhook id and answers normally for everything else. Models a
// future decode edge case or a bad row surfacing as a panic partway
// through a batch.
type panicOnOneStore struct {
	webhooks  map[uuid.UUID]platform.CustomerWebhook
	pending   []platform.WebhookDelivery
	panicOn   uuid.UUID
	delivered []uuid.UUID
}

func (s *panicOnOneStore) ListPendingDeliveries(context.Context, int) ([]platform.WebhookDelivery, error) {
	return s.pending, nil
}

func (s *panicOnOneStore) GetWebhook(_ context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	if id == s.panicOn {
		panic("simulated panic: this delivery's row is corrupt")
	}
	if w, ok := s.webhooks[id]; ok {
		return w, nil
	}
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (s *panicOnOneStore) MarkDelivered(_ context.Context, id uuid.UUID, _ int) error {
	s.delivered = append(s.delivered, id)
	return nil
}

func (s *panicOnOneStore) MarkAttemptFailed(context.Context, uuid.UUID, string, int, time.Time) error {
	return nil
}

func (*panicOnOneStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}

// TestTick_PanicInOneDeliveryDoesNotStopTheBatch pins the recover(): tick had no
// recover() anywhere between it and deliverOne, so a panic on one row
// (a decode edge case, a nil dereference) killed the poll loop for the
// rest of the process — every OTHER pending delivery, including ones
// already claimed in the same batch, silently stopped being drained.
func TestTick_PanicInOneDeliveryDoesNotStopTheBatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	badID := uuid.New()
	goodID := uuid.New()
	goodDeliveryID := uuid.New()
	store := &panicOnOneStore{
		webhooks: map[uuid.UUID]platform.CustomerWebhook{
			goodID: {ID: goodID, URL: ts.URL, SigningKey: []byte("secret"), Enabled: true},
		},
		panicOn: badID,
		pending: []platform.WebhookDelivery{
			{ID: uuid.New(), WebhookID: badID, EventType: "incident.sev1", Payload: []byte(`{}`)},
			{ID: goodDeliveryID, WebhookID: goodID, EventType: "incident.sev1", Payload: []byte(`{}`)},
		},
	}
	w := newWorker(store, Options{HTTPClient: &http.Client{Timeout: 5 * time.Second}}, unguardedClient)

	w.tick(context.Background())

	if len(store.delivered) != 1 || store.delivered[0] != goodDeliveryID {
		t.Fatalf("delivered = %v, want exactly the good delivery (%s) delivered despite the first row panicking", store.delivered, goodDeliveryID)
	}
}

// ctxHonouringStore is a DeliveryStore that treats its context the way
// a database driver does: a write attempted on a dead context fails
// with the context's error and changes nothing. The package's other
// fake ignores ctx entirely, which is why a mark write running on an
// already-expired context went unseen.
type ctxHonouringStore struct {
	mu sync.Mutex

	webhook platform.CustomerWebhook
	pending []platform.WebhookDelivery

	// lookupDelay is the store round-trip GetWebhook costs. The attempt
	// deadline starts BEFORE this and the HTTP client's timeout starts
	// after it, which is what guarantees the attempt context is the
	// first of the two to expire.
	lookupDelay time.Duration

	failed    []markedFailure
	delivered []int
	// markBudgets records, per outcome write, how long the write's
	// context had left (false = no deadline at all).
	markBudgets []markBudget
}

type markedFailure struct {
	status int
	next   time.Time
	msg    string
}

type markBudget struct {
	remaining time.Duration
	bounded   bool
}

func (s *ctxHonouringStore) ListPendingDeliveries(context.Context, int) ([]platform.WebhookDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out, nil
}

func (s *ctxHonouringStore) GetWebhook(ctx context.Context, _ uuid.UUID) (platform.CustomerWebhook, error) {
	select {
	case <-time.After(s.lookupDelay):
	case <-ctx.Done():
		return platform.CustomerWebhook{}, ctx.Err()
	}
	return s.webhook, nil
}

func (s *ctxHonouringStore) observe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dl, ok := ctx.Deadline()
	s.markBudgets = append(s.markBudgets, markBudget{remaining: time.Until(dl), bounded: ok})
	return nil
}

func (s *ctxHonouringStore) MarkDelivered(ctx context.Context, _ uuid.UUID, status int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.observe(ctx); err != nil {
		return err
	}
	s.delivered = append(s.delivered, status)
	return nil
}

func (s *ctxHonouringStore) MarkAttemptFailed(ctx context.Context, _ uuid.UUID, msg string, status int, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.observe(ctx); err != nil {
		return err
	}
	s.failed = append(s.failed, markedFailure{status: status, next: next, msg: msg})
	return nil
}

func newLifetimeStore(url string) *ctxHonouringStore {
	webhookID := uuid.New()
	return &ctxHonouringStore{
		lookupDelay: 20 * time.Millisecond,
		webhook: platform.CustomerWebhook{
			ID: webhookID, URL: url, Enabled: true,
			SigningKey: []byte("0123456789abcdef0123456789abcdef"), // gitleaks:allow — test fixture, not a credential
		},
		pending: []platform.WebhookDelivery{{
			ID: uuid.New(), WebhookID: webhookID,
			EventType: string(platform.WebhookEventIncidentSEV1),
			Payload:   []byte(`{"k":"v"}`),
		}},
	}
}

// TestTick_TimedOutPOSTStillRecordsTheAttempt exercises the webhook leg at
// the worker's real entry point (tick → deliverOne → handleFailure →
// the store write).
//
// A customer endpoint that hangs is the ordinary way a delivery fails.
// The attempt context's deadline started before the webhook lookup and
// the POST, so by the time the POST had exhausted the client timeout the
// context was already dead, and the write recording the failed attempt
// ran on it and failed. attempt_count never advanced, so the row kept
// its claim lease and was re-POSTed every lease interval — into the same
// timeout, forever, with MaxAttempts powerless because nothing was ever
// counted.
func TestTick_TimedOutPOSTStillRecordsTheAttempt(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Never answers. The body is drained first because the server
		// only notices a client disconnect once it has been; `release`
		// is the belt to that pair of braces, so Close can never block.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hang.Close()
	defer close(release)

	store := newLifetimeStore(hang.URL)
	w := newWorker(store, Options{HTTPClient: &http.Client{Timeout: 60 * time.Millisecond}}, unguardedClient)

	markErrBefore := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error"))
	netErrBefore := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("network_error"))
	start := time.Now()
	w.tick(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failed) != 1 {
		t.Fatalf("recorded %d failed attempts, want 1 — the write recording a timed-out POST must land, "+
			"or the row keeps its lease and is re-POSTed forever (mark_error delta = %v)",
			len(store.failed),
			testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error"))-markErrBefore)
	}
	got := store.failed[0]
	// RSEC-Y1: the recorded message is the fixed, address-free reason —
	// not the raw dial/transport error text, which can name an internal
	// destination — so this only pins the status and outcome, not wording
	// that embeds the URL.
	if got.status != 0 || got.msg == "" {
		t.Errorf("recorded failure = %+v, want a status-0 network failure", got)
	}
	// Transient, first attempt: a retry must be SCHEDULED (backoff is
	// jittered into [15s, 30s]), not cleared.
	if wait := got.next.Sub(start); wait < 15*time.Second || wait > 31*time.Second {
		t.Errorf("next attempt in %v, want the first backoff step (15s–30s)", wait)
	}
	if d := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error")) - markErrBefore; d != 0 {
		t.Errorf("mark_error delta = %v, want 0", d)
	}
	if d := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("network_error")) - netErrBefore; d != 1 {
		t.Errorf("network_error delta = %v, want 1 — the outcome is counted only once the mark persisted", d)
	}
	assertMarkBudget(t, store.markBudgets)
}

// cancelOnResponse is a transport that answers 200 and, in the same
// breath, cancels the worker's context: the customer's endpoint has
// accepted the event at the instant the process is told to stop.
type cancelOnResponse struct{ cancel context.CancelFunc }

func (c cancelOnResponse) RoundTrip(*http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

// TestTick_ShutdownDuringDeliveryStillRecordsIt — the other half of the
// same wrong lifetime. The attempt context's cancellation is the
// worker's shutdown signal; an event the customer has ALREADY accepted
// must be recorded delivered, or it is sent to them a second time when
// the lease expires.
func TestTick_ShutdownDuringDeliveryStillRecordsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newLifetimeStore("http://customer.invalid/hook")
	w := newWorker(store, Options{HTTPClient: &http.Client{
		Timeout:   time.Second,
		Transport: cancelOnResponse{cancel: cancel},
	}}, unguardedClient)

	markErrBefore := testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error"))
	w.tick(ctx)
	if ctx.Err() == nil {
		t.Fatal("precondition: the transport never ran, so nothing cancelled the worker context")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.delivered) != 1 || store.delivered[0] != http.StatusOK {
		t.Fatalf("delivered = %v, want [200] — the customer accepted the event; losing the mark to a "+
			"shutdown re-sends it (mark_error delta = %v)", store.delivered,
			testutil.ToFloat64(obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error"))-markErrBefore)
	}
	assertMarkBudget(t, store.markBudgets)
}

// assertMarkBudget — detaching the write from the attempt must not
// leave it unbounded: a hung store would stall the serial batch past
// the claim lease. Every outcome write carries its own deadline, fresh
// (not the remnant of the attempt's) and no longer than markWriteTimeout.
func assertMarkBudget(t *testing.T, budgets []markBudget) {
	t.Helper()
	if len(budgets) == 0 {
		t.Fatal("no outcome write reached the store on a live context")
	}
	for i, b := range budgets {
		if !b.bounded {
			t.Errorf("outcome write %d ran with no deadline — a hung store would stall the batch past the lease", i)
			continue
		}
		if b.remaining > markWriteTimeout || b.remaining < markWriteTimeout/2 {
			t.Errorf("outcome write %d had %v left, want a fresh budget of at most %v", i, b.remaining, markWriteTimeout)
		}
	}
}

// TestBatchLimit_WorstCaseIncludesTheMarkWrite — the outcome write has
// its own deadline now, so it is a term in the worst-case serial batch
// time. The lease invariant has to hold with it included; the
// compile-time guard beside the defaults enforces the same sum.
func TestBatchLimit_WorstCaseIncludesTheMarkWrite(t *testing.T) {
	w := New(nopStore{}, Options{})
	worst := time.Duration(w.opts.BatchLimit) * (w.attemptTimeout() + markWriteTimeout)
	if worst >= storeLeaseDuration {
		t.Fatalf("worst-case serial batch %s (BatchLimit=%d × (attempt %s + mark %s)) must stay under the %s lease",
			worst, w.opts.BatchLimit, w.attemptTimeout(), markWriteTimeout, storeLeaseDuration)
	}
}

// A caller-supplied Options.HTTPClient must not be a way around the
// delivery-time SSRF guard: the guard belongs to every Worker, not only to
// the one built from the nil default.
func TestNew_SuppliedClientStillRefusesInternalDestinations(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := New(nopStore{}, Options{HTTPClient: &http.Client{Timeout: 2 * time.Second}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := w.opts.HTTPClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("POST to loopback %s through a caller-supplied client = %v, want an SSRF-defence refusal", ts.URL, err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("loopback receiver was hit %d times, want 0", n)
	}
	if got := w.opts.HTTPClient.Timeout; got != 2*time.Second {
		t.Errorf("worker client Timeout = %s, want the caller's 2s preserved", got)
	}
}

func TestNew_SuppliedClientCannotFollowRedirects(t *testing.T) {
	follow := func(*http.Request, []*http.Request) error { return nil }
	w := New(nopStore{}, Options{HTTPClient: &http.Client{CheckRedirect: follow}})
	c := w.opts.HTTPClient
	if c.CheckRedirect == nil {
		t.Fatal("worker client follows redirects — a 302 to an internal host would route around the dialer")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// A supplied Transport owns its own dialer (and proxy), so honouring it
// would bypass the guard; New must refuse it rather than drop it silently.
func TestNew_RejectsSuppliedTransport(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a caller-supplied Transport, whose dialer bypasses the SSRF guard")
		}
	}()
	New(nopStore{}, Options{HTTPClient: &http.Client{Transport: http.DefaultTransport}})
}
