package freeze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Marker is the JSON shape stored at the `freeze:<asset>:<quote>`
// Redis key. Carries diagnostic context the API doesn't read but
// operators want for log correlation when investigating frozen
// pairs.
type Marker struct {
	// AssetID + QuoteID echo the (asset, quote) the freeze applies
	// to. Lets a Redis dump be self-describing without needing the
	// key to be parsed.
	AssetID string `json:"asset_id"`
	QuoteID string `json:"quote_id"`

	// Action is the anomaly Decision Action — always "freeze" by
	// construction; the field exists so the value type is
	// future-proof if we ever extend the marker to cover
	// ActionWarn-style warnings.
	Action anomaly.Action `json:"action"`

	// Class is the asset class that drove the threshold lookup
	// (stablecoin / volatile / fiat / etc).
	Class anomaly.AssetClass `json:"class"`

	// DeviationPct is the deviation from the previous bucket's VWAP
	// that triggered the freeze.
	DeviationPct float64 `json:"deviation_pct"`

	// Reason is the human-readable explanation from the Decision.
	Reason string `json:"reason,omitempty"`

	// FrozenAt is when the writer wrote this marker. RFC 3339 UTC.
	// This is the WRITE time, refreshed on every lifecycle tick — not
	// the freeze's age. Read [State.FiredAt] for that.
	FrozenAt time.Time `json:"frozen_at"`

	// Windowed reports that this marker was written by a build that
	// records per-window ladders, i.e. that [Marker.Ladders] — and not
	// [Marker.State] — is the authority for "which ladder does window W
	// own". It is a separate flag rather than `len(Ladders) > 0` so an
	// EMPTY map (every window's ladder retired while the marker is kept
	// alive for a still-frozen sibling, see
	// [Writer.RetireWindowLadder]) cannot be mistaken for a marker
	// written before per-window ladders existed.
	Windowed bool `json:"windowed,omitempty"`

	// Ladders is the ADR-0019 lifecycle PER aggregation window, keyed by
	// the canonical [time.Duration] label ("5m0s", "1h0m0s", "24h0m0s").
	// Authoritative when [Marker.Windowed]; a window with no entry, or
	// whose entry fails [LadderStillLive], owns no ladder.
	//
	// The marker's key is (asset, quote) because its PRESENCE is the
	// single flag the API serves as `flags.frozen` for the whole pair —
	// but the lifecycle inside it advances per (pair, window), and the
	// windows run independently. One ladder per marker could therefore
	// only ever be one window's, with nothing to say whose: a window
	// arriving at the freeze step with a cold key (the aggregator drops
	// a window under its USD-volume floor BEFORE the confidence and
	// freeze steps, so a thin window's key stays cold across ticks)
	// adopted a SIBLING window's FiredAt, HoldUntil, ExtensionsUsed and
	// Escalated as its own — pinning a last-known-good price on a window
	// nothing was wrong with and, once the inherited ladder had
	// escalated, leaving a manual `freeze-unfreeze` as the only exit.
	// Carrying every window's ladder keeps each window's rehydrate its
	// own, INCLUDING across a restart, where every window is cold at
	// once and a single owner tag would strand every window but one.
	Ladders map[string]State `json:"ladders,omitempty"`

	// UnownedLadder is a ladder known to be live for the PAIR but with
	// no recorded owning window. It is what a window with no entry in
	// [Marker.Ladders] falls back to, and it exists so narrowing the
	// record can never DROP a freeze that is still running.
	//
	// Exactly two things produce one, and both are records that predate
	// or lack the window dimension:
	//
	//   - upgrading a marker written before [Marker.Ladders] existed:
	//     its pair-level [Marker.State] becomes the unowned ladder.
	//   - recovering from a durable ladder with no recorded owner: a
	//     `freeze_events` row written before migration 0163 (one
	//     pair-level ladder, no window), or any [LadderStore] that is not
	//     a [WindowLadderStore]. When the marker is gone and that ladder
	//     is still live, the first window to re-mark records it here, so
	//     the pair's OTHER windows — cold in the same recovery, and with
	//     no prev-VWAP comparator to re-fire on — do not read the freshly
	//     narrowed marker as "you were never frozen" and publish the
	//     bucket the freeze was withholding. (A per-window durable record
	//     needs none of this: each window's ladder is restored under its
	//     own label.)
	//
	// It is carried forward only while [LadderStillLive] holds for it:
	// an unowned ladder is a snapshot nobody is advancing, so it retires
	// itself once its own hold plus the grace has passed. Until then it
	// keeps the pair present, but a window may ADOPT it only within one
	// grace of [Marker.UnownedSince] — see [Writer.LoadStateForWindow].
	UnownedLadder State `json:"unowned_ladder,omitempty"`

	// UnownedSince is when [Marker.UnownedLadder] was recorded: the
	// upgrade or durable recovery that produced it, carried forward
	// unchanged. Zero when there is none.
	UnownedSince time.Time `json:"unowned_since,omitempty"`

	// State is the ADR-0019 freeze-lifecycle state (fired_at,
	// hold_until, extensions_used, escalated, unfreeze_streak). Zero
	// for markers written by the pre-lifecycle [Writer.Mark] path.
	//
	// Carried in the marker for two reasons: an operator dumping
	// `freeze:*` can see how far up the extension ladder a pair is
	// without reading logs, and the aggregator re-hydrates the ladder
	// from here after a restart instead of silently starting the
	// 2-hour escalation clock over.
	//
	// On a [Marker.Windowed] marker this mirrors the most recently
	// written window's ladder and is NOT the per-window authority: it
	// exists so a reader from before [Marker.Ladders] — a rolled-back
	// binary, an operator script — keeps reading exactly what it read
	// before. [Writer.LoadStateForWindow] reads Ladders.
	State State `json:"state,omitempty"`
}

// RedisCache is the subset of the Redis client the Writer, Looker and
// Recovery worker need. Declared as an interface so tests can
// substitute miniredis without pulling the full UniversalClient
// surface.
type RedisCache interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	SetArgs(ctx context.Context, key string, value any, a redis.SetArgs) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// EventSink is the optional durable-mirror seam for freeze events.
// The Writer calls RecordFreeze on every Mark; the implementation
// is responsible for de-duplicating against the still-firing row
// (so refreshing a Redis TTL doesn't create N rows in postgres).
//
// Nil sinks are valid — pre-existing deployments without a sink
// keep their Redis-only behaviour. Production wires
// `internal/storage/timescale.FreezeEventSink` here; tests pass
// either nil or a fake.
//
// Per docs/architecture/explorer-data-inventory.md §11.3: this
// migrates the Redis-only freeze state into a queryable postgres
// timeline that powers /v1/anomalies.
type EventSink interface {
	// RecordFreeze persists a freeze event. Idempotent against the
	// "currently firing" row for (asset, quote): if a row with
	// recovered_at IS NULL already exists for this pair, the call
	// is a no-op. Otherwise INSERT a new row with frozen_at=now.
	//
	// frozenValue is the last-known-good VWAP we're freezing on,
	// formatted as a fixed-precision decimal string (matching the
	// API wire shape — the orchestrator passes
	// `formatRatFixed(prev, 12)`). Empty string is allowed when no
	// prior bucket exists (first-tick freeze) — implementations
	// stamp NULL or 0 in that case, but must not forward that
	// storage filler to anything that reads it as a price.
	//
	// Implementations must NOT block the Writer's hot path on
	// network failures — log + continue. The Redis marker write
	// is the load-bearing operation; the durable mirror is best-
	// effort.
	RecordFreeze(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error
}

// LadderStore is the durable home for the ADR-0019 lifecycle [State].
// Implemented by `internal/storage/timescale.FreezeEventSink` against the
// migration-0119 columns on `freeze_events`.
//
// Why this exists at all. The ladder would otherwise live only in volatile
// places: the aggregator's in-process state map and a JSON blob inside the
// Redis marker. Redis is a cache — possibly without persistence, flushed
// during incidents — and the orchestrator reads a MISSING marker under a
// live freeze as the ADR-0019 operator override. So a Redis flush would not
// merely forget how far a pair had climbed the ladder: the next tick would
// RELEASE every live freeze, and a pair at ESCALATED — which ADR-0019 holds
// "until manual unfreeze" — would silently unfreeze and republish the price
// a P1 had already escalated to a human.
//
// Optional, and separate from [EventSink] on purpose: a deployment that
// wires no ladder store keeps precisely the pre-0119 Redis-only behaviour,
// which is the same "nil sink = legacy" idiom this file already uses for
// the durable mirror.
//
// Implementations must be concurrent-safe. Both methods are best-effort
// from the Writer's perspective — a store failure must never take down the
// Redis write, which remains the serving path's authority for
// `flags.frozen`.
type LadderStore interface {
	// SaveLadder persists `state` as the current lifecycle for
	// (asset, quote). Called on EVERY lifecycle transition, so it must be
	// idempotent; the whole state is written each time, which makes a lost
	// write self-correcting on the next tick.
	//
	// Implementations must NOT create a freeze record — [EventSink] owns
	// row creation. "Nothing to update" IS an error (timescale.ErrNotFound):
	// zero rows affected means the open row is missing — most likely
	// migration 0119 has not been applied under this binary — and callers
	// count it via AnomalyFreezeLadderWriteFailuresTotal.
	SaveLadder(ctx context.Context, asset, quote canonical.Asset, state State) error

	// LoadLadder returns the durable lifecycle for (asset, quote) and
	// whether one exists.
	//
	// ok=false means "this pair has no durable open freeze": never frozen,
	// already recovered, or — critically — force-unfrozen by an operator,
	// since `stellarindex-ops freeze-unfreeze` stamps recovered_at as the
	// second half of the override. Distinguishing that from "Redis lost the
	// marker" is the entire point of the store.
	LoadLadder(ctx context.Context, asset, quote canonical.Asset) (State, bool, error)
}

// WindowLadderStore is the window-aware half of [LadderStore]
// (migration 0163): the same durable record, told WHICH window's ladder
// is being written and able to answer for one window at a time.
//
// It exists because [LadderStore] is keyed (asset, quote) while the
// ADR-0019 lifecycle advances per (pair, window). Every frozen window
// mirrors its ladder on every tick, so the pair-keyed record was simply
// whichever window wrote LAST — and the durable ladder is read at exactly
// one moment, after Redis has lost the marker, when it is the only
// authority left. A 5m window's fresh ten-minute ladder overwriting a 1h
// window's ESCALATED one therefore brought the escalated freeze back as
// an ordinary one that auto-unfreezes; the same record, copied onto every
// cold window, also froze windows nothing was wrong with.
//
// Optional, in the same idiom as the orchestrator's window-aware marker
// interface: a store that cannot scope a ladder keeps the pair-wide
// behaviour, which errs towards holding a freeze. Production wires
// `internal/storage/timescale.FreezeEventSink`, which implements both.
//
// Implementations MUST keep the pair-level [LadderStore] view in step as
// a fail-closed summary of the live per-window ladders (furthest hold,
// highest rung, escalated if any window is). [Recovery] and
// `stellarindex-ops freeze-unfreeze` read that view and need no window.
type WindowLadderStore interface {
	// SaveWindowLadder records `state` as `window`'s ladder on the pair's
	// open freeze record, leaving every other window's untouched. An
	// inactive `state` retires the window's ladder. Same "never creates a
	// record, zero rows is an error" contract as [LadderStore.SaveLadder].
	SaveWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration, state State) error

	// LoadWindowLadders returns every window's durable ladder for the
	// pair, verbatim (the caller applies [LadderStillLive]).
	//
	// `unowned` is a ladder the record holds with no owning window: the
	// pair-level ladder of a row written before 0163. It answers for any
	// window with no entry of its own, because narrowing it to "nobody's"
	// would drop a freeze that is still running.
	//
	// ok=false carries the [LadderStore.LoadLadder] meaning exactly: no
	// open record, or one whose ladder has been retired.
	LoadWindowLadders(ctx context.Context, asset, quote canonical.Asset) (ladders map[time.Duration]State, unowned State, ok bool, err error)
}

// Writer marks a (asset, quote) pair as frozen by writing a
// [Marker] to Redis at the `freeze:<asset>:<quote>` key with the
// configured TTL. Constructed by the aggregator orchestrator at
// startup.
//
// When an EventSink is wired, the Writer also records the freeze
// to the durable mirror — postgres-backed in production, used by
// the explorer's /anomalies timeline.
//
// Safe for concurrent Mark calls — fields are read-only after
// construction; the underlying RedisCache is concurrent-safe by
// contract; the EventSink contract requires concurrent-safety.
type Writer struct {
	cache  RedisCache
	ttl    time.Duration
	sink   EventSink
	ladder LadderStore
	// windowLadder is `ladder` when it can scope a durable ladder to the
	// window that owns it ([WindowLadderStore]), nil otherwise. Resolved
	// once in [WithLadderStore].
	windowLadder WindowLadderStore
	// ladderGrace is how far past a durable hold's expiry the ladder is
	// still honoured on a marker miss — see [WithLadderStore].
	ladderGrace time.Duration
	// clock is the time every ladder-liveness read and marker stamp is
	// taken at; nil is the wall clock. See [WithClock].
	clock func() time.Time
}

// now is the writer's clock: the injected one, else the wall clock.
func (w *Writer) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// NewWriter constructs a Writer. ttl=0 falls back to
// cachekeys.FreezeTTL — the TTL for the lifecycle-free [Writer.Mark]
// path only; [Writer.MarkHold] takes its TTL from the caller's
// lifecycle [Outcome] instead.
//
// sink is optional (nil = legacy Redis-only behaviour); production
// passes the timescale-backed implementation.
func NewWriter(cache RedisCache, ttl time.Duration, opts ...WriterOption) (*Writer, error) {
	if cache == nil {
		return nil, errors.New("freeze: RedisCache is required")
	}
	if ttl <= 0 {
		ttl = cachekeys.FreezeTTL
	}
	w := &Writer{cache: cache, ttl: ttl}
	for _, opt := range opts {
		opt(w)
	}
	if w.ladderGrace <= 0 {
		w.ladderGrace = DefaultLadderGrace
	}
	return w, nil
}

// WriterOption tunes a Writer at construction time.
type WriterOption func(*Writer)

// WithClock sets the clock [LadderStillLive] and the marker stamps are
// read against, so a caller driving the lifecycle on its own clock (the
// orchestrator's Signal.Now) judges a ladder's hold on that same clock.
func WithClock(clock func() time.Time) WriterOption {
	return func(w *Writer) { w.clock = clock }
}

// WithEventSink wires the durable freeze-event mirror. Pass
// `internal/storage/timescale.FreezeEventSink` in production; tests
// can inject a fake or omit the option entirely (nil sink = no
// mirror, same as the pre-Phase-2 behaviour).
func WithEventSink(sink EventSink) WriterOption {
	return func(w *Writer) {
		w.sink = sink
	}
}

// DefaultLadderGrace is how far past a durable hold's expiry
// [Writer.LoadState] still honours the stored ladder when the Redis marker
// is gone.
//
// It is the durable twin of [DefaultMarkerGrace], and deliberately the same
// value: the marker's TTL is already `remaining hold + MarkerGrace`, so a
// durable ladder honoured over exactly the same span makes losing Redis
// behaviour-neutral rather than behaviour-changing. Too short and a flush
// during the last seconds of a hold still drops the freeze; too long and a
// long-dead aggregator resurrects a stale freeze on restart, which is the
// failure mode the bound exists to prevent.
var DefaultLadderGrace = DefaultMarkerGrace

// WithLadderStore wires the durable ADR-0019 ladder (migration 0119).
// Production passes `internal/storage/timescale.FreezeEventSink`; tests
// pass a fake or omit the option for the pre-0119 Redis-only behaviour.
//
// With a store wired, [Writer.MarkHold] mirrors the lifecycle state to it on
// every transition and [Writer.LoadState] falls back to it when the Redis
// marker is missing — the two halves that let an escalated freeze survive a
// Redis flush.
//
// grace <= 0 falls back to [DefaultLadderGrace].
func WithLadderStore(store LadderStore, grace time.Duration) WriterOption {
	return func(w *Writer) {
		w.ladder = store
		w.ladderGrace = grace
		// A store that records which window owns each ladder lets every
		// window rehydrate ITS OWN freeze after a Redis loss instead of
		// the last writer's — see [WindowLadderStore].
		w.windowLadder, _ = store.(WindowLadderStore)
	}
}

// Mark records a freeze for (asset, quote) backed by the supplied
// anomaly Decision, with the writer's flat TTL and no lifecycle
// state. Idempotent — overwriting an existing marker refreshes its
// TTL.
//
// This is NOT the freeze-duration policy. "The freeze lives as long
// as something keeps re-marking it" was the pre-lifecycle behaviour
// and it is exactly the defect ADR-0019's extension ladder exists to
// prevent: it makes the release condition the negation of the fire
// condition, evaluated on a single bucket. Callers that own a pair's
// freeze lifecycle call [Writer.MarkHold]; this entry point remains
// for the composite/triangulation refusal, which is a genuinely
// per-tick decision about a derived price.
//
// frozenValue is the last-known-good VWAP being frozen on, encoded
// as a fixed-precision decimal string (orchestrator passes
// `formatRatFixed(prev, 12)`); empty string when no prior bucket
// exists (first-tick freeze). Forwarded to the EventSink so the
// freeze_events table records the frozen-on price; not stored in
// the Redis marker because the API only needs the boolean flag.
//
// Returns the underlying error wrapped when the Redis write fails;
// callers log + continue (the next bucket close retries the write).
func (w *Writer) Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	return w.MarkHold(ctx, asset, quote, frozenValue, decision, State{}, w.ttl)
}

// MarkHold is [Writer.Mark] plus the ADR-0019 lifecycle: it stamps
// the freeze's [State] into the marker and sets the marker's TTL from
// the lifecycle's [Outcome.MarkerTTL] (the remaining hold plus the
// silence grace) instead of the writer's flat default.
//
// This is the call the orchestrator's freeze path uses. The flat-TTL
// [Writer.Mark] remains for callers with no lifecycle of their own —
// today the triangulated-composite freeze, whose refusal is a
// per-tick decision about a DERIVED price rather than a freeze on an
// observed pair.
//
// ttl <= 0 falls back to the writer's default, so a caller that
// forgets to plumb the outcome's TTL still gets a marker that expires.
func (w *Writer) MarkHold(
	ctx context.Context,
	asset, quote canonical.Asset,
	frozenValue string,
	decision anomaly.Decision,
	state State,
	ttl time.Duration,
) error {
	return w.markHold(ctx, asset, quote, 0, frozenValue, decision, state, ttl)
}

// MarkHoldForWindow is [Writer.MarkHold] for a caller whose freeze
// lifecycle is scoped to ONE window of the pair — which is every
// lifecycle caller, since ADR-0019's ladder advances per (pair, window)
// while this marker is keyed per (asset, quote).
//
// It records `state` as `window`'s ladder in [Marker.Ladders], MERGING
// with the ladders already in the marker so a sibling window's freeze
// survives this write. Every other window's ladder is preserved
// verbatim, including one this process knows nothing about (a window
// that has been sitting under the USD-volume floor since startup, so
// its ladder exists only in the marker) — which is why this is a merge
// and not a rewrite from the caller's in-memory view.
//
// An inactive `state` RETIRES the window's ladder, the same as
// [Writer.RetireWindowLadder], so a release can never leave a stale
// ladder behind for a restart to rehydrate.
//
// window <= 0 writes an unscoped marker, i.e. exactly
// [Writer.MarkHold]: the existing ladder map is carried over untouched.
func (w *Writer) MarkHoldForWindow(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
	frozenValue string,
	decision anomaly.Decision,
	state State,
	ttl time.Duration,
) error {
	return w.markHold(ctx, asset, quote, window, frozenValue, decision, state, ttl)
}

// markHold is the shared body of [Writer.MarkHold] and
// [Writer.MarkHoldForWindow].
func (w *Writer) markHold(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
	frozenValue string,
	decision anomaly.Decision,
	state State,
	ttl time.Duration,
) error {
	if ttl <= 0 {
		ttl = w.ttl
	}
	key := cachekeys.Freeze(asset, quote)
	label := windowLabel(window)
	now := w.now()
	windowed, ladders, unowned, unownedSince, prior := w.mergeLadders(ctx, asset, quote, label, state)
	marker := Marker{
		AssetID:       asset.String(),
		QuoteID:       quote.String(),
		Action:        decision.Action,
		Class:         decision.Class,
		DeviationPct:  decision.DeviationPct,
		Reason:        decision.Reason,
		FrozenAt:      now.UTC(),
		Windowed:      windowed,
		Ladders:       ladders,
		UnownedLadder: unowned,
		UnownedSince:  unownedSince,
		State:         state,
	}
	if label == "" && !state.Active() && prior != nil && w.lifecycleTTLFloor(*prior, "", now) > 0 {
		// The lifecycle-free [Writer.Mark] landing on a marker a live
		// ADR-0019 ladder owns. Mark is entitled to one thing — the pair
		// carrying `flags.frozen` for its flat TTL — and the marker's
		// presence already is that. Everything else in it belongs to the
		// lifecycle: the ladders, the pair-level State a legacy marker
		// keeps its only ladder in, and the owner's diagnostics. Rewriting
		// them is how the triangulated-composite refusal (whose targets
		// are members of the aggregator's own pair set) zeroed a legacy
		// marker's ladder and relabelled an escalated freeze as an
		// inherited one. Keep the owner's marker; only the expiry below
		// can move, and only outwards.
		marker = *prior
	}
	// One key, one TTL, several owners. Whoever writes must not pull the
	// expiry in under a hold somebody else is still serving: the marker
	// outlives every live ladder in it by the grace, which is what each
	// ladder's own writer would have asked for. Without the floor Mark's
	// flat five minutes replaced a 35-minute hold, and a 5m window's short
	// remainder truncated a 1h sibling's — either of which lapses the
	// marker, and with it the freeze, on the first tick the owning window
	// returns early instead of re-marking.
	if floor := w.lifecycleTTLFloor(marker, label, now); floor > ttl {
		ttl = floor
	}
	body, err := json.Marshal(marker)
	if err != nil {
		// Unreachable — Marker has no func/chan fields. Wrap for
		// diagnostic completeness.
		return fmt.Errorf("freeze: marshal marker: %w", err)
	}
	if err := w.cache.Set(ctx, key.String(), body, ttl).Err(); err != nil {
		return fmt.Errorf("freeze: cache set %s: %w", key, err)
	}

	// Durable mirror. Best-effort: a sink failure must not surface
	// to the caller because the Redis write — the load-bearing
	// operation that drives flags.frozen on the API response — has
	// already succeeded. The sink is for the explorer /anomalies
	// timeline, not for liveness.
	if w.sink != nil {
		if sinkErr := w.sink.RecordFreeze(ctx, asset, quote, frozenValue, decision); sinkErr != nil {
			// Swallowed on purpose: the Redis write above is the
			// load-bearing operation and has already succeeded, and
			// WARN-ing on every transient postgres blip would bury the
			// transitions that matter. Note this is genuinely SILENT —
			// neither the Writer nor the sink holds a logger — so the
			// operator signal for a broken mirror is the gap between
			// AnomalyFreezeEngagedTotal and the freeze_events row count,
			// not a log line. (The ladder half below is counted directly;
			// see saveLadder.)
			_ = sinkErr
		}
	}

	// Durable ladder (migration 0119). Mirrored on EVERY lifecycle
	// transition, not only on the fire, because the fields that matter
	// most — extensions_used and escalated — only ever change on a LATER
	// transition than the one that opened the row.
	//
	// Ordered after RecordFreeze deliberately: SaveLadder stamps the open
	// row and never creates one, so on a fresh freeze the row has to exist
	// first. Best-effort on the same grounds as the sink — the Redis write
	// above already succeeded and is the serving path's authority.
	//
	// Only for ACTIVE states: [Writer.Mark]'s lifecycle-free path passes a
	// zero State (the triangulated-composite refusal, which owns no
	// ladder), and stamping that would overwrite a real pair's ladder with
	// zeros the reader would then mistake for "fired just now".
	//
	// A window-scoped write goes to the window's OWN durable ladder when
	// the store can carry one (migration 0163). The pair-level write is
	// last-writer-wins across the pair's windows, which is how a 5m
	// window's fresh ladder came to replace a 1h window's ESCALATED one in
	// the only record that survives Redis.
	switch {
	case w.windowLadder != nil && window > 0:
		w.saveWindowLadder(ctx, asset, quote, window, state, "mark_hold")
	case w.ladder != nil && state.Active():
		w.saveLadder(ctx, asset, quote, state, "mark_hold")
	}
	return nil
}

// saveWindowLadder is [Writer.saveLadder] for one window's durable ladder.
// An inactive `state` retires the entry. Counted on the same failure
// counter, for the same reason: a durable ladder that silently is not
// there is only discovered when a Redis loss needs it.
func (w *Writer) saveWindowLadder(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
	state State,
	op string,
) {
	if err := w.windowLadder.SaveWindowLadder(ctx, asset, quote, window, state); err != nil {
		obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues(op).Inc()
	}
}

// saveLadder mirrors a lifecycle state to the durable store, counting every
// failure. Best-effort by contract — the Redis write is the serving path's
// authority and has already succeeded — but NOT silent.
//
// The silence mattered more than the failure. The Writer holds no logger and
// the timescale sink holds no logger either, so a persistently failing
// ladder write produced no signal anywhere: the freeze looked healthy on
// every surface right up until a Redis flush needed the ladder that was
// never written. The shape that makes this concrete is migration 0119 not
// yet applied while the new binary is already running — the deploy pipeline
// applies migrations before the binary, so a partially-failed deploy lands
// exactly there — in which case EVERY write returns "no row matched" and the
// durable ladder is uniformly absent.
//
// `op` is the low-cardinality call site (mark_hold | clear), so an operator
// can tell "we cannot record ladders" from "we cannot retire them".
func (w *Writer) saveLadder(ctx context.Context, asset, quote canonical.Asset, state State, op string) {
	if err := w.ladder.SaveLadder(ctx, asset, quote, state); err != nil {
		obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues(op).Inc()
	}
}

// RetireWindowLadder drops ONE window's ladder while keeping the marker (and `flags.frozen`) for
// a still-frozen sibling, so a restart cannot rehydrate an ended freeze. KEEPTTL: the remaining hold
// is the sibling's. No-op on an absent or pre-Ladders marker; best-effort, the caller logs.
func (w *Writer) RetireWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration) error {
	label := windowLabel(window)
	if label == "" {
		return nil
	}
	// Durable entry first, whatever the marker holds: it is what a Redis loss falls back to, and
	// retiring it recomputes the pair-level summary. Counted under op "clear".
	if w.windowLadder != nil {
		w.saveWindowLadder(ctx, asset, quote, window, State{}, "clear")
	}
	key := cachekeys.Freeze(asset, quote)
	marker, ok, err := w.readMarker(ctx, key)
	if err != nil {
		return err
	}
	if !ok || !marker.Windowed {
		return nil
	}
	if _, held := marker.Ladders[label]; !held {
		return nil
	}
	delete(marker.Ladders, label)
	body, err := json.Marshal(marker)
	if err != nil {
		// Unreachable — Marker has no func/chan fields. Wrap for
		// diagnostic completeness.
		return fmt.Errorf("freeze: marshal marker: %w", err)
	}
	// XX: a marker cleared since the read stays cleared; KEEPTTL on an absent key would recreate it
	// with no expiry.
	err = w.cache.SetArgs(ctx, key.String(), body, redis.SetArgs{Mode: "XX", KeepTTL: true}).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("freeze: cache set %s: %w", key, err)
	}
	return nil
}

// ReleaseWindow ends `window`'s freeze for a caller that believes it is the LAST frozen window,
// reporting whether the marker was kept. That belief misses floor-dropped windows and every window
// after a restart, and once let a sibling's recovery end an ESCALATED freeze, so the marker and then
// the durable record are asked; an unreadable record returns the error rather than unfreezing.
func (w *Writer) ReleaseWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (bool, error) {
	label := windowLabel(window)
	marker, ok, err := w.readMarker(ctx, cachekeys.Freeze(asset, quote))
	if err != nil {
		return true, err
	}
	if label == "" {
		// No window to scope a retire to: the unscoped release it always was.
		return false, w.Clear(ctx, asset, quote)
	}
	held := ok && marker.Windowed && w.siblingLadderHeld(marker, label)
	if !held {
		if held, err = w.durableSiblingHeld(ctx, asset, quote, window); err != nil {
			return true, err
		}
	}
	if held {
		return true, w.RetireWindowLadder(ctx, asset, quote, window)
	}
	return false, w.Clear(ctx, asset, quote)
}

// durableSiblingHeld reports whether the durable record holds a live ([LadderStillLive]) ladder
// for another window, or a live unowned one. False without a read for a non-window store (it has
// nothing to retire per window). A store ERROR is returned: here "absent" destroys the record.
func (w *Writer) durableSiblingHeld(ctx context.Context, asset, quote canonical.Asset, own time.Duration) (bool, error) {
	if w.windowLadder == nil {
		return false, nil
	}
	stored, unowned, ok, err := w.windowLadder.LoadWindowLadders(ctx, asset, quote)
	if err != nil {
		return false, fmt.Errorf("freeze: load durable ladders %s/%s: %w", asset.String(), quote.String(), err)
	}
	if !ok {
		return false, nil
	}
	now := w.now()
	for window, st := range stored {
		if window != own && LadderStillLive(st, w.ladderGrace, now) {
			return true, nil
		}
	}
	return LadderStillLive(unowned, w.ladderGrace, now), nil
}

// siblingLadderHeld reports whether `marker` records a freeze for any
// window other than `own`: a still-live owned ladder, or a still-live
// unowned one (owner unknown, so it may be a sibling's).
func (w *Writer) siblingLadderHeld(marker Marker, own string) bool {
	now := w.now()
	for label, st := range marker.Ladders {
		if label != own && LadderStillLive(st, w.ladderGrace, now) {
			return true
		}
	}
	return LadderStillLive(marker.UnownedLadder, w.ladderGrace, now)
}

// mergeLadders computes the marker's ladder map with `label` set to `state` (removed if
// inactive). It MERGES because the caller knows one window, and windows (even floor-dropped ones)
// freeze independently. A legacy marker is upgraded in place; empty `label` claims nothing.
func (w *Writer) mergeLadders(
	ctx context.Context,
	asset, quote canonical.Asset,
	label string,
	state State,
) (bool, map[string]State, State, time.Time, *Marker) {
	key := cachekeys.Freeze(asset, quote)
	marker, ok, err := w.readMarker(ctx, key)
	now := w.now()
	ladders := map[string]State{}
	windowed := false
	var unowned State
	var unownedSince time.Time
	// prior is the marker this write replaces, for the one caller that
	// may have to leave it as it is ([Writer.markHold]'s Mark branch).
	var prior *Marker
	if err == nil && ok {
		prior = &marker
	}
	switch {
	case err != nil || !ok:
		// No marker: a first freeze, or Redis lost it mid-freeze. Restore every live durable ladder
		// under its own label, since from here the MARKER answers cold windows.
		ladders, unowned = w.liveDurableLadders(ctx, asset, quote)
		windowed = len(ladders) > 0
		unownedSince = now
	case marker.Windowed:
		windowed = true
		for k, v := range marker.Ladders {
			// A floor-dropped window never retires its own ladder.
			if LadderStillLive(v, w.ladderGrace, now) {
				ladders[k] = v
			}
		}
		unowned = marker.UnownedLadder
		unownedSince = marker.UnownedSince
	default:
		// Upgrading a marker written before per-window ladders existed:
		// its pair-level ladder has no owner, so the windows that were
		// running it may adopt it on the upgrade tick.
		unowned = marker.State
		unownedSince = now
	}
	if !LadderStillLive(unowned, w.ladderGrace, now) {
		// Nobody is advancing an unowned snapshot; once its own hold
		// plus the grace has passed it describes no running freeze.
		unowned = State{}
		unownedSince = time.Time{}
	}
	if label == "" {
		// An unscoped write must not HIDE durable ladders it restored: a present-but-unwindowed marker
		// reads as "frozen pair-wide, this window never was" and publishes what an escalated freeze withheld.
		return windowed || len(ladders) > 0 || unowned.Active(), ladders, unowned, unownedSince, prior
	}
	if state.Active() {
		ladders[label] = state
	} else {
		delete(ladders, label)
	}
	return true, ladders, unowned, unownedSince, prior
}

// lifecycleTTLFloor is the longest `remaining hold + grace` over the marker's live ladders, skipping
// `own` (whose caller passes its own TTL); zero means no live lifecycle owns it. A pre-Ladders
// marker's pair-level State counts; on a windowed marker State is only a mirror.
func (w *Writer) lifecycleTTLFloor(marker Marker, own string, now time.Time) time.Duration {
	var floor time.Duration
	consider := func(st State) {
		if !LadderStillLive(st, w.ladderGrace, now) {
			return
		}
		if need := st.HoldUntil.Sub(now) + w.ladderGrace; need > floor {
			floor = need
		}
	}
	for label, st := range marker.Ladders {
		if label != own {
			consider(st)
		}
	}
	consider(marker.UnownedLadder)
	if !marker.Windowed {
		consider(marker.State)
	}
	return floor
}

// liveDurableLadder returns the durable ladder when still running, else zero (no store, no row,
// lapsed, or error). Used only on a marker miss; it degrades to the pre-0119 answer, never inventing a freeze.
func (w *Writer) liveDurableLadder(ctx context.Context, asset, quote canonical.Asset) State {
	if w.ladder == nil {
		return State{}
	}
	st, ok, err := w.ladder.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		return State{}
	}
	return st
}

// liveDurableLadders is [Writer.liveDurableLadder] per window, plus the unowned ladder; a non-window
// store yields its pair-level ladder as unowned. Lapsed entries are dropped, not copied into the marker.
func (w *Writer) liveDurableLadders(ctx context.Context, asset, quote canonical.Asset) (map[string]State, State) {
	ladders := map[string]State{}
	if w.windowLadder == nil {
		return ladders, w.liveDurableLadder(ctx, asset, quote)
	}
	stored, unowned, ok, err := w.windowLadder.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		return ladders, State{}
	}
	now := w.now()
	for window, st := range stored {
		if label := windowLabel(window); label != "" && LadderStillLive(st, w.ladderGrace, now) {
			ladders[label] = st
		}
	}
	return ladders, unowned
}

// readMarker fetches and decodes the marker at `key`. ok=false means no
// marker (or one that does not decode — treated as "nothing to merge
// with" by the writers, which then replace it).
func (w *Writer) readMarker(ctx context.Context, key cachekeys.FreezeKey) (Marker, bool, error) {
	raw, err := w.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, false, fmt.Errorf("freeze: cache get %s: %w", key, err)
	}
	var marker Marker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return Marker{}, false, nil //nolint:nilerr // an undecodable marker carries no ladder to merge with
	}
	return marker, true, nil
}

// windowLabel renders a lifecycle window as the [Marker.Ladders] key.
// The zero duration is the "unscoped" sentinel — the lifecycle-free
// [Writer.Mark] path, which carries no ladder to scope.
func windowLabel(window time.Duration) string {
	if window <= 0 {
		return ""
	}
	return window.String()
}

// LoadState reads the lifecycle state, falling back to the durable ladder when the marker is gone,
// since a Redis flush must not read as an override. A missing marker with an open, unlapsed row is
// PRESENT; no open row means ended (freeze-unfreeze stamps recovered_at; a raw `redis-cli DEL` is not
// an override); a lapsed row is not resurrected. An undecodable marker is (State{}, true, nil).
func (w *Writer) LoadState(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	return w.loadState(ctx, asset, quote, 0)
}

// LoadStateForWindow is [Writer.LoadState] for one window. Presence is pair-scoped; the ladder is
// Ladders[window], and a window with none gets zero rather than a sibling's (that pinned a healthy
// window). Pre-Ladders markers and ownerless durable ladders answer pair-wide, adopted only within one
// grace of UnownedSince, because Escalated never auto-releases.
func (w *Writer) LoadStateForWindow(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
) (State, bool, error) {
	return w.loadState(ctx, asset, quote, window)
}

// loadState is the shared body of [Writer.LoadState] and
// [Writer.LoadStateForWindow]; `window` is the caller's window, zero (or
// negative) for the pair-wide read.
func (w *Writer) loadState(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (State, bool, error) {
	label := windowLabel(window)
	key := cachekeys.Freeze(asset, quote)
	raw, err := w.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		if label != "" && w.windowLadder != nil {
			return w.loadDurableWindowLadder(ctx, asset, quote, window)
		}
		return w.loadDurableLadder(ctx, asset, quote)
	}
	if err != nil {
		return State{}, false, fmt.Errorf("freeze: cache get %s: %w", key, err)
	}
	var marker Marker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return State{}, true, nil //nolint:nilerr // deliberate: present-but-undecodable must not read as unfrozen
	}
	if label != "" && marker.Windowed {
		// Own live ladder, else the unowned one on its recording tick, else zero.
		if st, owned := marker.Ladders[label]; owned && LadderStillLive(st, w.ladderGrace, w.now()) {
			return st, true, nil
		}
		if w.unownedAdoptable(marker, w.now()) {
			return marker.UnownedLadder, true, nil
		}
		return State{}, true, nil
	}
	return marker.State, true, nil
}

// unownedAdoptable reports whether a window with no ladder of its own may
// take `marker`'s unowned one: it is still live, and it was recorded no
// more than one grace ago, i.e. this is the upgrade or recovery tick.
func (w *Writer) unownedAdoptable(marker Marker, now time.Time) bool {
	if !LadderStillLive(marker.UnownedLadder, w.ladderGrace, now) || marker.UnownedSince.IsZero() {
		return false
	}
	return !now.After(marker.UnownedSince.Add(w.ladderGrace))
}

// loadDurableLadder disambiguates a marker miss: (State{}, false, nil) with no store, no open ladder,
// a lapsed hold, or a store ERROR. Inventing a freeze from a Postgres blip would pin an unrelated price;
// the caller still re-freezes on the pair's own signal.
func (w *Writer) loadDurableLadder(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	if w.ladder == nil {
		return State{}, false, nil
	}
	st, ok, err := w.ladder.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		return State{}, false, nil //nolint:nilerr // documented above: degrade to the pre-0119 answer, never invent a freeze
	}
	if !LadderStillLive(st, w.ladderGrace, w.now()) {
		// Inactive, or lapsed while the aggregator was down: do not resurrect.
		return State{}, false, nil
	}
	obs.AnomalyFreezeLadderRehydratedTotal.Inc()
	return st, true, nil
}

// loadDurableWindowLadder answers for one window from a per-window store: its own live ladder (an
// escalated window comes back escalated); else a live unowned ladder; else (State{}, true) if a sibling
// is live, present but not inherited. Otherwise (State{}, false).
func (w *Writer) loadDurableWindowLadder(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
) (State, bool, error) {
	stored, unowned, ok, err := w.windowLadder.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		return State{}, false, nil //nolint:nilerr // as loadDurableLadder: degrade to the pre-0119 answer, never invent a freeze
	}
	now := w.now()
	if own, held := stored[window]; held && LadderStillLive(own, w.ladderGrace, now) {
		obs.AnomalyFreezeLadderRehydratedTotal.Inc()
		return own, true, nil
	}
	if LadderStillLive(unowned, w.ladderGrace, now) {
		obs.AnomalyFreezeLadderRehydratedTotal.Inc()
		return unowned, true, nil
	}
	for _, sibling := range stored {
		if LadderStillLive(sibling, w.ladderGrace, now) {
			return State{}, true, nil
		}
	}
	return State{}, false, nil
}

// LadderStillLive is the single definition of "Redis lost the marker" vs "the freeze is over" (active,
// inside hold plus grace). LoadState, [Recovery.tick] and operator tools must agree: a recovery stamp on
// a live ladder destroys the rehydrate's predicate. Clock-injected so tests pin the boundary.
func LadderStillLive(st State, grace time.Duration, now time.Time) bool {
	if !st.Active() {
		return false
	}
	return !now.After(st.HoldUntil.Add(grace))
}

// Clear deletes the marker, so a released freeze does not keep `flags.frozen` for its remaining hold,
// and retires the durable ladder: otherwise a restart before the recovery sweep re-freezes a cleared
// pair from the still-open row. Idempotent; the store write is best-effort.
func (w *Writer) Clear(ctx context.Context, asset, quote canonical.Asset) error {
	key := cachekeys.Freeze(asset, quote)
	if err := w.cache.Del(ctx, key.String()).Err(); err != nil {
		return fmt.Errorf("freeze: cache del %s: %w", key, err)
	}
	if w.ladder != nil {
		w.saveLadder(ctx, asset, quote, State{}, "clear")
	}
	return nil
}

// overrideRecord is the body of the [cachekeys.FreezeOverride] tombstone.
type overrideRecord struct {
	Actor  string    `json:"actor"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// RecordOverride writes the override tombstone freeze-unfreeze sets before [Writer.Clear], so the
// orchestrator can tell a human's force-unfreeze from a marker that merely expired.
func (w *Writer) RecordOverride(ctx context.Context, asset, quote canonical.Asset, actor, reason string) error {
	key := cachekeys.FreezeOverride(asset, quote)
	body, err := json.Marshal(overrideRecord{Actor: actor, Reason: reason, At: w.now().UTC()})
	if err != nil {
		return fmt.Errorf("freeze: marshal override: %w", err)
	}
	if err := w.cache.Set(ctx, key.String(), body, cachekeys.FreezeTTL).Err(); err != nil {
		return fmt.Errorf("freeze: cache set %s: %w", key, err)
	}
	return nil
}

// OverrideRecorded reports whether [Writer.RecordOverride]'s tombstone is
// live for (asset, quote).
func (w *Writer) OverrideRecorded(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	key := cachekeys.FreezeOverride(asset, quote)
	err := w.cache.Get(ctx, key.String()).Err()
	switch {
	case errors.Is(err, redis.Nil):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("freeze: cache get %s: %w", key, err)
	}
	return true, nil
}

// Looker reads freeze markers; it satisfies internal/api/v1.FrozenLooker structurally. Concurrent-safe.
type Looker struct {
	cache RedisCache
}

// NewLooker constructs a Looker around a RedisCache.
func NewLooker(cache RedisCache) (*Looker, error) {
	if cache == nil {
		return nil, errors.New("freeze: RedisCache is required")
	}
	return &Looker{cache: cache}, nil
}

// FrozenForPair reports whether (asset, quote) has a live freeze marker. On a Redis error the caller
// logs and serves frozen=false: better a price without the warning than a 5xx.
func (l *Looker) FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	key := cachekeys.Freeze(asset, quote)
	_, err := l.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("freeze: cache get %s: %w", key, err)
	}
	return true, nil
}
