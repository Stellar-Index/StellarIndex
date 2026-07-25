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
	cache RedisCache
	ttl   time.Duration
	sink  EventSink
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
			// Caller logs at DEBUG; we don't want to spam WARN on
			// every transient postgres blip. The sink is expected
			// to log its own failures with full context.
			_ = sinkErr
		}
	}
	return nil
}

// LoadState reads the ADR-0019 lifecycle state a previous
// [Writer.MarkHold] stamped into the marker for (asset, quote).
//
// Returns (State{}, false, nil) when no marker exists — which the
// caller must read as BOTH "never frozen" and "the operator force-
// unfroze this pair out of band". Deleting the marker is the
// operator override ADR-0019 §"Freeze duration" requires, so a
// missing marker under a live freeze is a deliberate signal, not a
// lost write; the orchestrator drops its in-memory ladder when it
// sees one.
//
// A marker that is PRESENT but does not decode is reported as
// (State{}, true, nil) — present, lifecycle unknown — not as absent.
// The distinction is load-bearing: "absent" is the operator override,
// so reporting a corrupt marker as absent would let a stray byte in
// diagnostic JSON silently unfreeze a pair. Present-with-zero-state
// keeps the freeze and merely forgets where it was on the ladder,
// which is also exactly how a marker written by a pre-lifecycle build
// reads.
func (w *Writer) LoadState(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	key := cachekeys.Freeze(asset, quote)
	raw, err := w.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return State{}, false, nil
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
func (w *Writer) Clear(ctx context.Context, asset, quote canonical.Asset) error {
	key := cachekeys.Freeze(asset, quote)
	if err := w.cache.Del(ctx, key.String()).Err(); err != nil {
		return fmt.Errorf("freeze: cache del %s: %w", key, err)
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
