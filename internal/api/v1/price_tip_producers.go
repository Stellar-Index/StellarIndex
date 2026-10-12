package v1

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Shared tip-stream producers. A compute loop per CONNECTION makes N viewers of one pair cost N×
// the tip computation (6 DB queries/s per connection saturates the pool at ~2300 streams). One
// producer runs per DISTINCT (asset, quote, window) and publishes into the Hub's topic ring; every
// connection is a plain Hub subscriber, so cost scales with distinct pairs, not viewers.

// tipProducerLinger keeps a producer alive briefly after its last
// subscriber leaves, absorbing page reloads/reconnects without a
// stop/start churn of the compute loop.
const tipProducerLinger = 30 * time.Second

// defaultMaxTipProducers caps how many distinct tip producers may run at once.
//
// A tip-stream connection mints a DETACHED producer that outlives it for tipProducerLinger, so the
// SSE connection caps do not bound it: an unauthenticated client could open and abort streams in a
// loop and leave unbounded compute loops polling the database.
//
// The key is (asset, quote, window_seconds) with window_seconds CLIENT-CHOSEN in [1,60], and the
// Hub's topic reaper cannot shed it. 512 caps the worst case short of starving the DB pool; tune
// with [Server.SetMaxTipProducers]. At either global bound the oldest lingering entries are
// evicted, so only subscribed demand can refuse a mint.
const defaultMaxTipProducers = 512

// defaultMaxTipProducersPerCaller caps how many producers ONE caller may have minted and still
// hold registered (running or lingering).
//
// The global ceiling alone is one unpartitioned pool: one address looping the key space and
// aborting each connection would fill every slot for [tipProducerLinger], refusing all OTHER
// callers. So each producer is charged to its minter while its registry entry lives; releasing
// the connection does not return the slot.
//
// 24 sits just above the per-IP concurrent-stream cap, under 5% of the pool. Joining an
// ALREADY-RUNNING producer is never charged. Tune with [Server.SetMaxTipProducersPerCaller].
const defaultMaxTipProducersPerCaller = 24

// defaultMaxTipTicksPerMinute bounds the AGGREGATE compute rate of every registered producer; each
// tick is one computeTip plus its divergence lookup. The count ceilings do not bound the rate:
// window_seconds is client-chosen down to 1s, so 512 producers at 1s drive five times the query
// rate they were sized for at the default 5s window. The budget is that sized-for rate (512 x 12 =
// 6144/min, ~102 computes/s); a shorter window spends proportionally more of it.
const defaultMaxTipTicksPerMinute = defaultMaxTipProducers * (60 / defaultTipWindowSeconds)

// defaultMaxTipTicksPerMinutePerCaller is one caller's share of that rate, derived from its count
// quota (24 × 12 = 288/min): a caller at a 1s window gets four producers, and saturating the
// global budget still takes over 20 addresses.
const defaultMaxTipTicksPerMinutePerCaller = defaultMaxTipProducersPerCaller * (60 / defaultTipWindowSeconds)

// defaultTipTickConcurrency caps how many producer ticks may be inside computeTip at once.
//
// The tick budgets bound the AVERAGE rate, not instantaneous load: hundreds of producers can fire
// together (a restart, a Postgres stall releasing a backlog), each holding a pool connection for
// its whole compute. Detached producers must not take the API's request connections, so ticks
// queue for a slot and a tick that cannot get one within its own budget is skipped. 4 leaves 21 of
// a 25-connection pool to requests while draining the ~102 ticks/s budget.
const defaultTipTickConcurrency = 4

// tipTicksPerMinute is the compute rate of a producer on a window-second
// ticker, rounded UP so a budget is never undercharged.
func tipTicksPerMinute(window int) int {
	window = max(window, minTipWindowSeconds)
	return (60 + window - 1) / window
}

// unattributedTipCaller is the identity for an acquire with no principal to charge (in-process
// callers); the per-caller quota does not apply. The HTTP path never produces it:
// [Server.acquireTipProducer] substitutes [unknownTipCaller] when the client cannot be named.
const unattributedTipCaller = ""

// unknownTipCaller is the shared bucket for a request whose client IP cannot be resolved. One
// bucket is deliberate (fail-closed, mirroring streaming.resolveStreamClientIP): an unresolvable
// caller must not be exempt from the quota.
const unknownTipCaller = "unknown"

// tipProducerOutcome is the verdict of an acquire. The refusals stay distinct because a client
// that has had its share and a server at capacity need telling apart in the log.
type tipProducerOutcome int

const (
	// tipProducerAdmitted: a reference is held; call release exactly once.
	tipProducerAdmitted tipProducerOutcome = iota
	// tipProducerAtCallerQuota: this caller already holds its share of
	// minted producers. No release is handed back.
	tipProducerAtCallerQuota
	// tipProducerAtGlobalCeiling: the registry is full and this key would
	// need a NEW producer. No release is handed back.
	tipProducerAtGlobalCeiling
	// tipProducerAtCallerRateBudget: minting would take the caller past its share of the compute
	// rate (a short window costs more than a long one). No release is handed back.
	tipProducerAtCallerRateBudget
	// tipProducerAtGlobalRateBudget: minting would take the registry's aggregate compute rate
	// past its budget. No release is handed back.
	tipProducerAtGlobalRateBudget
)

// String is the low-cardinality reason label for logs.
func (o tipProducerOutcome) String() string {
	switch o {
	case tipProducerAdmitted:
		return "admitted"
	case tipProducerAtCallerQuota:
		return "caller_quota"
	case tipProducerAtGlobalCeiling:
		return "global_ceiling"
	case tipProducerAtCallerRateBudget:
		return "caller_rate_budget"
	case tipProducerAtGlobalRateBudget:
		return "global_rate_budget"
	default:
		return "unknown"
	}
}

// callerRefusal reports whether the refusal names a caller that has had
// its share, as opposed to a server at capacity.
func (o tipProducerOutcome) callerRefusal() bool {
	return o == tipProducerAtCallerQuota || o == tipProducerAtCallerRateBudget
}

type tipProducerKey struct {
	asset  string
	quote  string
	window int
}

// tipTopic is the Hub topic name for a producer key.
func (k tipProducerKey) topic() string {
	return "tip:" + k.asset + "/" + k.quote + "/" + strconv.Itoa(k.window)
}

type tipProducer struct {
	refs   int
	cancel context.CancelFunc
	linger *time.Timer
	// minter is the caller charged for this entry. When it closes its last stream while others
	// still hold the entry the charge moves to one of them; after the last holder leaves it is
	// held through the linger and released only when the entry is deleted.
	minter string
	// holders counts refs per caller; entries are deleted at zero.
	holders map[string]int
	// start and logger are kept so [tipProducerRegistry.respawnIfLive]
	// can relaunch the compute loop for THIS entry.
	start  func(ctx context.Context)
	logger *slog.Logger
	// ticks is the compute rate charged for this entry, per
	// [tipTicksPerMinute], held and released alongside minter's charge.
	ticks int
	// idleSeq orders lingering entries for eviction (lower = idle longer);
	// meaningful only while refs == 0.
	idleSeq uint64
}

// tipProducerRestartBackoff bounds how fast a producer that keeps exiting on its own (e.g. a panic
// on every tick) respawns, so a broken pair becomes a slow retry loop, not a tight spin that pegs
// a core and floods stellarindex_worker_panics_total.
const tipProducerRestartBackoff = time.Second

type tipProducerRegistry struct {
	mu     sync.Mutex
	active map[tipProducerKey]*tipProducer
	// lingerFor overrides tipProducerLinger when > 0 (tests).
	lingerFor time.Duration
	// restartBackoffFor overrides tipProducerRestartBackoff when > 0 (tests).
	restartBackoffFor time.Duration
	// gauge overrides obs.APITipProducers when non-nil (tests: the
	// package-level gauge also moves with other tests' linger timers).
	gauge prometheus.Gauge
	// maxProducers overrides defaultMaxTipProducers when > 0; a negative value disables the
	// ceiling, an operator's explicit escape hatch.
	maxProducers int
	// maxPerCaller overrides defaultMaxTipProducersPerCaller when > 0. A
	// negative value disables the per-caller quota.
	maxPerCaller int
	// minted counts the registered producers each caller minted; entries are deleted at zero, so
	// the map is bounded by the callers currently holding a producer.
	minted map[string]int
	// maxTicks / maxTicksPerCaller override defaultMaxTipTicksPerMinute /
	// defaultMaxTipTicksPerMinutePerCaller when > 0; negative disables.
	maxTicks          int
	maxTicksPerCaller int
	// ticks is the aggregate compute rate charged by every registered
	// producer; mintedTicks is its per-caller split, deleted at zero.
	ticks       int
	mintedTicks map[string]int
	// refused counts acquire calls turned away by ANY bound, so a
	// flood is visible rather than merely survived.
	refused uint64
	// refusedPerCaller counts refusals by a per-caller bound: "one client is enumerating the key
	// space", not "the deployment has outgrown its ceiling".
	refusedPerCaller uint64
	// idleSeq is the last [tipProducer.idleSeq] handed out.
	idleSeq uint64
	// tickConcurrency overrides defaultTipTickConcurrency when > 0 (tests).
	tickConcurrency int
	// tickSlots is the tick-compute semaphore, sized on first use.
	tickSlots chan struct{}
}

// acquireTickSlot blocks until a tick-compute slot is free, or returns
// ctx's error when the tick's budget runs out first.
func (r *tipProducerRegistry) acquireTickSlot(ctx context.Context) (release func(), err error) {
	r.mu.Lock()
	if r.tickSlots == nil {
		n := defaultTipTickConcurrency
		if r.tickConcurrency > 0 {
			n = r.tickConcurrency
		}
		r.tickSlots = make(chan struct{}, n)
	}
	slots := r.tickSlots
	r.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("tip tick slot: %w", ctx.Err())
	}
}

// computeTipGated is computeTip for a producer tick, holding a tick slot
// ([defaultTipTickConcurrency]) for exactly the compute and releasing it on every exit, including
// a panic the producer's recover absorbs.
func (s *Server) computeTipGated(ctx context.Context, asset, quote canonical.Asset, window int) (PriceSnapshot, []string, error) {
	release, err := s.tipProducers.acquireTickSlot(ctx)
	if err != nil {
		return PriceSnapshot{}, nil, err
	}
	defer release()
	return s.computeTip(ctx, asset, quote, window)
}

// limit is the effective producer ceiling; <= 0 from the operator means
// "no ceiling".
func (r *tipProducerRegistry) limit() int {
	if r.maxProducers != 0 {
		return r.maxProducers
	}
	return defaultMaxTipProducers
}

// callerLimit is the effective per-caller producer quota; <= 0 from the
// operator means "no per-caller quota".
func (r *tipProducerRegistry) callerLimit() int {
	if r.maxPerCaller != 0 {
		return r.maxPerCaller
	}
	return defaultMaxTipProducersPerCaller
}

// producersGauge is the registered-producer gauge this registry moves.
func (r *tipProducerRegistry) producersGauge() prometheus.Gauge {
	if r.gauge != nil {
		return r.gauge
	}
	return obs.APITipProducers
}

// refuseTipProducer counts a refusal by its reason and returns it.
func refuseTipProducer(o tipProducerOutcome) tipProducerOutcome {
	obs.APITipProducersRefusedTotal.WithLabelValues(o.String()).Inc()
	return o
}

// tickLimit is the effective aggregate compute-rate budget in ticks per
// minute; <= 0 from the operator means "no budget".
func (r *tipProducerRegistry) tickLimit() int {
	if r.maxTicks != 0 {
		return r.maxTicks
	}
	return defaultMaxTipTicksPerMinute
}

// callerTickLimit is the effective per-caller compute-rate share in ticks
// per minute; <= 0 from the operator means "no per-caller share".
func (r *tipProducerRegistry) callerTickLimit() int {
	if r.maxTicksPerCaller != 0 {
		return r.maxTicksPerCaller
	}
	return defaultMaxTipTicksPerMinutePerCaller
}

// overBudget reports whether charging cost on top of held would pass lim;
// lim <= 0 is no bound.
func overBudget(lim, held, cost int) bool {
	return lim > 0 && held+cost > lim
}

// admitLocked decides whether caller may mint a NEW producer costing ticks, counting any refusal.
// Per-caller bounds come FIRST so a client past its share is turned away before consuming another
// shared slot; the global bounds are then reached by many callers, not one.
func (r *tipProducerRegistry) admitLocked(caller string, ticks int) tipProducerOutcome {
	victims, outcome := r.planAdmitLocked(caller, ticks)
	if outcome != tipProducerAdmitted {
		r.refused++
		if outcome.callerRefusal() {
			r.refusedPerCaller++
		}
		return refuseTipProducer(outcome)
	}
	for _, key := range victims {
		r.removeLocked(key, r.active[key])
	}
	return outcome
}

// planAdmitLocked is the side-effect-free half of admitLocked: the verdict
// and the lingering producers that would be evicted to reach it.
func (r *tipProducerRegistry) planAdmitLocked(caller string, ticks int) ([]tipProducerKey, tipProducerOutcome) {
	attributed := caller != unattributedTipCaller
	switch {
	case attributed && overBudget(r.callerLimit(), r.minted[caller], 1):
		return nil, tipProducerAtCallerQuota
	case attributed && overBudget(r.callerTickLimit(), r.mintedTicks[caller], ticks):
		return nil, tipProducerAtCallerRateBudget
	}
	return r.planRoomLocked(ticks)
}

// wouldAdmit reports, reserving nothing and counting no refusal, whether acquireFor would admit
// caller on key. The stream handler asks it before its DB pre-flight so a refusal costs no reads;
// the real acquire stays authoritative.
func (r *tipProducerRegistry) wouldAdmit(key tipProducerKey, caller string) tipProducerOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.active[key]; running {
		return tipProducerAdmitted
	}
	_, outcome := r.planAdmitLocked(caller, tipTicksPerMinute(key.window))
	return outcome
}

// planRoomLocked fits a new producer costing ticks under the global bounds, evicting the
// longest-idle lingering producers if needed: a lingering entry is a reconnect optimisation
// serving nobody and must not refuse a real viewer. Subscribed producers are never evicted, and
// nothing is evicted unless it actually makes room.
func (r *tipProducerRegistry) planRoomLocked(ticks int) ([]tipProducerKey, tipProducerOutcome) {
	slots, held := len(r.active), r.ticks
	fits := func() bool {
		return !overBudget(r.limit(), slots, 1) && !overBudget(r.tickLimit(), held, ticks)
	}
	var victims []tipProducerKey
	if !fits() {
		for _, key := range r.lingeringLocked() {
			victims = append(victims, key)
			slots--
			held -= r.active[key].ticks
			if fits() {
				break
			}
		}
	}
	switch {
	case overBudget(r.limit(), slots, 1):
		return nil, tipProducerAtGlobalCeiling
	case overBudget(r.tickLimit(), held, ticks):
		return nil, tipProducerAtGlobalRateBudget
	}
	return victims, tipProducerAdmitted
}

// lingeringLocked lists the keys of zero-subscriber entries, longest idle
// first.
func (r *tipProducerRegistry) lingeringLocked() []tipProducerKey {
	var keys []tipProducerKey
	for key, p := range r.active {
		if p.refs == 0 {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b tipProducerKey) int {
		return cmp.Compare(r.active[a].idleSeq, r.active[b].idleSeq)
	})
	return keys
}

// removeLocked stops p and deletes its entry. The minter's quota slot returns only HERE, when the
// entry leaves the registry: returning it at connection close would free it while the producer
// still lingers, the window the abort-loop flood exploits.
func (r *tipProducerRegistry) removeLocked(key tipProducerKey, p *tipProducer) {
	if p.linger != nil {
		p.linger.Stop()
		p.linger = nil
	}
	p.cancel()
	delete(r.active, key)
	r.producersGauge().Dec()
	r.dischargeLocked(p)
}

// chargeLocked records that caller minted one more producer costing ticks.
// The aggregate rate is charged even for an unattributed caller.
func (r *tipProducerRegistry) chargeLocked(caller string, ticks int) {
	r.ticks += ticks
	r.chargeCallerLocked(caller, ticks)
}

// chargeCallerLocked adds one producer costing ticks to caller's quota.
func (r *tipProducerRegistry) chargeCallerLocked(caller string, ticks int) {
	if caller == unattributedTipCaller {
		return
	}
	if r.minted == nil {
		r.minted = make(map[string]int)
	}
	if r.mintedTicks == nil {
		r.mintedTicks = make(map[string]int)
	}
	r.minted[caller]++
	r.mintedTicks[caller] += ticks
}

// dischargeLocked gives p's minter its slot and rate back when p leaves
// the registry.
func (r *tipProducerRegistry) dischargeLocked(p *tipProducer) {
	r.ticks -= p.ticks
	r.dischargeCallerLocked(p.minter, p.ticks)
}

// dischargeCallerLocked removes one producer costing ticks from caller's
// quota.
func (r *tipProducerRegistry) dischargeCallerLocked(caller string, ticks int) {
	if caller == unattributedTipCaller {
		return
	}
	decrementOrDelete(r.minted, caller, 1)
	decrementOrDelete(r.mintedTicks, caller, ticks)
}

// transferChargeLocked moves p's per-caller charge from a minter that no longer holds it to a
// caller that does. The quota must meter what a caller controls: charging a departed minter would
// refuse it new pairs while it holds none, while dropping the charge would let a second address
// hold producers alive for free. The smallest attributed holder is chosen so the successor is
// deterministic.
func (r *tipProducerRegistry) transferChargeLocked(p *tipProducer) {
	next := unattributedTipCaller
	for c := range p.holders {
		if c != unattributedTipCaller && (next == unattributedTipCaller || c < next) {
			next = c
		}
	}
	r.dischargeCallerLocked(p.minter, p.ticks)
	r.chargeCallerLocked(next, p.ticks)
	p.minter = next
}

// decrementOrDelete subtracts n from m[k], deleting the entry at zero so
// the map cannot outgrow the set of callers currently holding a charge.
func decrementOrDelete(m map[string]int, k string, n int) {
	if v := m[k]; v <= n {
		delete(m, k)
	} else {
		m[k] = v - n
	}
}

// acquire is [tipProducerRegistry.acquireFor] with no principal to charge, for in-process callers.
// The HTTP path must always use acquireFor with a resolved caller
// (pinned by TestTipProducerCaller_KeysOnTheRotatableBlock).
//
//nolint:unparam // logger mirrors acquireFor's shape; only in-process
func (r *tipProducerRegistry) acquire(
	key tipProducerKey, logger *slog.Logger, start func(ctx context.Context),
) (release func(), ok bool) {
	release, outcome := r.acquireFor(key, unattributedTipCaller, logger, start)
	return release, outcome == tipProducerAdmitted
}

// acquireFor ensures a producer runs for key (starting it in a fresh goroutine with a
// registry-owned context if none is) and holds a reference. The release func must be called
// exactly once, and only when the outcome is [tipProducerAdmitted].
//
// A NEW producer is refused when it would take caller past its quota or compute-rate share, or the
// registry past its producer ceiling or rate budget. Joining an ALREADY-RUNNING producer is always
// allowed and never charged a new producer (a join during the linger takes over the departed
// minter's charge): refusing it would turn away a popular pair's viewers at no saving and defeat
// the page-reload case the linger exists for.
func (r *tipProducerRegistry) acquireFor(
	key tipProducerKey, caller string, logger *slog.Logger, start func(ctx context.Context),
) (release func(), outcome tipProducerOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[tipProducerKey]*tipProducer)
	}
	p, exists := r.active[key]
	if exists {
		if p.linger != nil {
			p.linger.Stop()
			p.linger = nil
		}
		p.refs++
		p.holders[caller]++
		// A join during the linger: the minter holds nothing, so the
		// charge moves to the holder now keeping the entry alive.
		if _, holding := p.holders[p.minter]; !holding {
			r.transferChargeLocked(p)
		}
	} else {
		ticks := tipTicksPerMinute(key.window)
		if outcome := r.admitLocked(caller, ticks); outcome != tipProducerAdmitted {
			return nil, outcome
		}
		// The cancel func is not lost (gosec G118 false positive): it is stored on the producer
		// and invoked by the linger timer in release().
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec
		p = &tipProducer{
			refs: 1, holders: map[string]int{caller: 1}, cancel: cancel,
			minter: caller, ticks: ticks, start: start, logger: logger,
		}
		r.active[key] = p
		r.producersGauge().Inc()
		r.chargeLocked(caller, ticks)
		// `start` is a func VALUE, so the static walk cannot see its recover guard; guarding HERE
		// makes the guarantee checkable at the `go` statement. A producer that exits uncancelled
		// (a recovered panic) is relaunched on the same entry by [tipProducerRegistry.respawnIfLive],
		// so a CURRENT subscriber is not left with heartbeats.
		r.spawn(ctx, key, p)
	}
	return func() { r.release(key, caller) }, tipProducerAdmitted
}

func (r *tipProducerRegistry) release(key tipProducerKey, caller string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.active[key]
	if !ok {
		return
	}
	p.refs--
	decrementOrDelete(p.holders, caller, 1)
	if p.refs > 0 {
		if _, holding := p.holders[p.minter]; !holding {
			r.transferChargeLocked(p)
		}
		return
	}
	linger := r.lingerFor
	if linger <= 0 {
		linger = tipProducerLinger
	}
	r.idleSeq++
	p.idleSeq = r.idleSeq
	// Last subscriber gone: linger, then stop — unless someone re-acquires.
	p.linger = time.AfterFunc(linger, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		cur, still := r.active[key]
		if !still || cur != p || cur.refs > 0 {
			return
		}
		r.removeLocked(key, cur)
	})
}

// spawn runs p.start on a worker.Recover-guarded goroutine and hands its exit to
// [tipProducerRegistry.respawnIfLive]. ctx must be the context p.cancel cancels.
func (r *tipProducerRegistry) spawn(ctx context.Context, key tipProducerKey, p *tipProducer) {
	go func() {
		defer worker.Recover(p.logger, "api-sse-price_tip_shared")
		defer r.respawnIfLive(ctx, key, p)
		p.start(ctx)
	}()
}

// respawnIfLive is spawn's exit hook. A recovered panic leaves the entry live with refs>0 but
// nothing computing, and release() only starts the linger at refs==0, so without a respawn a
// connected subscriber would get heartbeats only. A cancelled ctx means the linger ended it
// deliberately and it is never respawned; otherwise, after [tipProducerRestartBackoff], the loop
// is relaunched if key's entry is still THIS record. refs and the caller charge are untouched.
func (r *tipProducerRegistry) respawnIfLive(ctx context.Context, key tipProducerKey, p *tipProducer) {
	if ctx.Err() != nil {
		return
	}
	backoff := r.restartBackoffFor
	if backoff <= 0 {
		backoff = tipProducerRestartBackoff
	}
	time.AfterFunc(backoff, func() {
		r.mu.Lock()
		cur, still := r.active[key]
		r.mu.Unlock()
		if !still || cur != p || ctx.Err() != nil {
			return
		}
		r.spawn(ctx, key, p)
	})
}

// running reports how many producers are currently registered
// (running or lingering). Diagnostic + test hook.
func (r *tipProducerRegistry) running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// refusedCount reports how many acquires either bound has turned away
// since process start. Cumulative, like streaming.StreamsRejected.
func (r *tipProducerRegistry) refusedCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

// refusedPerCallerCount reports the subset of [refusedCount] turned away
// by the per-caller quota. Cumulative.
func (r *tipProducerRegistry) refusedPerCallerCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusedPerCaller
}

// mintedFor reports how many registered producers caller currently holds
// a charge for. Diagnostic + test hook.
func (r *tipProducerRegistry) mintedFor(caller string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.minted[caller]
}

// SetMaxTipProducers overrides the shared-tip-producer ceiling (negative disables it); call once at startup.
func (s *Server) SetMaxTipProducers(n int) {
	s.tipProducers.mu.Lock()
	defer s.tipProducers.mu.Unlock()
	s.tipProducers.maxProducers = n
}

// SetMaxTipProducersPerCaller overrides the per-caller producer quota (negative disables it); call once at startup.
func (s *Server) SetMaxTipProducersPerCaller(n int) {
	s.tipProducers.mu.Lock()
	defer s.tipProducers.mu.Unlock()
	s.tipProducers.maxPerCaller = n
}

// tipProducerCaller resolves the principal a minted producer is charged to: the trusted-proxy-aware
// client IP, aggregated to its /64 prefix for IPv6 ([ratelimit.ThrottleIPKey], shared by every
// per-IP cap) since ISPs delegate a whole /64 per subscriber and a /128 quota is bypassed by
// rotating low bits. An unresolvable caller collapses into [unknownTipCaller] rather than being
// exempted: fail closed.
func tipProducerCaller(r *http.Request) string {
	ip := middleware.RemoteIP(r)
	if ip == "" {
		return unknownTipCaller
	}
	return ratelimit.ThrottleIPKey(ip)
}

// acquireTipProducer ensures a shared producer runs for the pair and holds a reference, charging a
// NEW producer to the requesting caller. It returns the Hub topic and the release func.
//
// A reference is held only on [tipProducerAdmitted]; otherwise the caller has had its share or the
// registry is at capacity (count or compute rate). The caller must refuse the stream, not fall
// back to a per-connection loop, which would reintroduce the unbounded compute these bounds stop.
func (s *Server) acquireTipProducer(
	req *http.Request, asset, quote canonical.Asset, window int,
) (topic string, release func(), outcome tipProducerOutcome) {
	key := tipProducerKey{asset: asset.String(), quote: quote.String(), window: window}
	caller := tipProducerCaller(req)
	release, outcome = s.tipProducers.acquireFor(key, caller, s.logger, func(ctx context.Context) {
		s.runSharedTipProducer(ctx, key, asset, quote, window)
	})
	if outcome != tipProducerAdmitted {
		return "", nil, outcome
	}
	return key.topic(), release, tipProducerAdmitted
}

// runSharedTipProducer is the ONE compute loop for a pair: it computes the tip every `window`
// seconds and publishes to the Hub topic. The ring replays only to a subscriber sending
// Last-Event-ID; a fresh page paints from the handler's own synchronous first event.
func (s *Server) runSharedTipProducer(ctx context.Context, key tipProducerKey, asset, quote canonical.Asset, window int) {
	defer s.recoverStreamProducer("price_tip_shared")
	var gen streaming.Generator
	emit := func() {
		// ev.ID is dropped: the id on the wire is the one Publish mints
		// atomically with the ring insert.
		if ev, ok := s.tipTickEvent(ctx, &gen, asset, quote, window); ok {
			s.Hub.Publish(key.topic(), ev.Type, ev.Data)
		}
	}
	emit() // immediate first publish, to the subscribers already attached
	ticker := time.NewTicker(s.streamCadence(window))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}

// tipProducerPrecheck is the cheap refusal gate run before the stream's
// DB pre-flight; see [tipProducerRegistry.wouldAdmit].
func (s *Server) tipProducerPrecheck(req *http.Request, asset, quote canonical.Asset, window int) tipProducerOutcome {
	key := tipProducerKey{asset: asset.String(), quote: quote.String(), window: window}
	return s.tipProducers.wouldAdmit(key, tipProducerCaller(req))
}
