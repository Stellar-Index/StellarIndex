package v1

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Shared tip-stream producers (real-time program RT-1; audit 2026-08-04
// "tip stream = 6 DB queries/s PER CONNECTION"). The legacy shape ran
// one compute loop per CONNECTION, so N viewers of the same pair cost
// N× the tip computation and the shared pool saturated at ~2300
// streams. One producer now runs per DISTINCT (asset, quote, window)
// and publishes into the streaming Hub's topic ring; every connection
// is a plain Hub subscriber. Cost scales with distinct pairs being
// watched, not with viewers — the precondition for making every asset
// page hold a live stream ("this should feel alive", 2026-08-08).

// tipProducerLinger keeps a producer alive briefly after its last
// subscriber leaves, absorbing page reloads/reconnects without a
// stop/start churn of the compute loop.
const tipProducerLinger = 30 * time.Second

// defaultMaxTipProducers caps how many distinct tip producers may run at
// once (wave-D UNAUTH-DOS-1).
//
// The SSE caps count CONNECTIONS. A tip-stream connection also mints a
// DETACHED producer: its context comes from context.Background(), it
// outlives the request by design, and it survives the connection's
// release for tipProducerLinger. So the connection cap does not bound
// this — an unauthenticated client could open and immediately abort
// streams in a loop and leave an unbounded set of compute loops running,
// each polling the database on its own ticker, with no connection left
// to attribute them to.
//
// The key is (asset, quote, window_seconds), and window_seconds is
// CLIENT-CHOSEN in [1,60] — so the key space is pairs × 60, and an
// attacker does not even need distinct assets to enumerate it.
//
// The Hub's own topic reaper cannot shed this load either: it only
// evicts subscriber-less topics, and a live producer re-publishes to its
// topic every window, recreating it.
//
// 512 is generous against legitimate use — real fan-out is the set of
// pairs actually being watched, in the hundreds at most, and the
// explorer requests a single default window — while capping the worst
// case well short of what could starve the DB pool. Tune with
// [Server.SetMaxTipProducers].
//
// A lingering producer (no subscriber) still counts, because it still
// computes — but at either global bound the oldest lingering entries are
// evicted to admit a new one, so only subscribed demand can refuse a mint.
const defaultMaxTipProducers = 512

// defaultMaxTipProducersPerCaller caps how many producers ONE caller may
// have minted and still hold registered (running or lingering).
//
// The global [defaultMaxTipProducers] ceiling bounds the total, but on
// its own it is a pool partitioned by nothing: one unauthenticated
// address looping the key space — ~9 real pairs × window_seconds 1..60,
// aborting each connection as soon as the headers arrive — fills all 512
// slots with junk producers that survive the aborted connection for
// [tipProducerLinger], and from then on every OTHER caller's first
// request for a pair with no producer running is refused. The
// concurrent-connection caps cannot see this: the producer is detached
// (context.Background()) precisely so it outlives the request.
//
// So the ceiling has to be charged to a principal. Each producer is
// charged to the caller that MINTED it, for as long as its registry
// entry lives — releasing the connection is not enough to give the slot
// back, because the linger is exactly what the flood exploits.
//
// 24 is chosen against the shipped per-IP concurrent-stream cap
// ([config].api.max_streams_per_ip, default 20): a compliant caller can
// stream at most 20 distinct pairs at once, so 24 admits every producer
// it can legitimately be watching plus headroom for the linger overlap
// of navigating between pages, while capping one address at under 5% of
// the global pool — saturating it now takes 22 distinct addresses rather
// than one. Joining an ALREADY-RUNNING producer is never charged, so a
// page reload (the case the linger exists for) can never hit this.
// Tune with [Server.SetMaxTipProducersPerCaller].
const defaultMaxTipProducersPerCaller = 24

// defaultMaxTipTicksPerMinute bounds the AGGREGATE compute rate of every
// registered producer, in ticks per minute; each tick is one computeTip
// plus its divergence lookup against the database.
//
// The count ceilings above do not bound the rate: a producer ticks every
// window_seconds, client-chosen down to 1s, so 512 producers minted at a
// 1s window drive five times the query rate of 512 at the default 5s
// window the ceiling was sized against. The budget is exactly that
// sized-for rate (512 × 12 = 6144/min, ~102 computes/s): every slot stays
// usable at the default window, and a shorter window spends
// proportionally more of it.
const defaultMaxTipTicksPerMinute = defaultMaxTipProducers * (60 / defaultTipWindowSeconds)

// defaultMaxTipTicksPerMinutePerCaller is one caller's share of that rate,
// derived the same way from its count quota (24 × 12 = 288/min): a caller
// at the default window keeps its whole count quota, a caller at a 1s
// window gets four producers, and saturating the global budget still
// takes over 20 addresses.
const defaultMaxTipTicksPerMinutePerCaller = defaultMaxTipProducersPerCaller * (60 / defaultTipWindowSeconds)

// tipTicksPerMinute is the compute rate of a producer on a window-second
// ticker, rounded UP so a budget is never undercharged.
func tipTicksPerMinute(window int) int {
	window = max(window, minTipWindowSeconds)
	return (60 + window - 1) / window
}

// unattributedTipCaller is the caller identity for an acquire with no
// principal to charge — in-process/test callers that never came from an
// HTTP request. The per-caller quota does not apply to it. The HTTP path
// never produces it: [Server.acquireTipProducer] substitutes
// [unknownTipCaller] when the resolver cannot name the client.
const unattributedTipCaller = ""

// unknownTipCaller is the shared bucket for a request whose client IP
// cannot be resolved. Collapsing every such request into ONE bucket is
// deliberate (fail-closed, mirroring streaming.resolveStreamClientIP):
// an unresolvable caller must not be exempt from the quota.
const unknownTipCaller = "unknown"

// tipProducerOutcome is the verdict of an acquire. The two refusals are
// distinct states, not one "false": the caller-quota refusal names a
// client that has had its share, the ceiling refusal names a server at
// capacity, and an operator reading the log needs to tell them apart.
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
	// tipProducerAtCallerRateBudget: minting this producer would take the
	// caller's producers past its share of the compute rate — a short
	// window costs more than a long one. No release is handed back.
	tipProducerAtCallerRateBudget
	// tipProducerAtGlobalRateBudget: minting this producer would take the
	// registry's aggregate compute rate past its budget. No release is
	// handed back.
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
	// minter is the caller charged for this producer's registry entry.
	// The charge is held for the entry's whole life — through the linger
	// — and released only when the entry is deleted.
	minter string
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

// tipProducerRestartBackoff bounds how fast a producer that keeps
// exiting on its own (e.g. a panic on every tick) respawns, so a
// systematically broken pair degrades into a slow, logged retry loop
// rather than a tight spin that pegs a core and floods
// stellarindex_worker_panics_total.
const tipProducerRestartBackoff = time.Second

type tipProducerRegistry struct {
	mu     sync.Mutex
	active map[tipProducerKey]*tipProducer
	// lingerFor overrides tipProducerLinger when > 0 (tests).
	lingerFor time.Duration
	// gauge overrides obs.APITipProducers when non-nil (tests: the
	// package-level gauge also moves with other tests' linger timers).
	gauge prometheus.Gauge
	// maxProducers overrides defaultMaxTipProducers when > 0. A negative
	// value disables the ceiling (an operator's explicit choice, and the
	// escape hatch if the default is ever wrong for a deployment).
	maxProducers int
	// maxPerCaller overrides defaultMaxTipProducersPerCaller when > 0. A
	// negative value disables the per-caller quota.
	maxPerCaller int
	// minted counts the registered producers each caller minted. Entries
	// are deleted at zero, so the map cannot outgrow the set of callers
	// currently holding a producer.
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
	// refusedPerCaller counts the subset turned away by a per-caller
	// bound — the shape that says "one client is enumerating the key
	// space" rather than "the deployment has outgrown its ceiling".
	refusedPerCaller uint64
	// idleSeq is the last [tipProducer.idleSeq] handed out.
	idleSeq uint64
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

// admitLocked decides whether caller may mint a NEW producer costing
// ticks, counting any refusal (and its metric). Per-caller bounds come
// FIRST: a client that has had its share must be turned away before it
// can consume one more slot of the shared pool, so the global bounds are
// reached by many callers rather than by one.
func (r *tipProducerRegistry) admitLocked(caller string, ticks int) tipProducerOutcome {
	attributed := caller != unattributedTipCaller
	var outcome tipProducerOutcome
	switch {
	case attributed && overBudget(r.callerLimit(), r.minted[caller], 1):
		outcome = tipProducerAtCallerQuota
	case attributed && overBudget(r.callerTickLimit(), r.mintedTicks[caller], ticks):
		outcome = tipProducerAtCallerRateBudget
	default:
		outcome = r.makeRoomLocked(ticks)
	}
	if outcome != tipProducerAdmitted {
		r.refused++
		if outcome.callerRefusal() {
			r.refusedPerCaller++
		}
		return refuseTipProducer(outcome)
	}
	return outcome
}

// makeRoomLocked fits a new producer costing ticks under the global
// bounds, evicting the longest-idle lingering producers when that is
// what it takes. A lingering entry is a reconnect optimisation serving
// nobody; letting it refuse a real viewer made the ceiling bound past
// demand, not current demand. Subscribed producers are never evicted,
// and nothing is evicted unless the eviction actually makes room.
func (r *tipProducerRegistry) makeRoomLocked(ticks int) tipProducerOutcome {
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
		return tipProducerAtGlobalCeiling
	case overBudget(r.tickLimit(), held, ticks):
		return tipProducerAtGlobalRateBudget
	}
	for _, key := range victims {
		r.removeLocked(key, r.active[key])
	}
	return tipProducerAdmitted
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

// removeLocked stops p and deletes its entry. The minter's quota slot
// comes back only HERE, when the entry actually leaves the registry:
// releasing it when the connection closed would hand the slot back while
// the producer is still running out its linger — precisely the window
// the abort-loop flood exploits.
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
	if p.minter == unattributedTipCaller {
		return
	}
	decrementOrDelete(r.minted, p.minter, 1)
	decrementOrDelete(r.mintedTicks, p.minter, p.ticks)
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

// acquire is [tipProducerRegistry.acquireFor] with no principal to
// charge — for in-process callers that never came from a request, where
// there is no address to hold a quota against. The HTTP path must always
// go through acquireFor with a resolved caller; see
// [Server.acquireTipProducer], which cannot produce an unattributed one
// (pinned by TestTipProducerCaller_KeysOnTheRotatableBlock).
//
// callers reach this form and they have no logger to pass.
//
//nolint:unparam // logger mirrors acquireFor's shape; only in-process
func (r *tipProducerRegistry) acquire(
	key tipProducerKey, logger *slog.Logger, start func(ctx context.Context),
) (release func(), ok bool) {
	release, outcome := r.acquireFor(key, unattributedTipCaller, logger, start)
	return release, outcome == tipProducerAdmitted
}

// acquireFor ensures a producer runs for key (calling start in a fresh
// goroutine with a registry-owned context if none is running) and
// holds a reference to it. Returns the release func — call exactly
// once; idempotence is the caller's job.
//
// A NEW producer is refused when it would take caller past its quota of
// minted producers or its share of the compute rate, or the registry past
// its producer ceiling or aggregate rate budget. Joining an ALREADY-RUNNING producer is always allowed and
// never charged: refusing that would turn a popular pair's own viewers
// away while costing nothing to serve, which is the opposite of the
// protection intended — and it is the page-reload case the linger
// exists for. Callers must not use release unless the outcome is
// [tipProducerAdmitted].
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
	} else {
		ticks := tipTicksPerMinute(key.window)
		if outcome := r.admitLocked(caller, ticks); outcome != tipProducerAdmitted {
			return nil, outcome
		}
		// The cancel func is NOT lost (gosec G118 false positive): it is
		// stored on the producer record and invoked by the linger timer
		// in release() once the last reference is gone.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec
		p = &tipProducer{refs: 1, cancel: cancel, minter: caller, ticks: ticks, start: start, logger: logger}
		r.active[key] = p
		r.producersGauge().Inc()
		r.chargeLocked(caller, ticks)
		// `start` is a func VALUE, so the guard inside the producer it
		// wraps (runSharedTipProducer defers recoverStreamProducer) is
		// invisible to any static walk — including the one that keeps
		// this file's goroutines guarded. Registering it HERE makes the
		// guarantee checkable at the `go` statement, and it is a genuine
		// backstop for any future caller that passes an unguarded start.
		// A producer that exits without being cancelled (a recovered
		// panic) is relaunched on the same entry by
		// [tipProducerRegistry.respawnIfLive], so a CURRENT subscriber is
		// not left with heartbeats only until every viewer leaves.
		r.spawn(ctx, key, p)
	}
	return func() { r.release(key) }, tipProducerAdmitted
}

func (r *tipProducerRegistry) release(key tipProducerKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.active[key]
	if !ok {
		return
	}
	p.refs--
	if p.refs > 0 {
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

// spawn runs p.start on a goroutine guarded by worker.Recover and hands
// its exit to [tipProducerRegistry.respawnIfLive]. ctx must be the
// context p.cancel cancels.
func (r *tipProducerRegistry) spawn(ctx context.Context, key tipProducerKey, p *tipProducer) {
	go func() {
		defer worker.Recover(p.logger, "api-sse-price_tip_shared")
		defer r.respawnIfLive(ctx, key, p)
		p.start(ctx)
	}()
}

// respawnIfLive is spawn's exit hook. A recovered panic leaves the
// registry entry live with refs>0 but nothing computing, and release()
// only starts the linger once refs reaches 0 — so without a respawn a
// still-connected subscriber would get heartbeats only for as long as it
// stayed connected.
//
// ctx.Err() != nil means release()'s linger cancelled this producer
// deliberately: the normal stop path, never respawned. Otherwise, after
// [tipProducerRestartBackoff], the loop is relaunched on the same,
// still-live context if key's entry is still THIS record (not deleted,
// not replaced by a later acquire). refs and the caller charge are
// untouched: the entry never left the registry.
func (r *tipProducerRegistry) respawnIfLive(ctx context.Context, key tipProducerKey, p *tipProducer) {
	if ctx.Err() != nil {
		return
	}
	time.AfterFunc(tipProducerRestartBackoff, func() {
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

// TipProducersRunning reports the number of live shared tip producers.
// Exported for the operator diagnostics surface — a producer count that
// climbs while connections do not is the signature of the abort-loop
// flood the ceiling exists to stop.
func (s *Server) TipProducersRunning() int { return s.tipProducers.running() }

// TipProducersRefused reports the cumulative count of tip producers
// refused by either bound.
func (s *Server) TipProducersRefused() uint64 { return s.tipProducers.refusedCount() }

// TipProducersRefusedPerCaller reports the cumulative count of tip
// producers refused because one caller was already at its quota — the
// shape that distinguishes "a client is enumerating the key space" from
// "the deployment has outgrown its ceiling".
func (s *Server) TipProducersRefusedPerCaller() uint64 {
	return s.tipProducers.refusedPerCallerCount()
}

// SetMaxTipProducers overrides the shared-tip-producer ceiling. Pass a
// negative value to disable it. Call once at startup.
func (s *Server) SetMaxTipProducers(n int) {
	s.tipProducers.mu.Lock()
	defer s.tipProducers.mu.Unlock()
	s.tipProducers.maxProducers = n
}

// SetMaxTipProducersPerCaller overrides the per-caller producer quota.
// Pass a negative value to disable it. Call once at startup.
func (s *Server) SetMaxTipProducersPerCaller(n int) {
	s.tipProducers.mu.Lock()
	defer s.tipProducers.mu.Unlock()
	s.tipProducers.maxPerCaller = n
}

// tipProducerCaller resolves the principal a minted producer is charged
// to: the trusted-proxy-aware client IP, aggregated to its /64 prefix
// for IPv6.
//
// The prefix, not the address (SEC-15, mirroring
// middleware.remoteIPPrefixFor and streaming.maskStreamClientIP):
// residential and mobile ISPs delegate a whole /64 to one subscriber, so
// a quota keyed on the full /128 is bypassed by rotating the low bits —
// one fresh bucket per address, the quota never engages.
//
// An unresolvable caller collapses into [unknownTipCaller] rather than
// being exempted: fail-closed, the same posture the streaming per-IP cap
// takes.
func tipProducerCaller(r *http.Request) string {
	ip := middleware.RemoteIP(r)
	if ip == "" {
		return unknownTipCaller
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().Addr().String()
}

// acquireTipProducer ensures a shared producer runs for the pair and
// holds a reference to it, charging a NEW producer to the requesting
// caller. Returns the Hub topic to subscribe to and the release func.
//
// The outcome is [tipProducerAdmitted] only when a reference is held.
// Otherwise this pair has no producer running and one may not be minted
// — either the caller has had its share or the registry is at capacity,
// by producer count or by compute rate. The caller must refuse the stream rather than fall through to
// a per-connection loop, which would reintroduce exactly the unbounded
// compute these bounds are there to prevent.
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

// runSharedTipProducer is the ONE compute loop for a pair: computes the
// tip every `window` seconds and publishes to the Hub topic. The Hub's
// ring buffer gives late subscribers the most recent event immediately
// (resume semantics), so a fresh page paints without waiting a tick.
func (s *Server) runSharedTipProducer(ctx context.Context, key tipProducerKey, asset, quote canonical.Asset, window int) {
	defer s.recoverStreamProducer("price_tip_shared")
	var gen streaming.Generator
	emit := func() {
		// This budget covers the compute; the event's divergence lookup
		// takes its own shorter one inside it, like the per-connection
		// producer's tick.
		tickCtx, cancel := context.WithTimeout(ctx, tipStreamTickTimeout)
		defer cancel()
		snap, sources, err := s.computeTip(tickCtx, asset, quote, window)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("shared tip producer compute failed — skipping emit",
					"err", err, "asset", key.asset, "quote", key.quote)
			}
			return
		}
		if ev, ok := s.tipStreamEvent(tickCtx, &gen, asset, quote, snap, sources); ok {
			s.hub.Publish(key.topic(), ev.Type, ev.Data)
		}
	}
	emit() // immediate first publish — the ring serves it to every joiner
	ticker := time.NewTicker(time.Duration(window) * time.Second)
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
