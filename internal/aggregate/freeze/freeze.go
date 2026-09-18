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
	// Authoritative when [Marker.Windowed]; a window with no entry owns
	// no ladder.
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
	// itself once its own hold plus the grace has passed, and every
	// window that is genuinely still frozen has claimed an owned entry
	// long before then (a live freeze re-marks every tick).
	UnownedLadder State `json:"unowned_ladder,omitempty"`

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
// Per docs/architecture/explorer-implementation-plan.md
// Phase 2: this is what migrates the Redis-only freeze state into
// a queryable postgres timeline that powers /v1/anomalies.
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
	// stamp NULL or 0 in that case.
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
// Why this exists at all. Before 0119 the ladder lived in exactly two
// volatile places: the aggregator's in-process state map, and a JSON blob
// inside the Redis marker. Redis is a cache — deployed without persistence,
// flushed during incidents — and the orchestrator reads a MISSING marker
// under a live freeze as the ADR-0019 operator override, because until now
// that was the only way it could happen. So a Redis flush did not merely
// forget how far a pair had climbed the ladder: the next tick RELEASED
// every live freeze, and a pair that had spent the whole 2-hour ladder to
// ESCALATED — which ADR-0019 holds "until manual unfreeze" — silently
// unfroze and republished the price a P1 had already escalated to a human.
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
// forgets to plumb the outcome's TTL degrades to the old behaviour
// rather than writing a marker that never expires.
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
	now := time.Now()
	windowed, ladders, unowned, prior := w.mergeLadders(ctx, asset, quote, label, state)
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

// RetireWindowLadder drops ONE window's ladder from the shared marker
// while leaving the marker — and therefore `flags.frozen` — in place.
//
// This is the release half of [Writer.MarkHoldForWindow], for the case
// the orchestrator cannot express any other way: a window auto-releases
// while a SIBLING window of the same pair is still frozen, so the marker
// must stay (its presence is the pair-wide flag the API serves) but the
// releasing window's ladder must not. Without it the released ladder
// would sit in the marker until the next writer happened to overwrite
// it, and a restart in between would rehydrate a freeze that had
// already ended.
//
// The marker's TTL is preserved (Redis SET ... KEEPTTL): the remaining
// hold belongs to the sibling that is still frozen, and this call is not
// entitled to extend or truncate it.
//
// No-ops when the marker is absent (nothing to retire) or was written by
// a build with no per-window ladders (nothing that can be scoped — the
// sibling's next lifecycle write upgrades the marker in place).
// Idempotent, and best-effort in the same sense as the rest of the
// durable side: the caller logs, the freeze itself is unaffected.
func (w *Writer) RetireWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration) error {
	label := windowLabel(window)
	if label == "" {
		return nil
	}
	// The durable entry goes first, and regardless of what the marker
	// holds: it is the record a Redis loss falls back to, and a released
	// window left in it is rehydrated as a freeze that had already ended.
	// Retiring it also recomputes the pair-level summary from the windows
	// that are still frozen. Best-effort and counted, like every durable
	// write (op "clear" — this IS a retire).
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
	if err := w.cache.Set(ctx, key.String(), body, redis.KeepTTL).Err(); err != nil {
		return fmt.Errorf("freeze: cache set %s: %w", key, err)
	}
	return nil
}

// ReleaseWindow ends `window`'s freeze on the serving path for a caller
// that believes it is the pair's LAST frozen window, and reports whether
// the marker was nevertheless kept.
//
// The belief is the problem. The orchestrator decides "last" from its
// in-memory ladder map, and a window only enters that map by reaching the
// freeze step in the current process. A window sitting under the
// USD-volume floor never does, and after a restart no window has yet —
// so such a window's freeze exists ONLY in the marker (and the durable
// record behind it), where the in-memory check cannot see it. Clearing on
// that check deleted the marker and retired the durable ladder out from
// under it: an ESCALATED freeze, which ADR-0019 holds "until manual
// unfreeze", ended because a sibling window recovered, and the window's
// next qualifying bucket — cold, so with no prev-VWAP comparator to
// re-fire on — published.
//
// So the record is asked, not only the process. If any OTHER window still
// owns a ladder in the marker, or the marker still carries a live unowned
// one, this is [Writer.RetireWindowLadder]: the window's own ladder goes,
// the marker and `flags.frozen` stay. Otherwise it is [Writer.Clear].
//
// An owned sibling ladder counts while it is merely ACTIVE, not only while
// [LadderStillLive]: that is the test the cold-key rehydrate applies to an
// owned entry, and a release that used a narrower one would delete a
// ladder the very next read would have honoured. The marker's own TTL is
// what bounds a sibling nobody is advancing.
//
// A marker that cannot be read is NOT cleared — the error is returned and
// the marker is left to its TTL. Not knowing whether a sibling is frozen
// is no ground for unfreezing it. An absent, undecodable or pre-window
// marker has no sibling to protect and is cleared exactly as before, which
// keeps the operator-override path (marker already deleted) idempotent.
func (w *Writer) ReleaseWindow(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (bool, error) {
	label := windowLabel(window)
	marker, ok, err := w.readMarker(ctx, cachekeys.Freeze(asset, quote))
	if err != nil {
		return true, err
	}
	if ok && marker.Windowed && label != "" && w.siblingLadderHeld(marker, label) {
		return true, w.RetireWindowLadder(ctx, asset, quote, window)
	}
	return false, w.Clear(ctx, asset, quote)
}

// siblingLadderHeld reports whether `marker` records a freeze for any
// window other than `own`: an owned ladder that is still active, or an
// unowned one that is still live (owner unknown, so it may be a sibling's).
func (w *Writer) siblingLadderHeld(marker Marker, own string) bool {
	for label, st := range marker.Ladders {
		if label != own && st.Active() {
			return true
		}
	}
	return LadderStillLive(marker.UnownedLadder, w.ladderGrace, time.Now())
}

// mergeLadders computes the per-window ladder map for the marker about
// to be written at `key`: the ladders already stored there, with
// `label`'s entry set to `state` (or removed, when `state` is no longer
// active). Returns whether the resulting marker records per-window
// ladders at all.
//
// Merging rather than rewriting is the load-bearing part. The caller
// only ever knows ONE window's ladder, and the windows of a pair freeze
// and release independently — a window can even be frozen while never
// reaching the freeze step in this process, because the aggregator drops
// a window under its USD-volume floor first. Anything this write does
// not know about must therefore survive it untouched.
//
// An unreadable or absent marker yields a fresh map: the pair has no
// freeze on the serving path, so there is no ladder to preserve. A
// legacy marker (no per-window ladders) is upgraded in place — its
// pair-level [Marker.State] stays readable in the field it was written
// to, and the window being written becomes the first recorded owner.
//
// `label` empty is the unscoped [Writer.Mark] / [Writer.MarkHold] path:
// it owns no window's ladder, so it preserves what is there and claims
// nothing.
func (w *Writer) mergeLadders(
	ctx context.Context,
	asset, quote canonical.Asset,
	label string,
	state State,
) (bool, map[string]State, State, *Marker) {
	key := cachekeys.Freeze(asset, quote)
	marker, ok, err := w.readMarker(ctx, key)
	ladders := map[string]State{}
	windowed := false
	var unowned State
	// prior is the marker this write replaces, for the one caller that
	// may have to leave it as it is ([Writer.markHold]'s Mark branch).
	var prior *Marker
	if err == nil && ok {
		prior = &marker
	}
	switch {
	case err != nil || !ok:
		// No marker to merge with. Either this is the pair's first
		// freeze, or Redis lost the marker while the freeze ran — and
		// those differ only in the durable record, so ask it. Every
		// window's still-live durable ladder is restored under its own
		// label: from this write on the MARKER answers the pair's cold
		// windows, so a marker rebuilt from one window's view would lose
		// a sibling's escalation one tick after the durable read saved it.
		ladders, unowned = w.liveDurableLadders(ctx, asset, quote)
		windowed = len(ladders) > 0
	case marker.Windowed:
		windowed = true
		for k, v := range marker.Ladders {
			ladders[k] = v
		}
		unowned = marker.UnownedLadder
	default:
		// Upgrading a marker written before per-window ladders existed:
		// its pair-level ladder has no owner, so it stays readable by
		// every window until each has claimed its own.
		unowned = marker.State
	}
	if !LadderStillLive(unowned, w.ladderGrace, time.Now()) {
		// Nobody is advancing an unowned snapshot; once its own hold
		// plus the grace has passed it describes no running freeze.
		unowned = State{}
	}
	if label == "" {
		// An unscoped write claims no window, but it must not HIDE the
		// ladders it found. When the marker was absent and the durable
		// record supplied them (Redis lost the marker and the
		// lifecycle-free [Writer.Mark] is the first writer back), the
		// marker written here is what every cold window reads from now on
		// — the durable record is only consulted while the marker is
		// missing. It therefore has to be one whose ladders are honoured,
		// i.e. a windowed marker, or a present-but-empty marker reads as
		// "frozen pair-wide, this window never was" and the window
		// publishes the bucket an escalated freeze was withholding.
		return windowed || len(ladders) > 0 || unowned.Active(), ladders, unowned, prior
	}
	if state.Active() {
		ladders[label] = state
	} else {
		delete(ladders, label)
	}
	return true, ladders, unowned, prior
}

// lifecycleTTLFloor is the shortest TTL the marker may be written with
// without lapsing under a hold one of its ladders is still serving: the
// longest `remaining hold + grace` over every still-live ladder it
// carries. Zero when it carries none — which doubles as "does a live
// lifecycle own this marker".
//
// `own` is the label of the window doing the writing, skipped because
// that window's caller passes its own lifecycle TTL (the policy's
// MarkerGrace, which an operator may have tuned) and is the authority for
// it. Empty for a writer that owns no window.
//
// A marker written before per-window ladders existed keeps its one ladder
// in the pair-level State, so that counts too — but only there: on a
// windowed marker State is a mirror of the last writer, not an authority.
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

// liveDurableLadder returns the migration-0119 ladder for the marker's
// pair when one is still running, and the zero State otherwise (no
// store wired, no open row, a lapsed hold, or a store error).
//
// Used only when the marker is ABSENT at write time, which — given the
// write that follows is a live freeze's — is the "Redis lost the
// marker" case [Writer.LoadState] documents. Best-effort by the same
// reasoning as every other durable read: degrade to the pre-0119
// answer, never invent a freeze.
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

// liveDurableLadders is [Writer.liveDurableLadder] for a store that
// records a ladder per window (migration 0163): the still-live durable
// ladders keyed by marker label, plus the record's unowned ladder.
//
// A store with no window dimension yields no owned ladders and its single
// pair-level ladder as the unowned one — the pre-0163 answer, unchanged.
// Lapsed entries are dropped here rather than copied into the marker: a
// window whose own hold plus the grace has passed describes no running
// freeze, which is the same [LadderStillLive] bound every other durable
// read applies.
func (w *Writer) liveDurableLadders(ctx context.Context, asset, quote canonical.Asset) (map[string]State, State) {
	ladders := map[string]State{}
	if w.windowLadder == nil {
		return ladders, w.liveDurableLadder(ctx, asset, quote)
	}
	stored, unowned, ok, err := w.windowLadder.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		return ladders, State{}
	}
	now := time.Now()
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

// LoadState reads the ADR-0019 lifecycle state a previous
// [Writer.MarkHold] stamped into the marker for (asset, quote), falling
// back to the durable ladder (migration 0119) when the marker is gone.
//
// Returns (State{}, false, nil) only when the pair has NO live freeze by
// either authority — never frozen, already recovered, or force-unfrozen by
// an operator. The caller reads that as "not frozen", and under a live
// in-memory freeze as the ADR-0019 §"Freeze duration" operator override.
//
// # Why the marker alone is not the authority
//
// This used to return (State{}, false, nil) on any missing marker, with the
// documented reasoning that "a missing marker under a live freeze is a
// deliberate signal, not a lost write". Redis falsifies that: it is a cache,
// deployed without persistence and flushed during incidents. A flush
// therefore did not merely forget the ladder — it read as an operator
// override, so the next tick RELEASED every live freeze, and a pair that had
// climbed the whole 2-hour ladder to ESCALATED ("stays active until manual
// unfreeze") silently republished the price a P1 alert had already put in
// front of a human.
//
// So a missing marker is now disambiguated against the durable record:
//
//   - open freeze_events row + hold not lapsed  → Redis lost the marker.
//     Return the stored ladder as PRESENT; the freeze and its escalation
//     survive. This is the fix.
//   - no open row                               → the freeze genuinely
//     ended. `stellarindex-ops freeze-unfreeze` clears the marker AND
//     stamps recovered_at, so the supported override still reads as
//     absent and still sticks.
//   - open row but hold lapsed beyond the grace → the aggregator has been
//     down longer than the freeze's own hold. Do NOT resurrect it; behave
//     exactly as before 0119. The bound is what keeps a week-old
//     never-closed row from re-freezing a healthy pair on restart.
//
// A raw `redis-cli DEL` is no longer an override, by design: it never was a
// supported one (untyped, unlogged, un-mirrored — see the header of
// cmd/stellarindex-ops/freeze_unfreeze.go, which exists to replace it), and
// treating it as one is precisely what made a Redis flush indistinguishable
// from an operator decision.
//
// A marker that is PRESENT but does not decode is reported as
// (State{}, true, nil) — present, lifecycle unknown — not as absent.
// Present-with-zero-state keeps the freeze and merely forgets where it was
// on the ladder, which is also exactly how a marker written by a
// pre-lifecycle build reads.
//
// With no ladder store wired, behaviour is bit-for-bit the pre-0119 one.
func (w *Writer) LoadState(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	return w.loadState(ctx, asset, quote, 0)
}

// LoadStateForWindow is [Writer.LoadState] answering for ONE aggregation
// window of the pair — which is what every ADR-0019 lifecycle caller
// actually needs, because the ladder advances per (pair, window) while
// this marker is keyed per (asset, quote).
//
// PRESENCE is unchanged and stays pair-scoped: the marker is what the
// API serves as `flags.frozen` for the whole (asset, quote), and its
// absence under a live freeze is the ADR-0019 operator override for
// EVERY window — so a marker only a sibling window's freeze is keeping
// alive still reports present=true here, and deleting it still releases
// every window.
//
// The LADDER is what gets scoped, to [Marker.Ladders]`[window]`. A
// window with no entry owns no ladder and returns the zero [State]: it
// is not mid-freeze, and if its own bucket is anomalous it fires its own
// ladder from the bottom. Adopting another window's instead is the
// defect this method exists to remove — a window that had been sitting
// under the aggregator's USD-volume floor (so it never entered the
// in-memory ladder map) inherited a sibling's FiredAt, HoldUntil,
// ExtensionsUsed and Escalated on its first qualifying bucket, pinning a
// last-known-good price on a window nothing was wrong with, with a
// manual unfreeze the only exit once the inherited ladder escalated.
//
// Two shapes carry no per-window ladders and both answer pair-wide, on
// purpose, because the alternative is silently DROPPING a freeze that is
// still running:
//
//   - a marker written before [Marker.Ladders] existed. It is replaced
//     by the owning window's next lifecycle write, so it survives at
//     most from an upgrade until the freeze's next tick.
//   - a durable ladder with no recorded owner, read only when the marker
//     is gone (the [Writer.LoadState] contract above): a `freeze_events`
//     row written before migration 0163, or a [LadderStore] that is not
//     a [WindowLadderStore]. A Redis flush leaves it as the one surviving
//     record that this pair is inside an unreleased freeze.
//
// A durable record that DOES carry the window (migration 0163) is scoped
// exactly like the marker — see [Writer.loadDurableWindowLadder].
//
// Both therefore hold for every window of the pair until a window-scoped
// record replaces them: over-freezing a window is a degraded price that
// is already flagged frozen pair-wide and releases itself on the ADR's
// auto-unfreeze, whereas under-freezing publishes the manipulated print
// the freeze exists to withhold.
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
		// Present (the pair is frozen), and this window's ladder is
		// whatever the marker records for it. With no entry of its own
		// it falls back to a still-live ladder the pair holds with no
		// recorded owner, and to the zero State when there is none.
		// See the doc comment above.
		if st, owned := marker.Ladders[label]; owned {
			return st, true, nil
		}
		if LadderStillLive(marker.UnownedLadder, w.ladderGrace, time.Now()) {
			return marker.UnownedLadder, true, nil
		}
		return State{}, true, nil
	}
	return marker.State, true, nil
}

// loadDurableLadder is [Writer.LoadState]'s marker-miss branch: consult the
// durable ladder to tell "Redis lost the marker" from "this freeze ended".
//
// Returns (State{}, false, nil) — the pre-0119 answer — when no store is
// wired, when the store has no open ladder for the pair, or when the stored
// hold lapsed more than the grace ago.
//
// A store ERROR is also reported as absent, and that is the deliberate
// choice rather than an oversight. Propagating it would be read by the
// orchestrator's cold-key path as a transient failure (fine) but the whole
// point of this branch is the LIVE-freeze path, where any answer other than
// "present" ends the freeze — and inventing a freeze out of a Postgres blip
// on a pair whose marker is already gone would pin a price to a
// last-known-good value with no evidence that anything is wrong with it.
// The pre-0119 behaviour is the safe floor to degrade to, and the caller
// still re-freezes on the pair's own signal if the anomaly is live.
func (w *Writer) loadDurableLadder(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	if w.ladder == nil {
		return State{}, false, nil
	}
	st, ok, err := w.ladder.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		return State{}, false, nil //nolint:nilerr // documented above: degrade to the pre-0119 answer, never invent a freeze
	}
	if !LadderStillLive(st, w.ladderGrace, time.Now()) {
		// Inactive, or the hold lapsed while nobody was refreshing it (the
		// aggregator was down longer than this freeze's own hold). Same
		// answer as before 0119 — do not resurrect it.
		return State{}, false, nil
	}
	obs.AnomalyFreezeLadderRehydratedTotal.Inc()
	return st, true, nil
}

// loadDurableWindowLadder is [Writer.loadDurableLadder] answering for ONE
// window, from a store that records a ladder per window (migration 0163).
//
// The marker is gone, so the durable record is the only authority left,
// and it is read with the same three-way split the marker uses:
//
//   - `window` has a still-live durable ladder of its own → that ladder.
//     An escalated window comes back escalated whatever its siblings have
//     written since, which is the fix.
//   - it has none, but the record holds a still-live UNOWNED ladder (a row
//     written before 0163 — owner unknowable) → that ladder, for every
//     window, because dropping it releases a freeze that is still running.
//   - it has none, and a SIBLING window's ladder is still live → the pair
//     is frozen and this window is not: (State{}, true). Present, so a
//     live in-memory freeze does not read a Redis loss as the operator
//     override; zero, so a cold window does not inherit a freeze.
//
// Anything else — no store answer, no open record, every hold lapsed — is
// the pre-0119 (State{}, false), on [Writer.loadDurableLadder]'s grounds.
func (w *Writer) loadDurableWindowLadder(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
) (State, bool, error) {
	stored, unowned, ok, err := w.windowLadder.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		return State{}, false, nil //nolint:nilerr // as loadDurableLadder: degrade to the pre-0119 answer, never invent a freeze
	}
	now := time.Now()
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

// LadderStillLive reports whether a durable ladder describes a freeze that
// is STILL RUNNING — active, and inside its hold plus the marker grace.
//
// This is the single definition of "Redis merely lost the marker" as opposed
// to "this freeze is over", and it has THREE consumers that must not drift:
// [Writer.LoadState]'s marker-miss branch (rehydrate or not),
// [Recovery.tick] (close the durable row or leave it for the rehydrate), and
// any operator tool that renders a ladder. Two of those disagreeing is not a
// cosmetic bug — the recovery worker stamping recovered_at is precisely what
// destroys the predicate the rehydrate depends on, so a divergence here
// re-opens the whole finding with the row now marked "recovered normally".
//
// Exported (and clock-injected) so all three read the same function and a
// test can pin the boundary without sleeping.
func LadderStillLive(st State, grace time.Duration, now time.Time) bool {
	if !st.Active() {
		return false
	}
	return !now.After(st.HoldUntil.Add(grace))
}

// Clear deletes the freeze marker for (asset, quote), ending the
// freeze on the serving path immediately.
//
// Called on auto-unfreeze and on operator override. Deleting rather
// than letting the TTL lapse matters now that the TTL encodes the
// remaining HOLD: a released freeze whose marker still had 25 minutes
// of hold on it would keep `flags.frozen` true for 25 minutes after
// the price was republished as healthy.
//
// Idempotent — deleting an absent key is not an error.
//
// When a ladder store is wired, Clear also RETIRES the durable ladder
// (migration 0119) by writing back a zero [State], which nulls hold_until
// and so makes [LadderStore.LoadLadder] report no ladder for the pair.
//
// That is not tidiness — it closes a real window. The durable row itself is
// closed asynchronously by the recovery worker (60s poll), so between an
// auto-release and that sweep the row is still OPEN with a live hold. An
// aggregator restarting inside that window would hit LoadState on a cold key,
// find no marker, rehydrate the ladder from the still-open row and re-freeze a
// pair whose anomaly had already cleared. Retiring the ladder here makes the
// release visible to the durable authority at the same instant it becomes
// visible on the serving path.
//
// Best-effort, like every other write to the durable side: the marker DEL
// above is the load-bearing operation, and a store failure only reopens the
// pre-existing window rather than breaking the release.
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

// Looker reads the freeze marker for a pair. Implements the
// behaviour of internal/api/v1.FrozenLooker (the API package
// declares its own interface to avoid the import cycle; Looker
// satisfies it structurally).
//
// Safe for concurrent FrozenForPair calls.
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

// FrozenForPair reports whether (asset, quote) currently has a
// freeze marker in cache. Returns:
//
//   - (true, nil)  — marker present (TTL still alive)
//   - (false, nil) — no marker (clean state OR TTL elapsed; the
//     API can't distinguish the two and shouldn't need to)
//   - (false, err) — Redis read failed; caller (API handler) logs
//   - falls through with frozen=false. Better to publish a price
//     without the warning than 5xx because of a Redis blip.
//
// Implements the contract of [internal/api/v1.FrozenLooker].
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
