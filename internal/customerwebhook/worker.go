// Package customerwebhook drains the platform.WebhookStore delivery queue.
// Each pending row is HMAC-signed, POSTed to the customer's URL, and either marked delivered (2xx)
// or rescheduled with exponential backoff (5xx / network error). Permanent failures (4xx, attempt
// budget exhausted) leave delivered_at and next_attempt_at unset, so the row drops out of the
// pending listing but stays in the dashboard delivery log.
//
// # Concurrency contract
//
// A second instance is safe because the STORE guarantees at-most-one-in-flight delivery per row:
// [DeliveryStore.ListPendingDeliveries] claims rows with `FOR UPDATE SKIP LOCKED` and in the same
// statement pushes `next_attempt_at` 5 minutes out as a lease. A worker that dies mid-delivery
// releases the row at lease expiry. Any substitute DeliveryStore MUST claim and lease atomically,
// or two workers can POST the same row twice.
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// DeliveryStore is the worker's subset of [platform.WebhookStore].
// Implementations MUST honour two contracts the worker relies on:
//
//   - ListPendingDeliveries claims each returned row atomically and leases it, so at most one
//     worker has a given delivery in flight and an abandoned claim self-heals at lease expiry.
//   - GetWebhook returns [platform.ErrNotFound], and ONLY that, when the row is genuinely absent.
//     Not-found is terminal and every other error is retried, so flattening transport failures
//     into ErrNotFound would silently discard deliveries.
//
// It must ALSO implement [AccountStatusReader]; [New] enforces that at construction.
type DeliveryStore interface {
	ListPendingDeliveries(ctx context.Context, limit int) ([]platform.WebhookDelivery, error)
	GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error)
	MarkDelivered(ctx context.Context, id uuid.UUID, responseStatus int) error
	MarkAttemptFailed(ctx context.Context, id uuid.UUID, errMsg string, responseStatus int, nextAttemptAt time.Time) error
}

// AccountStatusReader is the account-level kill switch on the OUTBOUND side. WebhookAccountStatus
// returns the lifecycle status of the account that owns the webhook, or [platform.ErrNotFound]
// when the webhook is gone.
//
// The auth validator enforces the kill switch inbound; without a status read here a suspended or
// closed customer would keep RECEIVING data, so the worker re-reads it immediately before signing
// and POSTing. [New] refuses a store that cannot answer it (asserted at construction, not on
// [DeliveryStore], because production wiring passes a [platform.WebhookStore] interface value).
type AccountStatusReader interface {
	WebhookAccountStatus(ctx context.Context, webhookID uuid.UUID) (platform.AccountStatus, error)
}

// Worker tuning defaults and the safety invariant that binds them.
const (
	// defaultBatchLimit caps deliveries drained per poll. Kept low on
	// purpose — see the storeLeaseDuration invariant below.
	defaultBatchLimit = 25

	// defaultMaxAttempts is Options.MaxAttempts' default; see its doc.
	defaultMaxAttempts = 15

	// defaultConcurrency is Options.Concurrency's default.
	defaultConcurrency = 8

	// defaultHTTPTimeout bounds one attempt's webhook lookup + HTTP work. The outcome write has
	// its own markWriteTimeout, so worst-case serial batch time is
	// defaultBatchLimit × (defaultHTTPTimeout + markWriteTimeout).
	defaultHTTPTimeout = 10 * time.Second

	// storeLeaseDuration is the claim lease ListPendingDeliveries places on each row. A row not
	// finished within it is re-claimed by a second worker and DOUBLE-delivered. A batch may be all
	// one endpoint's serial rows, so worst-case time to the last row is
	// BatchLimit × (Timeout + markWriteTimeout) regardless of parallelism; that MUST stay under it.
	storeLeaseDuration = 5 * time.Minute

	// markWriteTimeout bounds the store write that records an attempt's outcome, on its OWN
	// deadline detached from the attempt's (see Worker.mark), so it is a term in the lease
	// invariant (2s of slack per row over 25 rows). A write that cannot land fails to mark_error
	// and the row is retried on lease expiry, as after a worker crash.
	markWriteTimeout = 1 * time.Second

	// maxDrainBytes caps how much of a response body we drain to reuse the connection; the body is
	// never read for content, so a hostile endpoint cannot tie the worker up streaming megabytes.
	maxDrainBytes = 64 << 10 // 64 KiB

	// webhookUserAgent lets the endpoint operator trace an unexpected POST back to us.
	webhookUserAgent = "stellar-index/webhooks (+https://stellarindex.io)"
)

// Compile-time guard for the two-worker double-delivery invariant: worst-case serial batch time
// (defaultBatchLimit × (defaultHTTPTimeout + markWriteTimeout)) must stay strictly under the
// store's claim lease, so raising any term fails the build. 25 × (10s + 1s) = 275s < 300s.
const _ = uint(storeLeaseDuration - defaultBatchLimit*(defaultHTTPTimeout+markWriteTimeout) - time.Nanosecond)

// Options tunes the worker. Zero values yield production defaults.
type Options struct {
	// PollInterval between queue drains. Default 5s.
	PollInterval time.Duration

	// BatchLimit caps how many deliveries each poll drains. Default 25.
	// INVARIANT: BatchLimit × (HTTPClient.Timeout + markWriteTimeout) MUST stay under the 5-minute
	// store lease, or a row not reached in time is re-claimed and DOUBLE-delivered. Concurrency
	// does not relax it (a batch can be all one endpoint's serial rows). New panics if it breaks.
	BatchLimit int

	// Concurrency caps how many ENDPOINTS a batch delivers to in parallel (default 8). An
	// endpoint's rows run serially in claim order, so it never has two POSTs in flight, and a
	// stalled endpoint holds one slot instead of the whole queue.
	Concurrency int

	// MaxAttempts before a delivery is marked permanently failed. Default 15: with backoffCeiling's
	// schedule the last retry lands ~4–8 h after the first failure, the customer-facing window the
	// runbooks quote (TestRetryWindowMatchesOperatorDocs); widening it is an SLA decision.
	MaxAttempts int

	// HTTPClient supplies the delivery client's Timeout (default 10s). New always sends through the
	// SSRF-guarded dialer, refuses redirects, and panics if a Transport is set.
	HTTPClient *http.Client

	// Logger receives structured logs (default slog.Default()): INFO per delivery, WARN on
	// configuration drift (webhook not found).
	Logger *slog.Logger

	// Clock is the time source. Override in tests; production
	// uses time.Now.
	Clock func() time.Time
}

// Worker drains the pending-delivery queue.
type Worker struct {
	store    DeliveryStore
	accounts AccountStatusReader
	opts     Options
	stopCh   chan struct{}
	doneCh   chan struct{}
	signFn   func(secret []byte, ts int64, payload []byte) string

	// running latches true on the first Run call and never resets, so
	// Run is one-shot for the Worker's lifetime (see Run's doc comment).
	running atomic.Bool
}

// New constructs a Worker. store must be non-nil and must implement
// [AccountStatusReader]; opts gets production defaults applied to every
// zero field.
func New(store DeliveryStore, opts Options) *Worker {
	return newWorker(store, opts, guardedClient)
}

// newWorker is New with the delivery-client builder injectable, so a test
// can target an httptest server on loopback without an exported bypass.
func newWorker(store DeliveryStore, opts Options, clientFor func(*http.Client) *http.Client) *Worker {
	if store == nil {
		panic("customerwebhook: New: store must not be nil")
	}
	accounts, ok := store.(AccountStatusReader)
	if !ok {
		// Fail closed at the earliest possible point. The alternative —
		// skipping the account check when the store cannot answer it —
		// silently reduces the account kill switch to inbound-only.
		panic("customerwebhook: New: store must implement AccountStatusReader (the account kill switch must not be bypassable)")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = defaultBatchLimit
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	opts.HTTPClient = clientFor(opts.HTTPClient)
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	// The compile-time guard only covers the DEFAULT BatchLimit and Timeout; caller Options can
	// raise either, so check the lease invariant at runtime and fail closed. effectiveTimeout
	// mirrors attemptTimeout()'s fallback: a zero Timeout still bounds each attempt at
	// defaultHTTPTimeout via the attempt context.
	effectiveTimeout := opts.HTTPClient.Timeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = defaultHTTPTimeout
	}
	if worst := time.Duration(opts.BatchLimit) * (effectiveTimeout + markWriteTimeout); worst >= storeLeaseDuration {
		panic(fmt.Sprintf(
			"customerwebhook: New: BatchLimit(%d) x (effective HTTPClient.Timeout(%s) + markWriteTimeout(%s)) = %s, must stay under the store's %s claim lease (storeLeaseDuration) or a row is re-claimed and double-delivered before this batch finishes it",
			opts.BatchLimit, effectiveTimeout, markWriteTimeout, worst, storeLeaseDuration))
	}
	return &Worker{
		store:    store,
		accounts: accounts,
		opts:     opts,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		signFn:   signHMACSHA256,
	}
}

// guardedClient builds the client every production Worker sends with. The dial hook re-resolves
// the host on every send, catching DNS rebinding; redirects are refused so a 302 from a public
// host cannot steer the POST to an internal one. A caller Transport carries its own dialer and
// proxy, so it is refused rather than silently discarded.
func guardedClient(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.Transport != nil {
		panic("customerwebhook: New: Options.HTTPClient.Transport must be nil (a caller transport bypasses the SSRF dial guard)")
	}
	guarded := *c
	guarded.Transport = &http.Transport{
		DialContext: ssrfGuardedDialContext,
		// Bound the idle pool: an unbounded one leaks a live socket per distinct
		// customer-controlled host for the worker's lifetime.
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 1,
	}
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &guarded
}

// Run drives the poll loop until ctx is cancelled and returns the context error. It is one-shot
// per Worker: any second call, concurrent or after return, yields ErrAlreadyRunning.
func (w *Worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
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
// tick (if any) completes. Safe to call before Run: doneCh is only
// closed by Run's own defer, so a Worker that was never Run has
// nothing to wait for.
func (w *Worker) Stop() {
	select {
	case <-w.stopCh:
		// already stopped
	default:
		close(w.stopCh)
	}
	if !w.running.Load() {
		return
	}
	<-w.doneCh
}

// tick drains one batch: one lane per endpoint, up to Options.Concurrency lanes at once, each
// serial in claim order, so one bad row or slow endpoint delays only its own lane. It returns
// when every lane has finished.
func (w *Worker) tick(ctx context.Context) {
	pending, err := w.store.ListPendingDeliveries(ctx, w.opts.BatchLimit)
	if err != nil {
		w.opts.Logger.Warn("customer-webhook: ListPendingDeliveries failed",
			"err", err)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("list_error").Inc()
		return
	}
	slots := make(chan struct{}, w.opts.Concurrency)
	var wg sync.WaitGroup
	for _, lane := range lanesByWebhook(pending) {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			// Release the slot and WaitGroup even on a panic, or tick blocks forever.
			defer func() {
				if r := recover(); r != nil {
					worker.Report(w.opts.Logger, "customer-webhook-lane", r)
				}
				<-slots
				wg.Done()
			}()
			w.deliverLane(ctx, lane)
		}()
	}
	wg.Wait()
}

// deliverLane delivers one endpoint's rows in order. Each attempt's deadline bounds the batch at
// BatchLimit × (Timeout + markWriteTimeout), the basis for the lease invariant (see
// Options.BatchLimit). It covers the lookup and POST only; the outcome write has its own (see
// Worker.mark) because this one is already spent exactly when the POST timed out.
func (w *Worker) deliverLane(ctx context.Context, lane []platform.WebhookDelivery) {
	for _, d := range lane {
		attemptCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout())
		w.deliverOneRecovered(attemptCtx, d)
		cancel()
	}
}

// lanesByWebhook groups a claimed batch by endpoint, keeping claim order
// both across lanes (by each endpoint's first row) and within each lane.
func lanesByWebhook(pending []platform.WebhookDelivery) [][]platform.WebhookDelivery {
	index := map[uuid.UUID]int{}
	var lanes [][]platform.WebhookDelivery
	for _, d := range pending {
		i, ok := index[d.WebhookID]
		if !ok {
			i = len(lanes)
			index[d.WebhookID] = i
			lanes = append(lanes, nil)
		}
		lanes[i] = append(lanes[i], d)
	}
	return lanes
}

// deliverOneRecovered isolates a single delivery's panic to that delivery. A top-level recover
// would keep the process alive but end the poll loop permanently, so one bad row would silently
// starve every OTHER pending delivery.
func (w *Worker) deliverOneRecovered(ctx context.Context, d platform.WebhookDelivery) {
	defer func() {
		if r := recover(); r != nil {
			w.opts.Logger.Error("customer-webhook: deliverOne panicked; delivery left for lease expiry, batch continues",
				"panic", r, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		}
	}()
	w.deliverOne(ctx, d)
}

// attemptTimeout is the per-delivery deadline: the HTTP client's own timeout (default
// defaultHTTPTimeout). With the separate outcome-write bound it caps a serial batch at
// BatchLimit × (Timeout + markWriteTimeout).
func (w *Worker) attemptTimeout() time.Duration {
	if t := w.opts.HTTPClient.Timeout; t > 0 {
		return t
	}
	return defaultHTTPTimeout
}

// lookupWebhook loads the delivery's webhook. False means DO NOT POST:
// the row has already been marked terminal or left for lease expiry.
func (w *Worker) lookupWebhook(ctx context.Context, d platform.WebhookDelivery) (platform.CustomerWebhook, bool) {
	wh, err := w.store.GetWebhook(ctx, d.WebhookID)
	switch {
	case err == nil:
		return wh, true
	case errors.Is(err, platform.ErrNotFound):
		// Deleted between enqueue and delivery; retrying can never succeed.
		w.opts.Logger.Warn("customer-webhook: webhook not found; permanently failing delivery",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.markTerminal(ctx, d, fmt.Sprintf("webhook lookup: %v", err), "webhook_missing")
	case errors.Is(err, platform.ErrWebhookKeyUnsealable) && !errors.Is(err, platform.ErrWebhookSealKeyMissing):
		// The key fails authentication under the configured seal key, so
		// no retry can sign this row. A missing seal key is the transient
		// case below: setting it repairs the row.
		w.opts.Logger.Warn("customer-webhook: signing key cannot be unsealed; permanently failing delivery",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.markTerminal(ctx, d, fmt.Sprintf("webhook lookup: %v", err), "no_secret")
	default:
		// A store blip or an unset seal key is not evidence the webhook is
		// gone: leave the row unmarked so its claim lease expires and it is
		// retried, the same recovery a worker crash takes.
		w.opts.Logger.Warn("customer-webhook: GetWebhook failed (transient); leaving delivery for lease expiry",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("lookup_error").Inc()
	}
	return platform.CustomerWebhook{}, false
}

// deliverOne processes a single delivery. POSTs the payload, signs
// it, and marks delivered/failed based on the response.
func (w *Worker) deliverOne(ctx context.Context, d platform.WebhookDelivery) {
	wh, ok := w.lookupWebhook(ctx, d)
	if !ok {
		return
	}
	if !wh.Enabled {
		// Webhook is disabled — silently terminate the delivery
		// rather than retry forever.
		w.markTerminal(ctx, d, "webhook disabled", "disabled")
		return
	}
	if !w.accountGateOpen(ctx, d) {
		// Account-level kill switch. The per-webhook `Enabled` flag above
		// is the CUSTOMER's switch; this is the OPERATOR's.
		// accountGateOpen has already recorded or parked the row.
		return
	}
	if len(wh.SigningKey) == 0 {
		// Defence-in-depth: a zero-length key yields a FORGEABLE HMAC (anyone can compute
		// HMAC("", body)). Terminal for this row, since retrying cannot repair a missing secret.
		w.opts.Logger.Warn("customer-webhook: empty signing secret; permanently failing delivery (never sign with an empty key)",
			"delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.markTerminal(ctx, d, "webhook signing secret is empty", "no_secret")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(d.Payload))
	if err != nil {
		// The customer-set URL is malformed; retrying cannot fix it.
		w.markTerminal(ctx, d, fmt.Sprintf("build request: %v", err), "build_error")
		// No roundtrip happened; observe zero so the build_error bucket still populates.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("build_error").Observe(0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webhookUserAgent)
	req.Header.Set("X-StellarIndex-Event", d.EventType)
	req.Header.Set("X-StellarIndex-Delivery-Id", d.ID.String())
	w.setSignatureHeaders(req.Header, wh, d)

	// Latency is recorded per outcome label so "their endpoint is slow" (delivered bucket) can be
	// told apart from "slow AND failing" (server_error bucket).
	start := time.Now()
	resp, err := w.opts.HTTPClient.Do(req)
	if err != nil {
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("network_error").Observe(time.Since(start).Seconds())
		// err can name the resolved address the SSRF guard blocked. last_error is served verbatim
		// to the customer, so store a fixed address-free reason and log the real error.
		w.opts.Logger.Warn("customer-webhook: delivery POST failed",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.handleFailure(ctx, d, 0, "network error contacting webhook URL", "network_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body (capped) so the connection can be reused; the cap stops a hostile endpoint
	// streaming unbounded data within the timeout.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	elapsed := time.Since(start).Seconds()

	w.classifyResponse(ctx, d, resp.StatusCode, elapsed)
}

// accountGateOpen reports whether the owning account permits this delivery. False means DO NOT
// POST: the row has already been recorded or parked here.
//
// The claim query withholds a non-active account's rows, but a batch drains serially, so an account
// suspended mid-batch has rows already in hand.
//
//   - not found: the webhook vanished since the lookup; terminal.
//   - status unreadable: fail CLOSED. Leave the row for lease expiry so a Postgres blip delays
//     deliveries rather than dropping them or POSTing to a just-suspended customer.
//   - closed: terminal; a closed account is not coming back.
//   - anything else non-active (suspended, or an unknown value): park for lease expiry.
//     Suspension is reversible, so destroying the backlog would lose events a reinstated
//     customer is owed.
func (w *Worker) accountGateOpen(ctx context.Context, d platform.WebhookDelivery) bool {
	status, err := w.accounts.WebhookAccountStatus(ctx, d.WebhookID)
	switch {
	case errors.Is(err, platform.ErrNotFound):
		w.opts.Logger.Warn("customer-webhook: webhook gone at account check; permanently failing delivery",
			"delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.markTerminal(ctx, d, "webhook lookup: account status: not found", "webhook_missing")
		return false
	case err != nil:
		w.opts.Logger.Warn("customer-webhook: account status unreadable; withholding delivery for lease expiry",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("lookup_error").Inc()
		return false
	}
	switch status {
	case platform.AccountActive:
		return true
	case platform.AccountClosed:
		w.opts.Logger.Warn("customer-webhook: owning account is closed; permanently failing delivery",
			"delivery_id", d.ID, "webhook_id", d.WebhookID, "account_status", string(status))
		// "disabled" is the terminal-by-policy label; the label set is a bounded registry in
		// internal/obs (metrics_seed_test.go). last_error carries the specific reason.
		w.markTerminal(ctx, d, "owning account is closed", "disabled")
		return false
	default:
		w.opts.Logger.Warn("customer-webhook: owning account is not active; withholding delivery",
			"delivery_id", d.ID, "webhook_id", d.WebhookID, "account_status", string(status))
		return false
	}
}

// classifyResponse routes a delivery's HTTP status into the delivered /
// retry / terminal outcomes. Split out of deliverOne so the status
// taxonomy reads as one table (and to stay under the funlen ceiling).
func (w *Worker) classifyResponse(ctx context.Context, d platform.WebhookDelivery, status int, elapsed float64) {
	switch {
	case status >= 200 && status < 300:
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("delivered").Observe(elapsed)
		if err := w.mark(ctx, func(markCtx context.Context) error {
			return w.store.MarkDelivered(markCtx, d.ID, status)
		}); err != nil {
			w.opts.Logger.Warn("customer-webhook: MarkDelivered failed",
				"err", err, "delivery_id", d.ID)
			obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error").Inc()
			return
		}
		w.opts.Logger.Info("customer-webhook: delivered",
			"delivery_id", d.ID, "webhook_id", d.WebhookID,
			"event_type", d.EventType, "status", status)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("delivered").Inc()
	case status >= 300 && status < 400:
		// Redirects are disabled as part of the SSRF defence, so a 3xx can never succeed on a
		// later attempt; terminal, not transient. A trailing-slash 308 is the common case.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("client_error").Observe(elapsed)
		w.handleFailure(ctx, d, status,
			fmt.Sprintf("HTTP %d (redirect not followed — register the final URL)", status),
			"client_error")
	case isRetryable4xx(status):
		// 408, 425 and 429 are temporary by spec, and 429 is what a rate-limiting gateway returns
		// under a burst; treating it as terminal would destroy the event on its first attempt.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("server_error").Observe(elapsed)
		w.handleFailure(ctx, d, status,
			fmt.Sprintf("HTTP %d (transient)", status), "server_error")
	case status >= 400 && status < 500:
		// The rest of 4xx is the customer's responsibility (auth, bad
		// URL, validation). Don't retry — they need to fix it.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("client_error").Observe(elapsed)
		w.handleFailure(ctx, d, status,
			fmt.Sprintf("HTTP %d (4xx terminal)", status), "client_error")
	default:
		// 5xx → transient, retry with backoff.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("server_error").Observe(elapsed)
		w.handleFailure(ctx, d, status,
			fmt.Sprintf("HTTP %d (transient)", status), "server_error")
	}
}

// isRetryable4xx reports whether a 4xx status is TEMPORARY (408, 425, 429) and expected to
// recover without any change on the customer's side. Everything else in 4xx (credentials, wrong
// URL, validation) needs the customer to act, so it stays terminal.
func isRetryable4xx(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// mark runs a store write that records an attempt's OUTCOME on a context owned by the write, not
// the attempt. A timed-out POST reaches the mark with a dead context, so attempt_count would never
// advance and the row would be re-POSTed every lease interval; and the attempt's cancellation is
// the shutdown signal, so a delivered event must not be resent because the process was stopping.
// markWriteTimeout bounds the write so a hung store cannot stall the batch past the claim lease.
func (w *Worker) mark(ctx context.Context, write func(context.Context) error) error {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markWriteTimeout)
	defer cancel()
	return write(markCtx)
}

// markTerminal records a no-retry outcome for a delivery, clearing its schedule so it drops out of
// the pending list.
//
// A store write error must NOT be discarded: the row still carries its lease, so the same request
// would be re-POSTed every lease interval with no signal. It goes on the mark_error counter, and
// the terminal `outcome` counter advances only when the mark persisted, so one call never counts both.
func (w *Worker) markTerminal(ctx context.Context, d platform.WebhookDelivery, msg, outcome string) {
	if err := w.mark(ctx, func(markCtx context.Context) error {
		return w.store.MarkAttemptFailed(markCtx, d.ID, msg, 0, time.Time{})
	}); err != nil {
		w.opts.Logger.Warn("customer-webhook: terminal MarkAttemptFailed failed; delivery keeps its claim lease and will re-POST until the write succeeds",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID, "outcome", outcome)
		obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues("mark_error").Inc()
		return
	}
	obs.CustomerWebhookDeliveryAttemptsTotal.WithLabelValues(outcome).Inc()
}

// handleFailure routes a non-2xx response into MarkAttemptFailed: terminal failures clear
// next_attempt_at; transient ones schedule the next try with capped exponential backoff.
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
	if err := w.mark(ctx, func(markCtx context.Context) error {
		return w.store.MarkAttemptFailed(markCtx, d.ID, msg, status, nextAttempt)
	}); err != nil {
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

// scheduleRetry returns the next attempt time for a 1-based attempt number: exponential backoff
// (30s, 1m, 2m, ... capped at 1h) with equal-jitter so deliveries that fail in the same tick
// don't retry in synchronized waves.
func (w *Worker) scheduleRetry(nextAttempt int) time.Time {
	return w.opts.Clock().Add(w.backoffDelay(nextAttempt))
}

// backoffDelay is backoffCeiling jittered into [ceiling/2, ceiling].
func (w *Worker) backoffDelay(nextAttempt int) time.Duration {
	return jitterDelay(backoffCeiling(nextAttempt))
}

// backoffCeiling is the un-jittered backoff delay for a 1-based attempt number. The shift
// exponent is clamped BEFORE the shift so the delay can never wrap int64 whatever MaxAttempts is.
func backoffCeiling(nextAttempt int) time.Duration {
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
	return delay
}

// jitterDelay returns a random duration in [delay/2, delay], so simultaneous failures don't
// re-hammer a recovering endpoint in lockstep. It never returns zero for a positive input.
func jitterDelay(delay time.Duration) time.Duration {
	half := delay / 2
	if half <= 0 {
		return delay
	}
	// rand.Int64N(half+1) ∈ [0, half]; result ∈ [half, delay].
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// setSignatureHeaders signs d with the current key and, during a rotation overlap, also with the
// previous key in the -Previous headers, so a receiver still holding the old key keeps verifying.
func (w *Worker) setSignatureHeaders(h http.Header, wh platform.CustomerWebhook, d platform.WebhookDelivery) {
	now := w.opts.Clock()
	sigTS := now.Unix()
	h.Set("X-StellarIndex-Timestamp", strconv.FormatInt(sigTS, 10))
	h.Set("X-StellarIndex-Signature", "sha256="+w.signFn(wh.SigningKey, sigTS, d.Payload))
	h.Set("X-StellarIndex-Signature-V2", "sha256="+signDeliveryHMACSHA256(wh.SigningKey, sigTS, d.ID.String(), d.EventType, d.Payload))
	if prev := wh.ActivePreviousSigningKey(now); prev != nil {
		h.Set("X-StellarIndex-Signature-Previous", "sha256="+w.signFn(prev, sigTS, d.Payload))
		h.Set("X-StellarIndex-Signature-V2-Previous", "sha256="+signDeliveryHMACSHA256(prev, sigTS, d.ID.String(), d.EventType, d.Payload))
	}
}

// signHMACSHA256 returns the hex HMAC-SHA-256 over `"<unix_ts>." + body`; signing the body alone
// would make a captured delivery replayable forever. Consumers recompute it, compare in constant
// time, AND reject a timestamp outside a tolerance window (~5 min) to bound replay.
func signHMACSHA256(secret []byte, ts int64, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// signDeliveryHMACSHA256 is the X-StellarIndex-Signature-V2 MAC over
// `"<unix_ts>.<delivery_id>.<event_type>." + body`. V1 leaves the Delivery-Id and Event headers
// outside the MAC, so a captured delivery could be re-sent under any id (defeating receiver
// dedupe) or event type. The encoding is unambiguous: the fields before the body are dot-free.
func signDeliveryHMACSHA256(secret []byte, ts int64, deliveryID, eventType string, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write([]byte(deliveryID))
	mac.Write([]byte{'.'})
	mac.Write([]byte(eventType))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrAlreadyRunning is returned when Worker.Run is called more
// than once on the same Worker instance.
var ErrAlreadyRunning = errors.New("customerwebhook: worker already running")
