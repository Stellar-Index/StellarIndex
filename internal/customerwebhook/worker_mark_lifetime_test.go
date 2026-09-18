package customerwebhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// ctxHonouringStore is a DeliveryStore that treats its context the way
// a database driver does: a write attempted on a dead context fails
// with the context's error and changes nothing. The package's other
// fake ignores ctx entirely, which is why a mark write running on an
// already-expired context went unseen (K025).
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
			SecretHash: []byte("0123456789abcdef0123456789abcdef"), // gitleaks:allow — test fixture, not a credential
		},
		pending: []platform.WebhookDelivery{{
			ID: uuid.New(), WebhookID: webhookID,
			EventType: string(platform.WebhookEventIncidentSEV1),
			Payload:   []byte(`{"k":"v"}`),
		}},
	}
}

// TestTick_TimedOutPOSTStillRecordsTheAttempt is K025's webhook leg at
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
	w := New(store, Options{HTTPClient: &http.Client{Timeout: 60 * time.Millisecond}})

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
	if got.status != 0 || !strings.Contains(got.msg, "POST ") {
		t.Errorf("recorded failure = %+v, want a status-0 network failure naming the POST", got)
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
	w := New(store, Options{HTTPClient: &http.Client{
		Timeout:   time.Second,
		Transport: cancelOnResponse{cancel: cancel},
	}})

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
