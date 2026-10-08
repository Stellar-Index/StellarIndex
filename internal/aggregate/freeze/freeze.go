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
	// XX: a marker cleared since the read (freeze-unfreeze, or its TTL
	// lapsing) stays cleared. KEEPTTL on an absent key would recreate it
	// with no expiry.
	err = w.cache.SetArgs(ctx, key.String(), body, redis.SetArgs{Mode: "XX", KeepTTL: true}).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("freeze: cache set %s: %w", key, err)
	}
	return nil
}

// ReleaseWindow ends `window`'s freeze on the serving path for a caller
// that believes it is the pair's LAST frozen window, and reports whether
// the marker was nevertheless kept.
//
// The caller's belief comes from its in-memory ladder map, which misses any
// window under the USD-volume floor and every window after a restart.
// Trusting it let a sibling's recovery end an ESCALATED freeze, which
// ADR-0019 holds until manual unfreeze, and the cold window then published.
// So the record is asked: if another window owns a ladder in the marker
// (merely ACTIVE counts, matching the cold-key rehydrate), or the marker
// carries a live unowned one, this is [Writer.RetireWindowLadder] and the
// marker and `flags.frozen` stay. Otherwise it is [Writer.Clear].
//
// A marker naming no sibling (absent, undecodable, old-format or rebuilt
// during a durable-read failure) is not evidence that none is frozen, and
// [Writer.Clear] retires every window's durable ladder, so the durable
// record is asked too ([Writer.durableSiblingHeld]). Skipping that let a
// 5m window's release during a Redis loss retire an escalated 1h sibling.
//
// An unreadable record is NOT cleared: the error is returned and the holds
// run out on their own. Not knowing whether a sibling is frozen is no
// ground for unfreezing it. `freeze-unfreeze` calls [Writer.Clear]
// directly, so by the time a release lands here it is an idempotent clear.
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

// durableSiblingHeld reports whether the DURABLE record holds a freeze for
// any window other than `own`: a sibling's still-live ladder, or a
// still-live unowned one (a row written before 0163, or a pair-level
// ladder a rolled-back binary advanced — owner unknown, so it may be a
// sibling's).
//
// [LadderStillLive] is the bound, as for every other durable read: it is
// the test the durable rehydrate applies, so a ladder this keeps is exactly
// one the next cold read would have honoured.
//
// False, with no read, for a store that cannot scope a ladder to a window.
// Its single pair-level ladder is last-writer-wins across the pair's
// windows, so it cannot say whose it is, and [Writer.RetireWindowLadder]
// has nothing to retire in it: holding it back would mean the release
// never reached the durable side at all. That store keeps the pair-wide
// clear it has always had. Production wires the window-aware sink.
//
// Unlike the rehydrate reads, a store ERROR is returned rather than
// degraded to "absent". There, "absent" is the answer that invents
// nothing. Here it is the answer that destroys the record.
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
		// No marker to merge with. Either this is the pair's first
		// freeze, or Redis lost the marker while the freeze ran — and
		// those differ only in the durable record, so ask it. Every
		// window's still-live durable ladder is restored under its own
		// label: from this write on the MARKER answers the pair's cold
		// windows, so a marker rebuilt from one window's view would lose
		// a sibling's escalation one tick after the durable read saved it.
		ladders, unowned = w.liveDurableLadders(ctx, asset, quote)
		windowed = len(ladders) > 0
		unownedSince = now
	case marker.Windowed:
		windowed = true
		for k, v := range marker.Ladders {
			// A window that stopped reaching the freeze step (dropped
			// under the USD-volume floor) never retires its own ladder;
			// past its hold plus the grace nobody is advancing it.
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
		return windowed || len(ladders) > 0 || unowned.Active(), ladders, unowned, unownedSince, prior
	}
	if state.Active() {
		ladders[label] = state
	} else {
		delete(ladders, label)
	}
	return true, ladders, unowned, unownedSince, prior
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

// LoadState reads the ADR-0019 lifecycle state a previous
// [Writer.MarkHold] stamped into the marker for (asset, quote), falling
// back to the durable ladder (migration 0119) when the marker is gone.
//
// Returns (State{}, false, nil) only when the pair has NO live freeze by
// either authority. The caller reads that as "not frozen", and under a live
// in-memory freeze as the ADR-0019 operator override.
//
// The marker alone cannot be the authority: Redis runs without persistence
// and is flushed in incidents, so a flush would read as an override and
// release every live freeze, ESCALATED ones included. A missing marker is
// disambiguated against freeze_events:
//
//   - open row, hold not lapsed: Redis lost the marker; return the stored
//     ladder as PRESENT.
//   - no open row: the freeze ended. freeze-unfreeze clears the marker AND
//     stamps recovered_at, so the supported override still sticks.
//   - open row lapsed beyond the grace: the aggregator was down longer than
//     the hold; do NOT resurrect it, or a week-old unclosed row re-freezes a
//     healthy pair on restart.
//
// A raw `redis-cli DEL` is therefore not an override; it never was a
// supported one (see internal/ops/accounts/freeze_unfreeze.go).
//
// A PRESENT marker that does not decode is (State{}, true, nil): the
// freeze is kept and only its ladder position is forgotten. With no ladder
// store wired, only the marker is consulted.
func (w *Writer) LoadState(ctx context.Context, asset, quote canonical.Asset) (State, bool, error) {
	return w.loadState(ctx, asset, quote, 0)
}

// LoadStateForWindow is [Writer.LoadState] for ONE aggregation window of
// the pair: the ladder advances per (pair, window) while the marker is
// keyed per (asset, quote).
//
// PRESENCE stays pair-scoped: the marker is the API's `flags.frozen` for
// the whole pair, and its absence is the operator override for EVERY
// window.
//
// The LADDER is scoped to [Marker.Ladders]`[window]`. A window with no entry
// gets the zero [State] and fires its own ladder if anomalous. Adopting a
// sibling's ladder pinned last-known-good on a healthy window (one that
// had sat under the USD-volume floor), with only a manual unfreeze as the
// exit once the inherited ladder escalated.
//
// Two shapes carry no per-window ladder and answer pair-wide, because the
// alternative silently DROPS a running freeze:
//
//   - a pre-[Marker.Ladders] marker. The first lifecycle write keeps its
//     ladder as [Marker.UnownedLadder], which a window without an entry
//     adopts only within one ladder grace of [Marker.UnownedSince].
//   - a durable ladder with no recorded owner, read only when the marker is
//     gone: the one surviving record of an unreleased freeze after a flush.
//
// A durable record that carries the window (migration 0163) is scoped like
// the marker ([Writer.loadDurableWindowLadder]). The adoption bound matters
// because Escalated never auto-releases; within it, over-freezing is
// preferred to publishing the manipulated print the freeze withholds.
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
		// it falls back to the unowned ladder only on the tick that
		// recorded it, and otherwise to the zero State. See the doc
		// comment above.
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
	if !LadderStillLive(st, w.ladderGrace, w.now()) {
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

// overrideRecord is the body of the [cachekeys.FreezeOverride] tombstone.
type overrideRecord struct {
	Actor  string    `json:"actor"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// RecordOverride writes the operator-override tombstone for (asset, quote).
// `stellarindex-ops freeze-unfreeze` calls it before [Writer.Clear]: the
// orchestrator sees only a missing marker, and without the tombstone it
// cannot tell a human's force-unfreeze from a marker and ladder that
// expired with nobody refreshing them.
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
