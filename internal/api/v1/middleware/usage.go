package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// postResponseWriteTimeout bounds the bookkeeping writes that run AFTER
// the response has been flushed (usage counters, last-used touch), on
// the shared [AfterResponse] worker pool rather than the request
// goroutine (GH-627). They run under a fresh context.Background(), never
// the request's — a wedged store must not pin a pool worker forever, and
// the tasks must survive a client abort or an exhausted RequestTimeout
// budget exactly as they did before. Generous relative to go-redis's 3 s
// default so it is a backstop, not the usual limiter.
const postResponseWriteTimeout = 5 * time.Second

// streamMeterInterval is how often [meterOpenStream] re-bills a
// still-open SSE connection. Without this, a stream is metered once
// at open (GH-1279): it buys unbounded duration for one quota unit,
// stays invisible in the DETAIL family for the rest of its lifetime,
// and never gains a second unit no matter how long it runs. Chosen
// to match the rate-limit bucket's per-minute granularity.
//
// atomic.Int64 (nanoseconds), not a plain time.Duration: a
// per-connection ticker goroutine reads this on every tick from
// outside the request goroutine, and [SetStreamMeterIntervalForTest]
// can rewrite it from a still-running test while a previous test's
// ticker goroutine has not yet observed its stream close.
var streamMeterInterval atomic.Int64

func init() {
	streamMeterInterval.Store(int64(time.Minute))
}

// UsageTracker records per-request daily counters keyed on the
// authenticated subject's OWNER ACCOUNT (auth.Subject Identifier, with
// KeyID as the fallback for credentials carrying no owner reference —
// see [UsageKeyForSubject]). Anonymous requests are skipped —
// /v1/account/usage is per-account, and there's no account to bill
// for IP-only callers.
//
// Two counter families per request, each advanced by the request's
// unit count (1 unless the handler raised it via [ChargeUsage]):
//
//   - The LEGACY per-day total (usage:<sub>:<day>) — feeds
//     [MonthlyQuota] and the /v1/account/usage fallback path. Only
//     BILLABLE traffic increments it: 429s are excluded so
//     rate-limit rejections don't eat monthly quota, and 5xx are
//     excluded for the same reason with more force (COR-05) — a
//     platform-caused failure is our fault, not the customer's, so
//     an outage must not burn through the quota they paid for and
//     lock them out of their own plan once we recover. Both classes
//     stay fully visible in the detail family below, so the traffic
//     is still observable; it is only the BILLABLE total that
//     excludes them.
//   - The per-endpoint DETAIL hash (usage:ep:<sub>:<day>) — one
//     field per (route pattern, outcome class): ok / 4xx / 429 /
//     5xx. The rollup worker folds these into the `usage_daily`
//     hypertable that backs the dashboard's per-endpoint analytics.
//
// The endpoint label is the mux route PATTERN (bounded cardinality),
// read via [obs.RouteFromContext] — the innermost obs.CaptureRoute
// middleware writes it after dispatch — with r.Pattern as fallback
// for stacks without the obs pair (tests). Unmatched paths bucket
// under "unmatched".
//
// An SSE request ([isStreamingPath]) is recorded when its headers
// commit (see [streamOpenMeter]), then re-billed every
// [streamMeterInterval] for as long as it stays open (see
// [meterOpenStream]) — a stream that never returns must still cost
// more than one unit the longer it runs. Every other request is
// recorded once, after its handler returns.
//
// Failures are logged at debug and dropped — usage tracking must
// never block a request.
//
// Wire this AFTER the auth middleware (so SubjectFrom returns) and
// OUTSIDE rate-limit, so 429 rejections are observed and counted as
// `throttled` in the detail family.
func UsageTracker(counter *usage.Counter, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if counter == nil {
				next.ServeHTTP(w, r)
				return
			}
			deadlineFired := new(atomic.Bool)
			reqCtx := r.Context()
			dispatchCtx := context.WithValue(reqCtx, readDeadlineKey{}, deadlineFired)
			r = r.WithContext(dispatchCtx)
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			units := &usageUnits{n: 1}
			// endpointFamily reads the dispatched copy: its r.Pattern
			// fallback is set on the request the mux actually received.
			inner := r.WithContext(context.WithValue(dispatchCtx, usageUnitsKey{}, units))
			once := &usageRecordOnce{record: func(panicked bool) {
				usageTrackerRecord(counter, logger, reqCtx, inner, rec, units, deadlineFired, panicked)
			}}
			var out http.ResponseWriter = rec
			if isStreamingPath(r.URL.EscapedPath()) {
				out = &streamOpenMeter{statusRecorder: rec, once: once}
				// GH-1279: the open-time record above bills interval zero;
				// without this, a stream held open longer than that buys
				// unbounded duration for the same one unit. family is read
				// BEFORE dispatch (pre-dispatch ResolveRoute fallback) so the
				// ticker goroutine never races the mux's write to inner.Pattern.
				if subject, ok := auth.SubjectFrom(reqCtx); ok {
					if id := UsageKeyForSubject(subject); id != "" {
						family := endpointFamily(inner)
						done := make(chan struct{})
						defer close(done)
						go meterOpenStream(counter, logger, id, family, rec, once, units, deadlineFired, done)
					}
				}
			}
			// Deferred so a panicking handler still gets counted: Recoverer
			// sits OUTSIDE this middleware, so a panic unwinds through here
			// on its way up, and straight-line bookkeeping after
			// next.ServeHTTP would never run (GH-1276). recover()+re-panic
			// so the outer Recoverer still sees and logs it; a panic never
			// wrote rec.status, so it's classed as 5xx explicitly rather
			// than read off the statusRecorder's 200 zero-value.
			defer func() {
				p := recover()
				once.fire(p != nil)
				if p != nil {
					panic(p)
				}
			}()
			next.ServeHTTP(out, inner)
		})
	}
}

// usageTrackerRecord is [UsageTracker]'s post-dispatch bookkeeping,
// pulled into its own deferred call so it still runs when the handler
// panics (GH-1276). On the non-panic path the counter writes go through
// [AfterResponse], which flushes rec for the client immediately and runs
// them on the shared post-response pool (GH-627) instead of the request
// goroutine. On a panic, rec is NOT known-complete (the outer Recoverer
// still has to write its 500), so the writes are enqueued directly,
// without flushing.
func usageTrackerRecord(counter *usage.Counter, logger *slog.Logger, reqCtx context.Context, inner *http.Request, rec *statusRecorder, units *usageUnits, deadlineFired *atomic.Bool, panicked bool) {
	subject, ok := auth.SubjectFrom(reqCtx)
	if !ok {
		return
	}
	id := UsageKeyForSubject(subject)
	if id == "" {
		return
	}
	family := endpointFamily(inner)
	// A panic never wrote rec.status, so reading it off the
	// statusRecorder's 200 zero-value would misclass an internal
	// error as billable "ok" traffic.
	class := outcomeClass(rec.status)
	if panicked {
		class = usage.ClassServerError
	}
	billable := billableClass(class, deadlineFired.Load())
	n := units.get()
	// The response is already written, so these counters MUST NOT
	// inherit the request's cancellation: a client that aborted, or
	// a handler that consumed the whole RequestTimeout budget, would
	// otherwise silently lose its usage row — and the legacy total is
	// the monthly-quota input. They run under context.Background(), on
	// the shared after-response pool rather than the request goroutine
	// (GH-627), so a wedged store bounds only a pool worker, never the
	// request (C3-102, audit-2026-07-23).
	//
	// DELIBERATE BEHAVIOUR CHANGE, recorded rather than discovered:
	// an ABORTED request now consumes monthly quota where before it
	// silently did not. statusRecorder defaults to 200, so a served-
	// then-abandoned request classes as billable and the Increment
	// (which used to die on the cancelled context) now lands. That
	// is the intended semantics — the request consumed the read, the
	// pool connection and the CPU, and NOT counting it is a
	// quota-evasion vector: a caller could abort before the body
	// completes and get unmetered traffic indefinitely.
	//
	// The 5 s bound is only as hard as the store's context honouring.
	// go-redis and database/sql both respect ctx cancellation on the
	// wire, so it holds for every store wired here today; a driver
	// that ignored ctx would block a pool worker for its own timeout
	// instead.
	fn := func() { //nolint:contextcheck // intentional detach: post-response work outlives the request ctx
		ctx, cancel := context.WithTimeout(context.Background(), postResponseWriteTimeout)
		defer cancel()
		if billable {
			// Legacy total: billable traffic only (quota input).
			// The counter is the alertable signal; a per-request log
			// line would flood during an outage.
			if err := counter.IncrementBy(ctx, id, n); err != nil {
				obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterBillable).Add(float64(n))
				logger.Debug("usage: increment failed", "err", err, "subject", id)
			}
		}
		if err := counter.IncrementDetailBy(ctx, id, family, class, n); err != nil {
			obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterDetail).Add(float64(n))
			logger.Debug("usage: detail increment failed",
				"err", err, "subject", id, "endpoint", family, "class", class)
		}
	}
	if panicked {
		submitAfterResponseTask(fn)
		return
	}
	AfterResponse(rec, fn)
}

// usageRecordOnce runs a request's usage bookkeeping at most once, from
// whichever fires first: a stream's header commit or handler return.
type usageRecordOnce struct {
	done   atomic.Bool
	record func(panicked bool)
}

func (o *usageRecordOnce) fire(panicked bool) {
	if o.done.CompareAndSwap(false, true) {
		o.record(panicked)
	}
}

// fired reports whether the once-record has already run — used by
// [meterOpenStream] to gate periodic ticks on the stream having
// actually committed a response.
func (o *usageRecordOnce) fired() bool {
	return o.done.Load()
}

// streamOpenMeter records an SSE request's usage when the handler commits
// its response headers, not when the stream closes: a stream can outlive
// the day it opened on, and one ended by a process kill or deploy never
// returns through the deferred close-time record at all. The status is
// known at commit, so a stream refused with 429/5xx still stays unbilled.
type streamOpenMeter struct {
	*statusRecorder
	once *usageRecordOnce
}

func (m *streamOpenMeter) WriteHeader(code int) {
	m.statusRecorder.WriteHeader(code)
	m.once.fire(false)
}

func (m *streamOpenMeter) Write(b []byte) (int, error) {
	n, err := m.statusRecorder.Write(b)
	m.once.fire(false)
	return n, err
}

// Flush counts too: flushing an unwritten response commits an implicit 200.
func (m *streamOpenMeter) Flush() {
	m.statusRecorder.Flush()
	m.once.fire(false)
}

// usageUnits is one request's metered cost in request units. The
// tracker opens it at one unit before dispatch; a handler whose product
// output scales with a client-chosen parameter raises it through
// [ChargeUsage]. Mutex-guarded for the same reason as rateLimitCharge.
type usageUnits struct {
	mu sync.Mutex
	n  int64
}

type usageUnitsKey struct{}

func (u *usageUnits) get() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// ChargeUsage prices the in-flight request at units request units IN
// TOTAL for the monthly meter — both the billable total [MonthlyQuota]
// enforces and the per-endpoint detail counters. It can raise a
// request's price, never lower it. A request that never crossed
// [UsageTracker] has no meter and is unaffected.
//
// This is the monthly-quota counterpart of [ChargeRateLimit], and is
// deliberately separate from it: rate-limit tokens are a capacity
// weight (a /v1/assets volume sort costs ten), while request units are
// the PRODUCT unit a plan sells — one per price returned, so a
// 1000-id POST /v1/price/batch costs what 1000 GET /v1/price calls do.
//
// The quota gate runs before dispatch and cannot know the cost, so a
// caller one unit under its cap can finish one batch over it; the
// overshoot is bounded by that request's size and the next request is
// refused.
func ChargeUsage(r *http.Request, units int) {
	u, ok := r.Context().Value(usageUnitsKey{}).(*usageUnits)
	if !ok || u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := int64(units); n > u.n {
		u.n = n
	}
}

// meterOpenStream re-bills a live SSE connection every
// [streamMeterInterval] for as long as it stays open, closing the
// GH-1279 gap where a stream was (and, at open, still is) metered
// exactly once: unbounded duration for one quota unit, invisible to
// the DETAIL family for its whole lifetime.
//
// Ticks are gated on once.fired(): [streamOpenMeter] fires it the
// moment the handler commits headers/first write/flush, and an SSE
// response can't change status after that (HTTP forbids it), so a
// tick observing fired()==true is bound (via the atomic.Bool it
// reads) to see rec.status/units as they stood at that commit —
// billing a connection already confirmed successful without racing
// the handler goroutine that still owns rec. A handler that errors
// out before opening the stream never gets ticked at all.
//
// family and id are resolved once, before dispatch, and passed in
// rather than recomputed per tick: after dispatch starts, the mux
// mutates inner.Pattern from the handler's goroutine, so reading it
// from here too would race it.
//
// Deliberately reuses [outcomeClass]/[billableClass]/[units] rather
// than assuming success, so COR-05 (a platform-side 5xx must never
// eat monthly quota) holds for every tick, not just the first.
//
// Writes run inline on this goroutine, not via [AfterResponse]'s
// shared pool: this goroutine is already dedicated to one connection
// (unlike a burst of post-response bookkeeping across many short
// requests), so there is no fan-out to bound, and routing through the
// pool's WaitGroup here made a test polling mid-stream race the
// pool's own drain-for-test Wait() (sync.WaitGroup forbids a
// concurrent Add once a Wait could observe zero).
//
// Stops when done is closed (the request handler returned).
func meterOpenStream(counter *usage.Counter, logger *slog.Logger, id, family string, rec *statusRecorder, once *usageRecordOnce, units *usageUnits, deadlineFired *atomic.Bool, done <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(streamMeterInterval.Load()))
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !once.fired() {
				continue
			}
			class := outcomeClass(rec.status)
			billable := billableClass(class, deadlineFired.Load())
			n := units.get()
			ctx, cancel := context.WithTimeout(context.Background(), postResponseWriteTimeout)
			if billable {
				if err := counter.IncrementBy(ctx, id, n); err != nil {
					obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterBillable).Add(float64(n))
					logger.Debug("usage: stream tick increment failed", "err", err, "subject", id)
				}
			}
			if err := counter.IncrementDetailBy(ctx, id, family, class, n); err != nil {
				obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterDetail).Add(float64(n))
				logger.Debug("usage: stream tick detail increment failed",
					"err", err, "subject", id, "endpoint", family, "class", class)
			}
			cancel()
		}
	}
}

// SetStreamMeterIntervalForTest overrides [streamMeterInterval] for the
// life of a test, returning a restore func. Test-only: production never
// changes the interval at runtime.
func SetStreamMeterIntervalForTest(d time.Duration) (restore func()) {
	orig := streamMeterInterval.Load()
	streamMeterInterval.Store(int64(d))
	return func() { streamMeterInterval.Store(orig) }
}

// UsageKeyForSubject picks the stable identifier we count under.
// Order of preference:
//  1. Identifier — the OWNER-ACCOUNT reference ([auth.AccountIdentifier],
//     `acct:<slug>`, for API keys; the client account id for SEP-10).
//     Every credential the same account holds shares ONE counter.
//  2. KeyID — per-credential fallback, reached only by a credential
//     whose store stamped no owner reference (legacy hand-seeded Redis
//     records). Metered narrowly beats not metered at all.
//  3. "" — anonymous; skip.
//
// # Why the ACCOUNT, not the credential (RLT-404)
//
// The counter this derives feeds [MonthlyQuota] and keys the per-minute
// rate-limit bucket ([authenticatedRateLimitKey]), and the ceiling each
// is compared against is a PLAN budget, not a per-credential allowance:
// platform.Tier.MaxMonthlyQuota is the ladder a dashboard-minted
// key's quota is clamped to, and the account-level
// `MonthlyRequestQuotaOverride` is the hard ceiling above it — the
// documented contract is that a customer may only ever LOWER their cap,
// never raise it. Keying on KeyID handed them two ways to raise it
// anyway, because the counter's identity was the credential's:
//
//   - MINT: N live keys under one account meant N independent
//     month-to-date counters, so the plan's allowance multiplied by the
//     number of keys held (bounded by account.go's
//     defaultAccountKeyQuota, but bounded is not capped).
//   - ROTATE: revoke-and-mint produced a fresh KeyID, hence a
//     month-to-date of zero, so the cap reset on demand mid-month —
//     and the self-service key cap counts only un-revoked keys, so the
//     rotation was free.
//
// Counting per account closes both: the allowance is conserved across
// every credential the account holds and survives rotation, which is
// what a plan budget means.
//
// The per-credential `Subject.MonthlyQuota` still decides the ceiling
// for the request in front of us, so a customer who deliberately lowers
// ONE key's quota now gets that key cut off at that many ACCOUNT-wide
// requests. That is stricter than the pre-fix reading and deliberately
// so: it fails closed, and in the ordinary case every key on an account
// carries the same clamped plan value, where the two readings coincide.
//
// Exported (HLT-01) so every reader of these counters — [MonthlyQuota]
// and /v1/account/usage's handler — calls this single implementation
// instead of reimplementing the derivation. A duplicated copy that
// drifts from this one silently breaks the reader: it would key off
// something the writer never wrote under, and /v1/account/usage would
// return [] despite incoming requests being recorded. That coupling is
// why the derivation can only move with its writer: a reader pointed at
// an account key the writer never writes meters NOTHING.
func UsageKeyForSubject(s auth.Subject) string {
	if s.Tier == auth.TierAnonymous || s.Tier == "" {
		return ""
	}
	if s.Identifier != "" {
		return "id:" + s.Identifier
	}
	if s.KeyID != "" {
		return "key:" + s.KeyID
	}
	return ""
}

// resolvedRouteKey is the context key [ResolveRoute] stashes its
// pre-dispatch pattern match under.
type resolvedRouteKey struct{}

// ResolveRoute pre-computes the mux-matched route pattern via a
// read-only mux.Handler lookup — no dispatch, so it never runs a
// handler — and stashes it in the request context. Wire this OUTSIDE
// every pre-dispatch gate (auth, key policy, monthly quota, rate
// limit, usage tracker): a request one of those gates rejects never
// reaches the mux, so [obs.CaptureRoute] (wired innermost, directly
// above the mux) never runs and obs.RouteFromContext/r.Pattern stay
// empty for the rest of the stack, including [endpointFamily]'s
// post-response read. Without this, every gate-rejected request
// bucketed under the bounded-cardinality "unmatched" family instead
// of its real route (Q177), hiding exactly the per-endpoint throttle
// pattern the DETAIL family exists to surface.
//
// [obs.CaptureRoute] remains the authoritative source once the mux
// actually dispatches — [endpointFamily] still prefers it — so this
// only fills the gap for requests a gate stops short of the mux.
func ResolveRoute(mux muxMatcher) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, pattern := mux.Handler(r); pattern != "" {
				r = r.WithContext(context.WithValue(r.Context(), resolvedRouteKey{}, pattern))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// stripMethodPrefix strips the leading "METHOD " a Go 1.22+ mux
// pattern carries (e.g. "GET /v1/assets/{id}") down to the path.
func stripMethodPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// endpointFamily resolves the bounded-cardinality endpoint label for
// the request: the mux route pattern path, never the raw URL.
func endpointFamily(r *http.Request) string {
	if route := obs.RouteFromContext(r.Context()); route != "" {
		return route
	}
	// Fallback: the mux mutates r.Pattern in place, so when no
	// inner middleware re-wrapped the request (plain test stacks)
	// the pattern is visible here directly.
	if p := r.Pattern; p != "" {
		return stripMethodPrefix(p)
	}
	// A pre-dispatch gate (RateLimit, MonthlyQuota, ...) rejected the
	// request before the mux ever ran — [ResolveRoute]'s early match
	// is the only source left. See that doc comment for why this
	// exists at all.
	if p, ok := r.Context().Value(resolvedRouteKey{}).(string); ok && p != "" {
		return stripMethodPrefix(p)
	}
	return "unmatched"
}

// billableClass reports whether an outcome class increments the
// LEGACY per-day total — the counter [MonthlyQuota] enforces against.
//
// Only outcomes the CALLER caused are billable. A 429 is our
// throttle firing and a fast-failing 5xx is our failure; charging
// either against the customer's monthly quota means a rate-limit storm
// or an outage on our side eats the plan they paid for (COR-05).
//
// The exception is a 5xx on which a server-side read deadline fired
// ([MarkReadDeadline]): that read held a pool connection for its whole
// budget, usually because of the request's own size, and leaving it
// unbilled made the most expensive request shape free. Every class
// stays counted in the per-endpoint DETAIL family regardless.
func billableClass(class string, readDeadlineFired bool) bool {
	switch class {
	case usage.ClassThrottled:
		return false
	case usage.ClassServerError:
		return readDeadlineFired
	default:
		return true
	}
}

type readDeadlineKey struct{}

// MarkReadDeadline records that a server-side read deadline fired while
// serving the request ctx belongs to, making a 5xx answer billable (see
// [billableClass]). A no-op outside [UsageTracker]; safe for concurrent use.
func MarkReadDeadline(ctx context.Context) {
	if fired, ok := ctx.Value(readDeadlineKey{}).(*atomic.Bool); ok {
		fired.Store(true)
	}
}

// outcomeClass maps a response status onto the four bounded usage
// classes. <400 (incl. 3xx/304) counts as ok; 429 is its own class
// so throttling is visible separately from real client errors.
func outcomeClass(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return usage.ClassThrottled
	case status >= 500:
		return usage.ClassServerError
	case status >= 400:
		return usage.ClassClientError
	default:
		return usage.ClassOK
	}
}
