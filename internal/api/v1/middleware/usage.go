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
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// postResponseWriteTimeout bounds the bookkeeping writes that run on the [AfterResponse] pool.
// They use a fresh context.Background(), never the request's: they must survive a client abort
// or an exhausted RequestTimeout, yet a wedged store must not pin a pool worker forever.
const postResponseWriteTimeout = 5 * time.Second

// streamMeterInterval is how often [meterOpenStream] re-bills a still-open SSE connection, so a
// long stream does not buy unbounded duration for one quota unit (per-minute, like the rate-limit
// bucket). It is an atomic.Int64 (nanoseconds) because ticker goroutines read it while
// [SetStreamMeterIntervalForTest] may rewrite it from a still-running test.
var streamMeterInterval atomic.Int64

func init() {
	streamMeterInterval.Store(int64(time.Minute))
}

// UsageTracker records per-request daily counters keyed on the authenticated subject's OWNER
// ACCOUNT (see [UsageKeyForSubject]); anonymous requests have no account to bill and are skipped.
//
// Two counter families per request, each advanced by its unit count (1 unless the handler raised
// it via [ChargeUsage]):
//   - The LEGACY per-day total (usage:<sub>:<day>) feeds [MonthlyQuota] and the
//     /v1/account/usage fallback. Only BILLABLE traffic increments it: 429s and 5xx are
//     excluded so rate-limit rejections and our own outages never burn paid quota.
//   - The per-endpoint DETAIL hash (usage:ep:<sub>:<day>) has one field per (mux route
//     PATTERN, so bounded cardinality; outcome class), folded by the rollup worker into
//     `usage_daily`.
//
// An SSE request ([isStreamingPath]) is recorded at header commit ([streamOpenMeter]) and
// re-billed every [streamMeterInterval] ([meterOpenStream]); others once, after the handler
// returns. Failures are logged and dropped. Wire AFTER auth and OUTSIDE rate-limit.
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
			// Deferred so a panicking handler still gets counted: Recoverer sits OUTSIDE this
			// middleware, so straight-line code after next.ServeHTTP would never run. It re-panics for
			// that Recoverer and classes the request 5xx explicitly (rec.status was never written).
			//
			// Registered BEFORE the streaming block so it runs AFTER that block's close(done)+<-exited
			// (defers are LIFO). meterOpenStream must have fully returned, not merely been signalled,
			// or it could see fired()==true and bill rec's default 200 as OK, double-billing a request
			// this defer classes as 5xx.
			defer func() {
				p := recover()
				once.fire(p != nil)
				if p != nil {
					panic(p)
				}
			}()
			var out http.ResponseWriter = rec
			if isStreamingPath(r.URL.EscapedPath()) {
				out = &streamOpenMeter{statusRecorder: rec, once: once}
				// The open-time record bills interval zero; this ticker bills the rest. family is
				// read BEFORE dispatch so the ticker never races the mux's write to inner.Pattern.
				if subject, ok := auth.SubjectFrom(reqCtx); ok {
					if id := UsageKeyForSubject(subject); id != "" {
						family := endpointFamily(inner)
						// Cancelled by a tick that finds the monthly quota spent, so the
						// handler ends the stream exactly as a client disconnect would.
						streamCtx, endStream := context.WithCancel(inner.Context())
						defer endStream()
						inner = inner.WithContext(streamCtx) //nolint:contextcheck // false positive: streamCtx is derived from inner's own request context
						done := make(chan struct{})
						exited := make(chan struct{})
						// Wait for meterOpenStream to actually return, not just observe
						// close(done): bounded by postResponseWriteTimeout, since the
						// only way this blocks is a tick already in flight finishing
						// its counter writes under that same timeout.
						defer func() { close(done); <-exited }()
						//nolint:gosec,contextcheck // G118/contextcheck: intentional detach, same rationale (and pattern) as usageTrackerRecord's context.Background() below — ticks must outlive a client-aborted request ctx, not inherit it
						go meterOpenStream(counter, logger, id, family, subject.MonthlyQuota, endStream, rec, once, units, deadlineFired, done, exited)
					}
				}
			}
			next.ServeHTTP(out, inner)
		})
	}
}

// usageTrackerRecord is [UsageTracker]'s post-dispatch bookkeeping, split out so it still runs
// when the handler panics. Normally the writes go through [AfterResponse], which flushes rec
// first. On a panic rec is not complete (the outer Recoverer still writes its 500), so the
// writes are enqueued directly without flushing.
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
	// These counters MUST NOT inherit the request's cancellation: an aborted client or exhausted
	// RequestTimeout would lose the usage row, the monthly-quota input. A wedged store therefore
	// bounds only a pool worker, and only as hard as the store honours ctx (5 s).
	//
	// DELIBERATE: an ABORTED request consumes monthly quota (statusRecorder defaults to 200, so it
	// is billable). It still cost the read and CPU, and not counting it would be a quota-evasion
	// vector: abort before the body completes, go unmetered.
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

// streamOpenMeter records an SSE request's usage when the handler commits its headers, not when
// the stream closes: a stream can outlive its day, and one ended by a kill or deploy never
// reaches the close-time record. The status is known at commit, so a 429/5xx stream stays unbilled.
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

// ChargeUsage prices the in-flight request at units request units IN TOTAL for the monthly meter
// ([MonthlyQuota] and the detail counters). It can raise a price, never lower it.
//
// Separate from [ChargeRateLimit]: rate-limit tokens are a capacity weight, while request units
// are the PRODUCT unit a plan sells, so a 1000-id POST /v1/price/batch costs what 1000 GET
// /v1/price calls do.
//
// The quota gate runs before dispatch and cannot know the cost, so a caller one unit under its
// cap can finish one batch over it; the overshoot is bounded by that request and the next is
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

// meterOpenStream re-bills a live SSE connection every [streamMeterInterval] while it stays
// open.
//
// Ticks before [streamOpenMeter]'s header commit are skipped, not deferred. They are gated on
// once.fired(): an SSE response cannot change status after the first write/flush, so a tick
// seeing fired()==true reads rec.status/units without racing the handler goroutine that owns rec.
// family and id are resolved before dispatch because the mux later mutates inner.Pattern.
//
// It reuses [outcomeClass]/[billableClass], so a platform 5xx never eats quota, and a billable
// tick is refused at the same used >= quota ceiling [MonthlyQuota] applies at open (endStream,
// then stop). Writes run inline since this goroutine is dedicated to one connection. Closing
// exited on the way out (even on panic) lets the caller wait until rec/once/units are untouched.
func meterOpenStream(counter *usage.Counter, logger *slog.Logger, id, family string, quota int64, endStream context.CancelFunc, rec *statusRecorder, once *usageRecordOnce, units *usageUnits, deadlineFired *atomic.Bool, done <-chan struct{}, exited chan<- struct{}) {
	defer worker.Recover(logger, "api-usage-stream-meter")
	defer close(exited)
	ticker := time.NewTicker(time.Duration(streamMeterInterval.Load()))
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !meterTick(counter, logger, id, family, quota, endStream, rec, once, units, deadlineFired, done) {
				return
			}
		}
	}
}

// meterTick runs one [meterOpenStream] tick's re-check-and-bill decision,
// split out so the caller's loop stays simple. Returns false once the
// stream has closed or been ended for quota, telling meterOpenStream to stop.
func meterTick(counter *usage.Counter, logger *slog.Logger, id, family string, quota int64, endStream context.CancelFunc, rec *statusRecorder, once *usageRecordOnce, units *usageUnits, deadlineFired *atomic.Bool, done <-chan struct{}) bool {
	// Re-check done, non-blocking: a tick already buffered in ticker.C when done closed must not
	// read rec after the request has finished with it.
	select {
	case <-done:
		return false
	default:
	}
	if hook := streamTickHook.Load(); hook != nil {
		(*hook)()
	}
	if !once.fired() {
		return true
	}
	class := outcomeClass(rec.status)
	billable := billableClass(class, deadlineFired.Load())
	n := units.get()
	ctx, cancel := context.WithTimeout(context.Background(), postResponseWriteTimeout)
	defer cancel()
	if billable {
		if streamQuotaExhausted(ctx, counter, logger, id, quota) {
			endStream()
			return false
		}
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
	return true
}

// streamQuotaExhausted reports whether id has reached a positive quota.
// A read error fails open, as [MonthlyQuota] does inside its dwell window.
func streamQuotaExhausted(ctx context.Context, counter *usage.Counter, logger *slog.Logger, id string, quota int64) bool {
	if quota <= 0 {
		return false
	}
	used, err := counter.MonthToDate(ctx, id)
	if err != nil {
		obs.MonthlyQuotaFailOpenTotal.Inc()
		logger.Warn("usage: stream tick quota read failed; failing open", "err", err, "subject", id)
		return false
	}
	return used >= quota
}

// SetStreamMeterIntervalForTest overrides [streamMeterInterval] for the
// life of a test, returning a restore func. Test-only: production never
// changes the interval at runtime.
func SetStreamMeterIntervalForTest(d time.Duration) (restore func()) {
	orig := streamMeterInterval.Load()
	streamMeterInterval.Store(int64(d))
	return func() { streamMeterInterval.Store(orig) }
}

// streamTickHook, when set, runs on every tick after the done re-check and before once.fired().
// Test-only: it pauses a tick at the once.fire/exited race instead of relying on scheduling.
var streamTickHook atomic.Pointer[func()]

// SetStreamTickHookForTest installs fn as [streamTickHook] for the life
// of a test, returning a restore func. Test-only.
func SetStreamTickHookForTest(fn func()) (restore func()) {
	streamTickHook.Store(&fn)
	return func() { streamTickHook.Store(nil) }
}

// UsageKeyForSubject picks the stable identifier we count under, in order:
//  1. Identifier: the OWNER-ACCOUNT reference ([auth.AccountIdentifier], `acct:<slug>` for API
//     keys; the client account id for SEP-10).
//  2. KeyID: for credentials with no owner reference (legacy Redis records).
//  3. "": anonymous; skip.
//
// Why the ACCOUNT, not the credential: the counter feeds [MonthlyQuota] and keys the per-minute
// bucket ([authenticatedRateLimitKey]); each ceiling is a PLAN budget a customer may only LOWER.
// Keying on KeyID would let them raise it by minting N keys (N counters) or rotating (fresh
// KeyID, zero month-to-date).
//
// The per-credential `Subject.MonthlyQuota` still sets the ceiling, so lowering ONE key's quota
// cuts it off at that many ACCOUNT-wide requests: stricter, deliberately (it fails closed).
//
// Exported so every reader ([MonthlyQuota], /v1/account/usage) shares one implementation: a
// drifted copy would key off something the writer never wrote and meter NOTHING.
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

// ResolveRoute stashes the mux-matched route pattern in the request context via a read-only
// mux.Handler lookup (no dispatch). Wire it OUTSIDE every pre-dispatch gate (auth, key policy,
// monthly quota, rate limit, usage tracker): a request a gate rejects never reaches the mux, so
// [obs.CaptureRoute] never runs and it would bucket under "unmatched", hiding the per-endpoint
// throttle pattern the DETAIL family exists to surface. [obs.CaptureRoute] stays authoritative
// once the mux dispatches; this only fills the gap.
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
	// A pre-dispatch gate rejected the request before the mux ran; [ResolveRoute]'s early
	// match is the only source left.
	if p, ok := r.Context().Value(resolvedRouteKey{}).(string); ok && p != "" {
		return stripMethodPrefix(p)
	}
	return "unmatched"
}

// billableClass reports whether an outcome class increments the LEGACY per-day total that
// [MonthlyQuota] enforces against.
//
// Only outcomes the CALLER caused are billable: a 429 or fast-failing 5xx is our fault, and
// charging it would let a rate-limit storm or outage eat the customer's paid plan.
//
// The exception is a 5xx on which a read deadline fired ([MarkReadDeadline]): that read held a
// pool connection for its whole budget, usually due to the request's own size, and leaving it
// unbilled would make the most expensive request shape free. The DETAIL family counts every class.
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
