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
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
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
//
// It must ALSO implement [AccountStatusReader]; see that type for why
// the requirement is enforced in [New] rather than listed here.
type DeliveryStore interface {
	ListPendingDeliveries(ctx context.Context, limit int) ([]platform.WebhookDelivery, error)
	GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error)
	MarkDelivered(ctx context.Context, id uuid.UUID, responseStatus int) error
	MarkAttemptFailed(ctx context.Context, id uuid.UUID, errMsg string, responseStatus int, nextAttemptAt time.Time) error
}

// AccountStatusReader is the account-level kill switch on the OUTBOUND
// side (SEC-06 / RLT-420). WebhookAccountStatus returns the lifecycle
// status of the account that owns the webhook, or [platform.ErrNotFound]
// when the webhook is gone.
//
// C3-010 wired the kill switch into the auth validator, so a suspended
// account's API keys stop authenticating — but nothing in this delivery
// path ever read account status, so a suspended or closed customer kept
// RECEIVING their data at the endpoints they had registered. The
// suspension was inbound-only. The worker now re-reads the status
// immediately before it signs and POSTs.
//
// It is required, not optional: [New] refuses a store that cannot answer
// it, because a delivery path that silently skips the kill switch when
// the capability is missing is the bug this type exists to remove. The
// requirement is asserted at construction rather than added to
// [DeliveryStore] because the production wiring (cmd/stellarindex-api)
// passes a [platform.WebhookStore] interface value; promoting
// WebhookAccountStatus onto that shared interface — and so getting the
// check at compile time — is a follow-up in internal/platform/webhook.go.
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

	// defaultHTTPTimeout bounds a single delivery POST. It also bounds
	// each attempt's webhook lookup + HTTP work (see tick). The write that
	// records the outcome is bounded separately by markWriteTimeout, so
	// the worst-case serial batch time is
	// defaultBatchLimit × (defaultHTTPTimeout + markWriteTimeout).
	defaultHTTPTimeout = 10 * time.Second

	// storeLeaseDuration is the claim lease ListPendingDeliveries places
	// on each row (`next_attempt_at = now() + 5m`; see the package doc /
	// internal/platform/postgresstore/webhook_store.go). A row not
	// finished within the lease is re-claimed by a second worker and
	// DOUBLE-delivered. One endpoint's rows are delivered serially and a
	// batch may be all one endpoint's, so the worst-case time to reach the
	// last row is still BatchLimit × (Timeout + markWriteTimeout), however
	// many endpoints run in parallel; that product MUST stay comfortably
	// under this lease.
	storeLeaseDuration = 5 * time.Minute

	// markWriteTimeout bounds the store write that records an attempt's
	// outcome. That write runs on its OWN deadline, detached from the
	// attempt's (see Worker.mark), so it is an additional term in the
	// worst-case per-row time and therefore in the lease invariant. The
	// lease leaves 50s of margin over 25 rows — 2s a row — so this must
	// stay under 2s; 1s keeps 25s of margin and is three orders of
	// magnitude above a healthy single-row UPDATE by primary key. A write
	// that cannot land in 1s fails to mark_error and the row is retried
	// on lease expiry with a fresh budget, exactly as a worker crash is.
	markWriteTimeout = 1 * time.Second

	// maxDrainBytes caps how much of a response body we drain to reuse
	// the connection. We never read the body for content, so a hostile
	// or buggy endpoint must not tie the worker up streaming megabytes.
	maxDrainBytes = 64 << 10 // 64 KiB

	// webhookUserAgent identifies deliveries to the customer's endpoint
	// operator so an unexpected POST can be traced back to us (RLT-450).
	// Every other outbound fetch in the repo sets an identifying
	// User-Agent (internal/metadata/sep1.go, internal/stellarrpc); this
	// path was the one exception.
	webhookUserAgent = "stellar-index/webhooks (+https://stellarindex.io)"
)

// Compile-time guard for the two-worker double-delivery invariant
// (MEDIUM, F-1270 hardening): the worst-case serial batch time
// (defaultBatchLimit × (defaultHTTPTimeout + markWriteTimeout)) must stay
// strictly under the store's claim lease. Raising any of the three past
// that point fails the build here rather than silently reintroducing the
// double-delivery race. 25 × (10s + 1s) = 275s < 300s, with 25s of
// margin. The mark term is in the product because the outcome write has
// its own deadline (K025): it no longer shares the attempt's 10s.
const _ = uint(storeLeaseDuration - defaultBatchLimit*(defaultHTTPTimeout+markWriteTimeout) - time.Nanosecond)

// Options tunes the worker. Zero values yield production defaults.
type Options struct {
	// PollInterval between queue drains. Default 5s — tight
	// enough that SEV-1 deliveries feel real-time, loose enough
	// that an idle worker doesn't hammer postgres.
	PollInterval time.Duration

	// BatchLimit caps how many deliveries each poll drains.
	// Default 25 (defaultBatchLimit). The store leases each claimed row
	// for storeLeaseDuration (5m); a row not reached before its lease
	// expires is re-claimed by a second worker and DOUBLE-delivered.
	// INVARIANT: BatchLimit × (HTTPClient.Timeout + markWriteTimeout)
	// MUST stay under the 5-minute store lease (default
	// 25 × (10s + 1s) = 275s < 300s). Concurrency does not relax it:
	// one endpoint's rows are serial and a batch can be all one
	// endpoint's. New panics on a resolved Options that breaks it.
	BatchLimit int

	// Concurrency caps how many ENDPOINTS a batch delivers to in
	// parallel. Each endpoint's rows run serially, in claim order, so one
	// endpoint never has two POSTs in flight from this worker, and a
	// stalled endpoint holds one slot instead of the whole queue.
	// Default 8 (defaultConcurrency).
	Concurrency int

	// MaxAttempts before a delivery is marked permanently failed.
	// Default 15: with backoffCeiling's 30s→doubling→1h-capped schedule
	// the last retry lands ~4–8 h after the first failure (jitter). That
	// window is the customer-facing delivery SLA the runbooks quote
	// (pinned by TestRetryWindowMatchesOperatorDocs); widening it is an
	// SLA decision, not a tuning knob.
	MaxAttempts int

	// HTTPClient supplies the delivery client's Timeout (default
	// 10s). New always sends through the SSRF-guarded dialer and
	// refuses redirects, and panics if a Transport is set.
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
	store    DeliveryStore
	accounts AccountStatusReader
	opts     Options
	stopCh   chan struct{}
	doneCh   chan struct{}
	signFn   func(secret []byte, ts int64, payload []byte) string
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
		// silently restores the inbound-only kill switch this worker was
		// changed to close (SEC-06 / RLT-420).
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
	// RLT-223: the compile-time guard beside the defaults only covers the
	// DEFAULT BatchLimit and HTTPClient.Timeout. A caller-supplied Options
	// can raise either independently — Options.BatchLimit's doc comment
	// says explicitly "if you raise this, keep the product under the
	// lease", which was an unenforced operator obligation, not a runtime
	// check. effectiveTimeout mirrors attemptTimeout()'s own fallback (a
	// zero/absent Timeout still bounds each attempt at defaultHTTPTimeout
	// via the attempt context, so that — not the raw client field — is
	// the true per-attempt worst case). Fail closed at construction
	// rather than let a misconfigured worker double-deliver under load.
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

// guardedClient builds the client every production Worker sends with. The
// dial hook re-resolves the host on every send, so DNS rebinding between
// registration and delivery is caught here; redirects are refused so a 302
// from a public host cannot steer the POST to an internal one. Only the
// caller's Timeout (and Jar) survive: a caller Transport carries its own
// dialer and proxy, so it is refused rather than silently discarded.
func guardedClient(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.Transport != nil {
		panic("customerwebhook: New: Options.HTTPClient.Transport must be nil (a caller transport bypasses the SSRF dial guard)")
	}
	guarded := *c
	guarded.Transport = &http.Transport{DialContext: ssrfGuardedDialContext}
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &guarded
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

// tick drains one batch from the pending queue: one lane per endpoint,
// up to Options.Concurrency lanes at once, each lane serial in claim
// order. Errors are logged + counted per delivery so one bad row doesn't
// stall the queue, and one slow endpoint delays only its own lane. tick
// returns when every lane has finished.
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

// deliverLane delivers one endpoint's rows in order. Each attempt is
// bounded by the per-request timeout, so no lane — and therefore no batch
// — outlives BatchLimit × (Timeout + markWriteTimeout), the basis for the
// BatchLimit-vs-lease invariant (see Options.BatchLimit). This deadline
// covers the webhook lookup and the POST only; the outcome write has its
// own (see Worker.mark), because this one is already spent exactly when
// the POST timed out.
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

// deliverOneRecovered isolates a single delivery's panic to that delivery
// (RLT-450). Without this, tick's top-level `defer
// recoverBackgroundWorker(...)` (cmd/stellarindex-api) only stops the whole
// process from crashing — it still ends the poll loop permanently, so one
// bad row (a future decode edge case, a nil dereference) would silently
// starve every OTHER pending delivery for the rest of the process lifetime.
func (w *Worker) deliverOneRecovered(ctx context.Context, d platform.WebhookDelivery) {
	defer func() {
		if r := recover(); r != nil {
			w.opts.Logger.Error("customer-webhook: deliverOne panicked; delivery left for lease expiry, batch continues",
				"panic", r, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		}
	}()
	w.deliverOne(ctx, d)
}

// attemptTimeout is the per-delivery deadline: the HTTP client's own
// per-request timeout (default defaultHTTPTimeout). Bounding each attempt,
// and separately each outcome write, keeps the worst-case serial batch
// time at BatchLimit × (Timeout + markWriteTimeout).
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
		w.markTerminal(ctx, d, fmt.Sprintf("webhook lookup: %v", err), "webhook_missing")
		return
	}
	if !wh.Enabled {
		// Webhook is disabled — silently terminate the delivery
		// rather than retry forever.
		w.markTerminal(ctx, d, "webhook disabled", "disabled")
		return
	}
	if !w.accountGateOpen(ctx, d) {
		// Account-level kill switch (SEC-06 / RLT-420). The per-webhook
		// `Enabled` flag above is the CUSTOMER's switch; this is the
		// OPERATOR's, and until this check existed only the customer had
		// one. accountGateOpen has already recorded or parked the row.
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
		w.markTerminal(ctx, d, "webhook signing secret is empty", "no_secret")
		return
	}

	sigTS := w.opts.Clock().Unix()
	signature := w.signFn(wh.SecretHash, sigTS, d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(d.Payload))
	if err != nil {
		// URL malformed at request-build time. This is
		// permanently broken — the URL is set per-webhook by the
		// customer; we can't fix it by retrying.
		w.markTerminal(ctx, d, fmt.Sprintf("build request: %v", err), "build_error")
		// Build error: no HTTP roundtrip happened. Record a
		// near-zero duration so the histogram still has a sample
		// at this label and operators see the build_error bucket
		// populate rather than disappear.
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("build_error").Observe(0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webhookUserAgent)
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
		// RSEC-Y1: err can carry internal network detail — in particular
		// the SSRF dial guard's own refusal names the resolved address it
		// blocked. last_error is stored verbatim and served by the
		// dashboard API's deliveryDTO, so the customer gets a fixed,
		// address-free reason; the real error is logged for operators.
		w.opts.Logger.Warn("customer-webhook: delivery POST failed",
			"err", err, "delivery_id", d.ID, "webhook_id", d.WebhookID)
		w.handleFailure(ctx, d, 0, "network error contacting webhook URL", "network_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body (capped) so the connection can be reused. We never
	// read it for content, so io.LimitReader stops a hostile or buggy
	// endpoint from streaming unbounded data at us within the timeout.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	elapsed := time.Since(start).Seconds()

	w.classifyResponse(ctx, d, resp.StatusCode, elapsed)
}

// accountGateOpen reports whether the account that owns this delivery's
// webhook permits it to be sent. False means DO NOT POST — the row has
// already been recorded or parked here, and deliverOne must return.
//
// This is the last line of defence, not the only one: the store's claim
// query already withholds a non-active account's rows
// (ListPendingDeliveries in internal/platform/postgresstore). It still
// has real work to do, because a batch is claimed once and drained
// serially — an account suspended mid-batch has rows already in hand.
//
// The outcomes, and why each:
//
//   - not found: the webhook vanished between the lookup above and here.
//     Terminal, same as deliverOne's not-found branch.
//   - status unreadable: fail CLOSED. An unresolved status is not
//     evidence of an active account. Leave the row for lease expiry —
//     the same recovery a transient GetWebhook failure takes — so a
//     Postgres blip delays deliveries rather than either dropping them
//     or POSTing to a customer we may have just suspended.
//   - closed: terminal. A closed account is not coming back and we
//     should not be holding its events, let alone delivering them.
//   - anything else non-active (suspended, or a value this build does
//     not know): park for lease expiry. Suspension is reversible
//     (AccountStore.Unsuspend), so destroying the backlog would lose
//     events a reinstated customer is entitled to.
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
		// "disabled" is the existing terminal-by-policy outcome label;
		// the label set is a lockstep bounded registry in internal/obs
		// (seedBoundedLabelSeries + metrics_seed_test.go), so splitting
		// out an account_inactive label is a follow-up there. The row's
		// last_error carries the specific reason either way.
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
		// Redirects are DISABLED (CheckRedirect returns
		// ErrUseLastResponse) as part of the SSRF defence, so a 3xx can
		// never succeed on a later attempt — retrying it 15 times over
		// ~8h is pure waste and the recorded reason used to say
		// "transient", which is false and misdirects diagnosis. A
		// trailing-slash 308 from nginx/Rails/Django is the common case
		// (cold audit 2026-08-04).
		obs.CustomerWebhookDeliveryDurationSeconds.WithLabelValues("client_error").Observe(elapsed)
		w.handleFailure(ctx, d, status,
			fmt.Sprintf("HTTP %d (redirect not followed — register the final URL)", status),
			"client_error")
	case isRetryable4xx(status):
		// Not every 4xx is the customer's fault forever. 408, 425 and
		// 429 are explicitly temporary in the HTTP spec, and 429 is what
		// any endpoint behind a rate-limiting gateway returns under a
		// burst — treating it as terminal DESTROYED the event on its
		// first attempt, which is how a SEV-1 notification could be lost
		// to a coincident freeze burst (cold audit 2026-08-04).
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

// isRetryable4xx reports whether a 4xx status describes a TEMPORARY
// condition the customer's endpoint is expected to recover from without
// any change on their side.
//
//   - 408 Request Timeout — the gateway gave up reading; retrying works.
//   - 425 Too Early       — explicitly "retry later" by definition.
//   - 429 Too Many Requests — the canonical back-off signal.
//
// Everything else in 4xx (401/403 credentials, 404 wrong URL, 400/422
// validation) genuinely needs the customer to act, so it stays terminal.
func isRetryable4xx(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// mark runs a store write that records an attempt's OUTCOME, on a
// context whose lifetime belongs to the write rather than to the attempt
// it is recording (K025). Every outcome write goes through here.
//
// The attempt context is the wrong lifetime for it twice over. Its
// deadline starts before GetWebhook and the POST, so it expires no later
// than the HTTP client's own timeout: a POST that times out — the
// commonest failure there is — reached the mark with a context that was
// already dead, MarkAttemptFailed failed on it, attempt_count never
// advanced, and the row kept its claim lease and was re-POSTed every
// lease interval, timing out again each time. The loop never ended on
// its own and MaxAttempts could not end it, because no attempt was ever
// counted. And its cancellation is the worker's shutdown signal: a
// customer who was just sent an event must not be sent it again because
// the process was stopping when the 200 came back.
//
// WithoutCancel drops both the parent's deadline and its cancellation
// while keeping its values; markWriteTimeout then bounds the write so a
// hung store cannot stall the batch past the claim lease (the guard
// beside the defaults holds the sum under it).
func (w *Worker) mark(ctx context.Context, write func(context.Context) error) error {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markWriteTimeout)
	defer cancel()
	return write(markCtx)
}

// markTerminal records a no-retry outcome for a delivery (webhook
// missing/disabled, empty secret, un-buildable request), clearing its
// schedule so it drops out of the pending list.
//
// NTF-WH-01: it must NOT silently discard the store write error. The row
// still carries the claim lease ListPendingDeliveries set (next_attempt_at
// = now()+5m); if the terminal mark's UPDATE fails and the error is
// dropped, attempt_count never advances and the row is re-claimed and the
// same request re-POSTed every lease interval with no signal. Surfacing
// the failure on the mark_error counter — the same counter the retry and
// delivered paths already emit — makes the loop visible to an alert. The
// terminal `outcome` counter is advanced only when the mark persisted, so
// a mark_error and a terminal outcome are never both counted for one call.
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

// scheduleRetry returns the next attempt time given a 1-based attempt
// number. Exponential backoff — 30s, 1m, 2m, 4m, 8m, … capped at 1h —
// with bounded full-jitter so deliveries that fail in the same tick
// don't retry in synchronized waves. Caller decides whether to use this
// or to mark the delivery terminally failed.
func (w *Worker) scheduleRetry(nextAttempt int) time.Time {
	return w.opts.Clock().Add(w.backoffDelay(nextAttempt))
}

// backoffDelay is backoffCeiling jittered into [ceiling/2, ceiling].
func (w *Worker) backoffDelay(nextAttempt int) time.Duration {
	return jitterDelay(backoffCeiling(nextAttempt))
}

// backoffCeiling is the un-jittered exponential-backoff delay for a
// 1-based attempt number. The shift exponent is clamped BEFORE the shift
// so the delay can never wrap int64 regardless of MaxAttempts (the
// previous `base << (n-1)` overflowed to a non-positive value for n ≳ 30).
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
