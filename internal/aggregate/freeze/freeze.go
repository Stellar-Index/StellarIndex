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

	// State is the ADR-0019 freeze-lifecycle state (fired_at,
	// hold_until, extensions_used, escalated, unfreeze_streak). Zero
	// for markers written by the pre-lifecycle [Writer.Mark] path.
	//
	// Carried in the marker for two reasons: an operator dumping
	// `freeze:*` can see how far up the extension ladder a pair is
	// without reading logs, and the aggregator re-hydrates the ladder
	// from here after a restart instead of silently starting the
	// 2-hour escalation clock over.
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
	if ttl <= 0 {
		ttl = w.ttl
	}
	marker := Marker{
		AssetID:      asset.String(),
		QuoteID:      quote.String(),
		Action:       decision.Action,
		Class:        decision.Class,
		DeviationPct: decision.DeviationPct,
		Reason:       decision.Reason,
		FrozenAt:     time.Now().UTC(),
		State:        state,
	}
	body, err := json.Marshal(marker)
	if err != nil {
		// Unreachable — Marker has no func/chan fields. Wrap for
		// diagnostic completeness.
		return fmt.Errorf("freeze: marshal marker: %w", err)
	}
	key := cachekeys.Freeze(asset, quote)
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
	if w.ladder != nil && state.Active() {
		w.saveLadder(ctx, asset, quote, state, "mark_hold")
	}
	return nil
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
	key := cachekeys.Freeze(asset, quote)
	raw, err := w.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return w.loadDurableLadder(ctx, asset, quote)
	}
	if err != nil {
		return State{}, false, fmt.Errorf("freeze: cache get %s: %w", key, err)
	}
	var marker Marker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return State{}, true, nil //nolint:nilerr // deliberate: present-but-undecodable must not read as unfrozen
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
