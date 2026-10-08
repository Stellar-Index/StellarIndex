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

	// Windowed reports that [Marker.Ladders], not [Marker.State], is the per-window authority.
	// A separate flag so an EMPTY map (every ladder retired, marker kept for a frozen sibling)
	// is not mistaken for a marker written before per-window ladders.
	Windowed bool `json:"windowed,omitempty"`

	// Ladders is the ADR-0019 lifecycle per window, keyed by duration label ("5m0s"). The marker
	// is keyed per pair but the ladder advances per (pair, window): a single ladder let a cold window
	// adopt a sibling's escalated ladder and pin a healthy price, and would strand all but one on restart.
	Ladders map[string]State `json:"ladders,omitempty"`

	// UnownedLadder is a ladder live for the pair with no owning window (a pre-Ladders marker, or
	// a durable row from before 0163 / a non-window store); windows without an entry fall back to it so
	// narrowing never drops a running freeze. It retires once [LadderStillLive] fails and may be ADOPTED
	// only within one grace of [Marker.UnownedSince] (see [Writer.LoadStateForWindow]).
	UnownedLadder State `json:"unowned_ladder,omitempty"`

	// UnownedSince is when [Marker.UnownedLadder] was recorded; zero when there is none.
	UnownedSince time.Time `json:"unowned_since,omitempty"`

	// State is the pair-level lifecycle, so an operator dump shows the ladder and a restart does
	// not reset the 2-hour escalation clock. On a Windowed marker it only mirrors the latest window,
	// for pre-Ladders readers (rollback binaries, scripts).
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

// EventSink is the optional durable mirror behind /v1/anomalies (nil = Redis only); production
// wires `timescale.FreezeEventSink`.
type EventSink interface {
	// RecordFreeze persists a freeze event, a no-op while the pair has an open row. frozenValue
	// is the last-known-good VWAP as a fixed decimal string, empty on a first-tick freeze; any NULL/0
	// filler must never be read as a price. Must not block on network failure: Redis is the authority.
	RecordFreeze(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error
}

// LadderStore is the durable home for the lifecycle [State] (migration 0119). Redis may be
// flushed, and a MISSING marker under a live freeze reads as the operator override, so without it a
// flush would RELEASE every freeze, escalated ones included. Optional (nil = Redis only), concurrent-safe,
// best-effort: a store failure never blocks the Redis write.
type LadderStore interface {
	// SaveLadder writes the whole state on every transition, so it is idempotent and self-correcting.
	// It never creates a record; zero rows affected is an error (timescale.ErrNotFound), counted.
	SaveLadder(ctx context.Context, asset, quote canonical.Asset, state State) error

	// LoadLadder returns the durable lifecycle; ok=false means no open freeze, including one an
	// operator force-unfroze (freeze-unfreeze stamps recovered_at), which is how a flush is told apart.
	LoadLadder(ctx context.Context, asset, quote canonical.Asset) (State, bool, error)
}

// WindowLadderStore is the per-window [LadderStore] (migration 0163). A pair-keyed record is
// last-writer-wins, so a 5m window's fresh ladder could replace a 1h ESCALATED one in the only record
// that survives Redis. Implementations keep the pair-level view as a fail-closed summary (furthest
// hold, highest rung, escalated if any) for [Recovery] and freeze-unfreeze.
type WindowLadderStore interface {
	// SaveWindowLadder records `state` as `window`'s ladder on the pair's
	// open freeze record, leaving every other window's untouched. An
	// inactive `state` retires the window's ladder. Same "never creates a
	// record, zero rows is an error" contract as [LadderStore.SaveLadder].
	SaveWindowLadder(ctx context.Context, asset, quote canonical.Asset, window time.Duration, state State) error

	// LoadWindowLadders returns every window's ladder verbatim; `unowned` is a pre-0163 pair-level
	// ladder answering for windows without one. ok=false means what it does for LoadLadder.
	LoadWindowLadders(ctx context.Context, asset, quote canonical.Asset) (ladders map[time.Duration]State, unowned State, ok bool, err error)
}

// Writer writes `freeze:<asset>:<quote>` markers and, when wired, the durable mirror and ladder.
// Safe for concurrent use.
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

// NewWriter constructs a Writer; ttl=0 means cachekeys.FreezeTTL, used only by [Writer.Mark]
// ([Writer.MarkHold] takes its TTL from the lifecycle [Outcome]).
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

// WithEventSink wires the durable freeze-event mirror.
func WithEventSink(sink EventSink) WriterOption {
	return func(w *Writer) {
		w.sink = sink
	}
}

// DefaultLadderGrace equals [DefaultMarkerGrace] so losing Redis is behaviour-neutral: shorter
// drops a freeze flushed near its hold's end, longer lets a long-dead aggregator resurrect one.
var DefaultLadderGrace = DefaultMarkerGrace

// WithLadderStore wires the durable ladder: MarkHold mirrors every transition and LoadState
// falls back to it on a marker miss, so an escalated freeze survives a flush. grace <= 0 → default.
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

// Mark records a freeze with the flat TTL and no lifecycle, refreshing an existing marker. It is
// NOT the freeze-duration policy (re-mark-to-live releases on one bucket, the defect ADR-0019 fixes);
// it stays for the per-tick composite/triangulation refusal. frozenValue goes only to the EventSink.
func (w *Writer) Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	return w.MarkHold(ctx, asset, quote, frozenValue, decision, State{}, w.ttl)
}

// MarkHold is [Writer.Mark] plus the lifecycle: State in the marker, TTL from
// [Outcome.MarkerTTL]. ttl <= 0 falls back to the default so the marker still expires.
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

// MarkHoldForWindow records `state` as `window`'s ladder, MERGING with the marker so siblings
// survive, including windows this process never saw (under the volume floor since startup). An
// inactive state retires the ladder; window <= 0 is exactly [Writer.MarkHold].
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
		// Lifecycle-free Mark on a marker a live ladder owns: keep the owner's ladders and diagnostics
		// (rewriting them zeroed a legacy ladder and relabelled an escalated freeze); only expiry may move out.
		marker = *prior
	}
	// One key, several owners: never pull the expiry in under someone else's live hold, else Mark's
	// flat 5 min or a 5m window's remainder lapses a longer sibling's freeze.
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

	// Durable mirror, best-effort: Redis already succeeded and drives flags.frozen.
	if w.sink != nil {
		if sinkErr := w.sink.RecordFreeze(ctx, asset, quote, frozenValue, decision); sinkErr != nil {
			// Silent on purpose (no logger here); a broken mirror shows as the gap between
			// AnomalyFreezeEngagedTotal and freeze_events rows. The ladder half below is counted.
			_ = sinkErr
		}
	}

	// Durable ladder on EVERY transition (extensions/escalation change later than the fire), after
	// RecordFreeze because SaveLadder never creates the row, and only for ACTIVE states (Mark's zero State
	// would read as "fired just now"). Window-scoped when the store can, since pair-level is last-writer-wins.
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

// saveLadder mirrors a state to the durable store: best-effort but counted, since nothing here
// logs and a missing ladder is found only when a flush needs it (e.g. binary ahead of migration 0119).
// `op` (mark_hold | clear) tells recording failures from retiring ones.
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
