// Package customerwebhook drains the platform.WebhookStore
// delivery queue. Each pending row gets HMAC-signed, POSTed to the
// customer's registered URL, and either marked delivered (2xx) or
// rescheduled with exponential backoff (transient failure / 5xx
// / network error). Permanent failures (4xx, attempt budget
// exhausted) leave the row with delivered_at unset + next_attempt_at
// unset so it drops out of the pending-listing predicate but
// remains visible in the dashboard delivery log.
//
// F-1270 (audit-2026-05-12). Architecture: one poll loop per
// process, configurable poll interval (default 5s), one HTTP
// client shared across deliveries.
//
// # Concurrency contract (AGT-06, audit-2026-07-23)
//
// The worker is safe to run alongside a second instance because the
// STORE guarantees at-most-one-in-flight delivery per row: the
// [DeliveryStore.ListPendingDeliveries] implementation claims rows with
// `FOR UPDATE SKIP LOCKED` and, in the same statement, pushes
// `next_attempt_at` 5 minutes out as a lease (F-1247, see
// internal/platform/postgresstore/webhook_store.go). Two workers never
// hand the same row to two HTTP POSTs, and a worker that dies
// mid-delivery releases the row when its lease expires.
//
// That guarantee is part of the DeliveryStore CONTRACT, not an
// implementation detail: any substitute implementation MUST claim-and-
// lease atomically. This docstring previously claimed the opposite —
// that no SKIP LOCKED dedup existed and double-delivery was merely
// "cosmetically harmless" — which was both false about the store and an
// invitation to write a replacement without the primitive the design
// depends on. MarkDelivered / MarkAttemptFailed idempotency is a
// second line of defence, not the first.
package customerwebhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// DeliveryStore is the worker's subset of [platform.WebhookStore].
// Pulled out so tests can substitute an in-memory fake without
// pulling the full CRUD surface.
//
// Implementations MUST honour two contracts the worker relies on:
//
//   - ListPendingDeliveries claims each returned row atomically and
//     leases it (postgres: `FOR UPDATE SKIP LOCKED` + `next_attempt_at =
//     now() + 5m` in one statement), so at most one worker has a given
//     delivery in flight and an abandoned claim self-heals when the
//     lease expires. See the package doc.
//   - GetWebhook returns [platform.ErrNotFound], and ONLY that, when the
//     row is genuinely absent. The worker treats not-found as terminal
//     and every other error as transient/retryable, so an implementation
//     that flattens transport failures into ErrNotFound would silently
//     discard deliveries (NTF-13).
type DeliveryStore interface {
	ListPendingDeliveries(ctx context.Context, limit int) ([]platform.WebhookDelivery, error)
	GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error)
	MarkDelivered(ctx context.Context, id uuid.UUID, responseStatus int) error
	MarkAttemptFailed(ctx context.Context, id uuid.UUID, errMsg string, responseStatus int, nextAttemptAt time.Time) error
}

// Worker tuning defaults and the safety invariant that binds them.
const (
	// defaultBatchLimit caps deliveries drained per poll. Kept low on
	// purpose — see the storeLeaseDuration invariant below.
	defaultBatchLimit = 25

	// defaultHTTPTimeout bounds a single delivery POST. It also bounds
	// each attempt's total store+HTTP work (see tick), so the worst-case
	// serial batch time is defaultBatchLimit × defaultHTTPTimeout.
	defaultHTTPTimeout = 10 * time.Second

	// storeLeaseDuration is the claim lease ListPendingDeliveries places
	// on each row (`next_attempt_at = now() + 5m`; see the package doc /
	// internal/platform/postgresstore/webhook_store.go). A row not
	// finished within the lease is re-claimed by a second worker and
	// DOUBLE-delivered. The worker processes a batch SERIALLY, so the
	// worst-case time to reach the last row is BatchLimit × Timeout; that
	// product MUST stay comfortably under this lease.
	storeLeaseDuration = 5 * time.Minute

	// maxDrainBytes caps how much of a response body we drain to reuse
	// the connection. We never read the body for content, so a hostile
	// or buggy endpoint must not tie the worker up streaming megabytes.
	maxDrainBytes = 64 << 10 // 64 KiB
)

// Compile-time guard for the two-worker double-delivery invariant
// (MEDIUM, F-1270 hardening): the worst-case serial batch time
// (defaultBatchLimit × defaultHTTPTimeout) must stay strictly under the
// store's claim lease. Raising either default past that point fails the
// build here rather than silently reintroducing the double-delivery
// race. 25 × 10s = 250s < 300s, with 50s of margin.
const _ = uint(storeLeaseDuration - defaultBatchLimit*defaultHTTPTimeout - time.Nanosecond)

// Options tunes the worker. Zero values yield production defaults.
type Options struct {
	// PollInterval between queue drains. Default 5s — tight
	// enough that SEV-1 deliveries feel real-time, loose enough
	// that an idle worker doesn't hammer postgres.
	PollInterval time.Duration

	// BatchLimit caps how many deliveries each poll drains.
	// Default 25 (defaultBatchLimit). The worker processes a batch
	// SERIALLY and the store leases each claimed row for
	// storeLeaseDuration (5m); a row not reached before its lease
	// expires is re-claimed by a second worker and DOUBLE-delivered.
	// INVARIANT: BatchLimit × HTTPClient.Timeout MUST stay under the
	// 5-minute store lease (default 25 × 10s = 250s < 300s). The
	// compile-time guard beside the defaults enforces it for the
	// default values; if you raise this, keep the product under the
	// lease. Higher values bias toward throughput at the cost of
	// postgres lock duration per cycle and shrink that safety margin.
	BatchLimit int

	// MaxAttempts before a delivery is marked permanently failed.
	// Default 15. With scheduleRetry's 30s→doubling→1h-capped backoff,
	// the 15-attempt schedule spans only ~8h of wall-clock — NOT the 72h
	// that migration 0027's docblock ("Stripe-style: signed deliveries,
	// exponential retry over 72h") and the CustomerWebhook struct doc
	// aspire to. That 72h figure is ASPIRATIONAL: reaching it would need
	// a larger MaxAttempts (and/or a higher backoff cap) and is a
	// customer-facing delivery-SLA decision — deliberately NOT changed
	// here, because silently bumping the attempt budget would move the
	// SLA under the operator's feet.
	MaxAttempts int

	// HTTPClient sends the actual POST. Default has a 10s
	// per-request timeout. Inject one in tests to capture the
	// request bodies.
	HTTPClient *http.Client

	// Logger receives the worker's structured logs. Default
	// slog.Default(). Worker logs at INFO on every delivery
	// (success + failure) so operators can dashboard the
	// activity; WARN on configuration drift (webhook not found).
	Logger *slog.Logger

	// Clock is the time source. Override in tests; production
	// uses time.Now.
	Clock func() time.Time
}

// Worker drains the pending-delivery queue.
type Worker struct {
	store  DeliveryStore
	opts   Options
	stopCh chan struct{}
	doneCh chan struct{}
	signFn func(secret []byte, ts int64, payload []byte) string
}

// New constructs a Worker. store must be non-nil; opts gets
// production defaults applied to every zero field.
func New(store DeliveryStore, opts Options) *Worker {
	if store == nil {
		panic("customerwebhook: New: store must not be nil")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = defaultBatchLimit
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 15
	}
	if opts.HTTPClient == nil {
		// F-1245 (codex audit-2026-05-12): SSRF defence at delivery
		// time. The dial hook re-resolves the host on every send so
		// DNS rebinding between registration and delivery is
		// caught here. Redirects are disabled via CheckRedirect to
		// stop a 302 from a public host pointing at an internal
		// destination from being followed automatically.
		opts.HTTPClient = &http.Client{
			Timeout: defaultHTTPTimeout,
			Transport: &http.Transport{
				DialContext: ssrfGuardedDialContext,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Worker{
		store:  store,
		opts:   opts,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		signFn: signHMACSHA256,
	}
}

// Run drives the poll loop until ctx is cancelled. Returns the
// context error on shutdown. Safe to call once; calling Run twice
// on the same Worker panics.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	defer close(w.doneCh)

	// Drain once immediately so a fresh worker doesn't wait one
	// full PollInterval before processing the existing backlog.
	w.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.stopCh:
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// Stop signals Run to exit cleanly. Returns after the in-flight
// tick (if any) completes.
func (w *Worker) Stop() {
	select {
	case <-w.stopCh:
		// already stopped
	default:
		close(w.stopCh)
	}
	<-w.doneCh
}

// tick drains one batch from the pending queue. Errors during the
// batch are logged + counted; the loop continues to the next
// delivery so one bad row doesn't stall the queue.
func (w *Worker) tick(ctx context.Context) {
	pending, err := w.store.ListPendingDeliveries(ctx, w.opts.BatchLimit)
	if err != nil {
		w.opts.Logger.Warn("customer-webhook: ListPendingDeliveries failed",
			"err", err)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("list_error").Inc()
		return
	}
	for _, d := range pending {
		// Bound each attempt to the per-request timeout so the worst-case
		// serial batch time stays BatchLimit × Timeout — the basis for the
		// BatchLimit-vs-lease invariant (see Options.BatchLimit). A single
		// stuck delivery can't blow the batch past the 5-minute store lease
		// and let a second worker re-claim the tail.
		attemptCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout())
		w.deliverOne(attemptCtx, d)
		cancel()
	}
}

// attemptTimeout is the per-delivery deadline: the HTTP client's own
// per-request timeout (default defaultHTTPTimeout). Bounding each attempt
// keeps the worst-case serial batch time at BatchLimit × Timeout.
func (w *Worker) attemptTimeout() time.Duration {
	if t := w.opts.HTTPClient.Timeout; t > 0 {
		return t
	}
	return defaultHTTPTimeout
}

// deliverOne processes a single delivery. POSTs the payload, signs
// it, and marks delivered/failed based on the response.
func (w *Worker) deliverOne(ctx context.Context, d platform.WebhookDelivery) {
	wh, err := w.store.GetWebhook(ctx, d.WebhookID)
	if err != nil {
		if !errors.Is(err, platform.ErrNotFound) {
			// NTF-13 (audit-2026-07-23): a TRANSIENT store failure — a
			// connection reset, a fail-over, a statement timeout — is not
			// evidence the webhook is gone. Terminally failing the row on
			// it (as this path used to, for every error alike) silently and
			// PERMANENTLY dropped a customer's delivery on a blip: the row
			// leaves the pending predicate and nothing ever revisits it.
			// Return WITHOUT marking, so the store's 5-minute claim lease
			// expires and the row is picked up again — the same recovery
			// path a worker crash uses.
			w.opts.Logger.Warn("customer-webhook: GetWebhook failed (transient); leaving delivery for lease expiry",
				"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
			obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("lookup_error").Inc()
			return
		}
		// Genuine not-found: the webhook was deleted between enqueue +
		// delivery. Mark the delivery terminally failed so it drops out
		// of the pending listing — retrying can never succeed.
		w.opts.Logger.Warn("customer-webhook: webhook not found; permanently failing delivery",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		_ = w.store.MarkAttemptFailed(ctx, d.ID,
			fmt.Sprintf("webhook lookup: %v", err), 0, time.Time{})
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("webhook_missing").Inc()
		return
	}
	if !wh.Enabled {
		// Webhook is disabled — silently terminate the delivery
		// rather than retry forever.
		_ = w.store.MarkAttemptFailed(ctx, d.ID,
			"webhook disabled", 0, time.Time{})
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("disabled").Inc()
		return
	}
	if len(wh.SecretHash) == 0 {
		// Defence-in-depth: a zero-length signing key yields a FORGEABLE
		// HMAC (anyone can compute HMAC("", body) for any payload). This is
		// unreachable via the API today (generateSecret always writes 32
		// crypto/rand bytes), but we refuse to emit a spoofable signature.
		// Treat it as a TERMINAL misconfiguration for this row — retrying
		// can never repair a missing secret — and never POST.
		w.opts.Logger.Warn("customer-webhook: empty signing secret; permanently failing delivery (never sign with an empty key)",
			"delivery_id", d.ID, "webhook_id", d.WebhookID)
		_ = w.store.MarkAttemptFailed(ctx, d.ID,
			"webhook signing secret is empty", 0, time.Time{})
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("no_secret").Inc()
		return
	}

	sigTS := w.opts.Clock().Unix()
	signature := w.signFn(wh.SecretHash, sigTS, d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(d.Payload))
	if err != nil {
		// URL malformed at request-build time. This is
		// permanently broken — the URL is set per-webhook by the
		// customer; we can't fix it by retrying.
		_ = w.store.MarkAttemptFailed(ctx, d.ID,
			fmt.Sprintf("build request: %v", err), 0, time.Time{})
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("build_error").Inc()
		// Build error: no HTTP roundtrip happened. Record a
		// near-zero duration so the histogram still has a sample
		// at this label and operators see the build_error bucket
		// populate rather than disappear.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("build_error").Observe(0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-StellarIndex-Event", d.EventType)
	req.Header.Set("X-StellarIndex-Timestamp", strconv.FormatInt(sigTS, 10))
	req.Header.Set("X-StellarIndex-Signature", "sha256="+signature)
	req.Header.Set("X-StellarIndex-Delivery-Id", d.ID.String())

	// Time the HTTP roundtrip + body drain. Recorded against the
	// outcome label so operators can chart p95/p99 latency
	// separately for delivered vs failure paths — a customer
	// endpoint that's slow but eventually 200s shows up in the
	// `delivered` bucket; a customer endpoint that's slow AND
	// 500s shows up in the `server_error` bucket. Helps isolate
	// "their endpoint is slow" from "we have a delivery problem".
	start := time.Now()
	resp, err := w.opts.HTTPClient.Do(req)
	if err != nil {
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("network_error").Observe(time.Since(start).Seconds())
		w.handleFailure(ctx, d, 0, fmt.Sprintf("POST %s: %v", wh.URL, err), "network_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body (capped) so the connection can be reused. We never
	// read it for content, so io.LimitReader stops a hostile or buggy
	// endpoint from streaming unbounded data at us within the timeout.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	elapsed := time.Since(start).Seconds()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("delivered").Observe(elapsed)
		if err := w.store.MarkDelivered(ctx, d.ID, resp.StatusCode); err != nil {
			w.opts.Logger.Warn("customer-webhook: MarkDelivered failed",
				"err", err, "delivery_id", d.ID)
			obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error").Inc()
			return
		}
		w.opts.Logger.Info("customer-webhook: delivered",
			"delivery_id", d.ID, "webhook_id", d.WebhookID,
			"event_type", d.EventType, "status", resp.StatusCode)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("delivered").Inc()
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 4xx is the customer's responsibility (auth, bad URL,
		// validation). Don't retry — they need to fix it.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("client_error").Observe(elapsed)
		w.handleFailure(ctx, d, resp.StatusCode,
			fmt.Sprintf("HTTP %d (4xx terminal)", resp.StatusCode), "client_error")
	default:
		// 5xx + 3xx → transient, retry with backoff.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("server_error").Observe(elapsed)
		w.handleFailure(ctx, d, resp.StatusCode,
			fmt.Sprintf("HTTP %d (transient)", resp.StatusCode), "server_error")
	}
}

// handleFailure routes a non-2xx response into the right
// MarkAttemptFailed call: terminal failures clear next_attempt_at;
// transient failures schedule the next try with exponential
// backoff capped at 1 hour.
func (w *Worker) handleFailure(ctx context.Context, d platform.WebhookDelivery, status int, msg, outcome string) {
	nextAttempt := w.scheduleRetry(d.AttemptCount + 1)
	if outcome == "client_error" || d.AttemptCount+1 >= w.opts.MaxAttempts {
		// Terminal: clear schedule so the row drops out of the
		// pending list.
		nextAttempt = time.Time{}
		if outcome != "client_error" {
			outcome = "exhausted"
		}
	}
	if err := w.store.MarkAttemptFailed(ctx, d.ID, msg, status, nextAttempt); err != nil {
		w.opts.Logger.Warn("customer-webhook: MarkAttemptFailed failed",
			"err", err, "delivery_id", d.ID)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error").Inc()
		return
	}
	w.opts.Logger.Info("customer-webhook: delivery failed",
		"delivery_id", d.ID, "webhook_id", d.WebhookID,
		"event_type", d.EventType, "status", status,
		"outcome", outcome, "attempt", d.AttemptCount+1,
		"next_attempt", nextAttempt)
	obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues(outcome).Inc()
}

// scheduleRetry returns the next attempt time given a 1-based attempt
// number. Exponential backoff — 30s, 1m, 2m, 4m, 8m, … capped at 1h —
// with bounded full-jitter so deliveries that fail in the same tick
// don't retry in synchronized waves. Caller decides whether to use this
// or to mark the delivery terminally failed.
func (w *Worker) scheduleRetry(nextAttempt int) time.Time {
	return w.opts.Clock().Add(w.backoffDelay(nextAttempt))
}

// backoffDelay is the jittered exponential-backoff delay for a 1-based
// attempt number. The shift exponent is clamped BEFORE the shift so the
// delay can never wrap int64 regardless of MaxAttempts (the previous
// `base << (n-1)` overflowed to a non-positive value for n ≳ 30). The
// deterministic delay is then jittered into [delay/2, delay].
func (w *Worker) backoffDelay(nextAttempt int) time.Duration {
	const (
		base    = 30 * time.Second
		maxWait = time.Hour
		// maxShift: 30s << 7 = 3840s already exceeds maxWait, so any
		// exponent at or above it clamps to maxWait. Clamping here keeps
		// the shift far below the int64 overflow point no matter how large
		// MaxAttempts is set.
		maxShift = 7
	)
	shift := nextAttempt - 1
	if shift < 0 {
		shift = 0
	}
	var delay time.Duration
	if shift >= maxShift {
		delay = maxWait
	} else {
		delay = base << shift
		if delay <= 0 || delay > maxWait {
			delay = maxWait
		}
	}
	return jitterDelay(delay)
}

// jitterDelay applies full-jitter to a backoff delay, returning a random
// duration in [delay/2, delay]. Spreading N simultaneous failures across
// the back half of the window stops them re-hammering a recovering
// endpoint in lockstep (thundering herd). Never returns a negative or
// zero delay for a positive input.
func jitterDelay(delay time.Duration) time.Duration {
	half := delay / 2
	if half <= 0 {
		return delay
	}
	// rand.Int64N(half+1) ∈ [0, half]; result ∈ [half, delay].
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// signHMACSHA256 produces the hex-encoded HMAC-SHA-256 signature over the
// timestamped payload `"<unix_ts>." + body` using `secret` (CS-055 —
// signing the body alone made a captured delivery replayable forever).
// Consumers verify by recomputing HMAC-SHA-256(secret, "<X-StellarIndex-
// Timestamp>." + rawBody), constant-time-comparing against the
// `X-StellarIndex-Signature: sha256=…` header, AND rejecting a timestamp
// outside a tolerance window (~5 min) to bound replay.
func signHMACSHA256(secret []byte, ts int64, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrAlreadyRunning is returned when Worker.Run is called more
// than once on the same Worker instance.
var ErrAlreadyRunning = errors.New("customerwebhook: worker already running")
