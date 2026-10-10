package freeze_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ─── the durable-ladder seam (migration 0119) ────────────────────

// fakeLadderStore is an in-memory freeze.LadderStore. It stores the last
// saved state per pair and lets a test CLOSE the durable row (what
// `stellarindex-ops freeze-unfreeze` does when it stamps recovered_at) or
// fail the read.
type fakeLadderStore struct {
	states map[string]freeze.State
	closed map[string]bool
	saves  int
	err    error
}

func newFakeLadderStore() *fakeLadderStore {
	return &fakeLadderStore{
		states: map[string]freeze.State{},
		closed: map[string]bool{},
	}
}

func (f *fakeLadderStore) key(asset, quote canonical.Asset) string {
	return asset.String() + "|" + quote.String()
}

func (f *fakeLadderStore) SaveLadder(_ context.Context, asset, quote canonical.Asset, st freeze.State) error {
	f.saves++
	if f.err != nil {
		return f.err
	}
	// Mirrors the SQL: an UPDATE of the OPEN row only. A closed row is
	// not re-opened by a save.
	if f.closed[f.key(asset, quote)] {
		return nil
	}
	f.states[f.key(asset, quote)] = st
	return nil
}

func (f *fakeLadderStore) LoadLadder(_ context.Context, asset, quote canonical.Asset) (freeze.State, bool, error) {
	if f.err != nil {
		return freeze.State{}, false, f.err
	}
	k := f.key(asset, quote)
	if f.closed[k] {
		return freeze.State{}, false, nil
	}
	st, ok := f.states[k]
	// Mirrors the SQL's `hold_until IS NOT NULL` filter: a retired ladder
	// (Clear writes back a zero State) reads as "no durable ladder".
	if ok && st.HoldUntil.IsZero() {
		return freeze.State{}, false, nil
	}
	return st, ok, nil
}

// close simulates the operator override's durable half: `recovered_at` is
// stamped, so the row is no longer OPEN.
func (f *fakeLadderStore) close(asset, quote canonical.Asset) {
	f.closed[f.key(asset, quote)] = true
}

func escalatedState(now time.Time) freeze.State {
	return freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
}

func freezeDecision() anomaly.Decision {
	return anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 14.2,
		Reason:       "phase2:3_signal_AND confidence=0.121 z=8.44 sources=1",
	}
}

// TestWriter_LadderSurvivesRedisFlush is THE regression for the durable
// freeze ladder (migration 0119).
//
// Scenario, exactly as it happens in production:
//
//  1. A pair has climbed ADR-0019's whole 4 × 30-minute extension ladder
//     without earning its auto-unfreeze. It is ESCALATED — a P1 has paged a
//     human, and the ADR says the freeze "stays active until manual
//     unfreeze". /v1/price is serving a last-known-good value for it.
//  2. Redis is flushed (restart without persistence, an incident, an
//     eviction). The `freeze:<asset>:<quote>` marker is gone.
//
// Pre-0119 the ladder lived ONLY in that marker's JSON and in the
// aggregator's memory, and the orchestrator reads a missing marker under a
// live freeze as the ADR-0019 operator force-unfreeze — so the next tick
// released the freeze and republished the price a P1 had already escalated.
//
// This asserts the corrected values, not merely that something is returned:
// the freeze must read as PRESENT, and the restored state must carry the
// escalation flag and the exhausted extension count, because those are what
// the orchestrator's next Evaluate() reads to decide the freeze does not
// auto-unfreeze.
func TestWriter_LadderSurvivesRedisFlush(t *testing.T) {
	mr, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	now := time.Now().UTC()
	want := escalatedState(now)

	// The escalating tick: marker written, ladder mirrored durably.
	if err := w.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(), want, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if ladder.saves != 1 {
		t.Fatalf("SaveLadder called %d times, want 1 (every lifecycle transition mirrors)", ladder.saves)
	}

	// Sanity: while Redis is healthy the marker is the source.
	if _, present, lerr := w.LoadState(context.Background(), asset, quote); lerr != nil || !present {
		t.Fatalf("precondition: LoadState with a live marker = (present=%v, err=%v), want (true, nil)", present, lerr)
	}

	// ── the flush ────────────────────────────────────────────────
	mr.FlushAll()
	if _, gerr := mr.Get(cachekeys.Freeze(asset, quote).String()); gerr == nil {
		t.Fatal("precondition: marker should be gone after FlushAll")
	}

	got, present, err := w.LoadState(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("LoadState after flush: %v", err)
	}
	if !present {
		t.Fatal("freeze read as ABSENT after a Redis flush — the orchestrator treats that as an operator " +
			"force-unfreeze, so an ESCALATED freeze silently releases and republishes the price a P1 escalated")
	}
	if !got.Escalated {
		t.Error("restored state lost Escalated=true — the freeze would resume auto-unfreezing, " +
			"contradicting ADR-0019's \"stays active until manual unfreeze\"")
	}
	if got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("restored ExtensionsUsed = %d, want %d — a reset ladder restarts the 2-hour escalation clock",
			got.ExtensionsUsed, freeze.DefaultMaxExtensions)
	}
	if !got.Corroborated {
		t.Error("restored state lost Corroborated — the initial-hold rationale is not recoverable")
	}
	if !got.FiredAt.Equal(want.FiredAt) {
		t.Errorf("restored FiredAt = %v, want %v (the freeze's true age drives the minimum-hold floor)", got.FiredAt, want.FiredAt)
	}
	if !got.HoldUntil.Equal(want.HoldUntil) {
		t.Errorf("restored HoldUntil = %v, want %v", got.HoldUntil, want.HoldUntil)
	}
}

// TestWriter_OperatorUnfreezeStillSticks — the durable fallback must not
// take the ADR-0019 operator override away.
//
// `stellarindex-ops freeze-unfreeze` does both halves: it clears the Redis
// marker AND stamps recovered_at on the open freeze_events row. After that
// there is no OPEN durable row, so the pair must read ABSENT even though
// its last ladder was escalated — otherwise a human could not
// end a freeze that by construction never ends on its own.
func TestWriter_OperatorUnfreezeStillSticks(t *testing.T) {
	mr, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	if err := w.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(),
		escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	// The operator command, in its production order.
	if err := w.Clear(context.Background(), asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	ladder.close(asset, quote) // MarkRecovered
	_ = mr

	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Fatalf("LoadState after a full operator unfreeze = (present=%v, err=%v), want (false, nil) — "+
			"the override must still end the freeze", present, err)
	}
}

// TestWriter_LapsedDurableHoldIsNotResurrected — the freshness bound.
//
// A row can sit open indefinitely if the aggregator (which also runs the
// recovery worker) dies: nothing is left to close it. Honouring such a
// ladder unconditionally would mean a week-old freeze springs back to life
// the moment the aggregator restarts, pinning a healthy pair to a
// week-stale last-known-good price. Past hold_until + grace, behave exactly
// as before 0119.
func TestWriter_LapsedDurableHoldIsNotResurrected(t *testing.T) {
	_, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, time.Minute))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	stale := escalatedState(time.Now().UTC())
	stale.HoldUntil = time.Now().UTC().Add(-7 * 24 * time.Hour) // a week past its hold
	if err := ladder.SaveLadder(context.Background(), asset, quote, stale); err != nil {
		t.Fatalf("SaveLadder: %v", err)
	}

	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Fatalf("LoadState on a week-lapsed hold = (present=%v, err=%v), want (false, nil)", present, err)
	}

	// ...and the boundary case: a hold that expired only just now is still
	// inside the grace, so a flush during the tail of a hold is covered.
	fresh := escalatedState(time.Now().UTC())
	fresh.HoldUntil = time.Now().UTC().Add(-10 * time.Second)
	if err := ladder.SaveLadder(context.Background(), asset, quote, fresh); err != nil {
		t.Fatalf("SaveLadder: %v", err)
	}
	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || !present {
		t.Fatalf("LoadState 10s past a hold with a 1m grace = (present=%v, err=%v), want (true, nil)", present, err)
	}
}

// TestWriter_NoLadderStoreKeepsPre0119Behaviour — a deployment that wires
// no ladder store (and every existing test double) must behave bit-for-bit
// as before: a missing marker reads ABSENT.
func TestWriter_NoLadderStoreKeepsPre0119Behaviour(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	if err := w.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(),
		escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	mr.FlushAll()

	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Fatalf("LoadState with no ladder store = (present=%v, err=%v), want (false, nil)", present, err)
	}
}

// TestWriter_LadderStoreErrorDegradesToRedisOnly — a Postgres blip must not
// invent a freeze. The marker is already gone; reporting "present" off a
// failed read would pin a price to a last-known-good value with no evidence
// anything is wrong with it. Degrade to the pre-0119 answer instead.
func TestWriter_LadderStoreErrorDegradesToRedisOnly(t *testing.T) {
	_, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	ladder.err = errors.New("simulated postgres blip")
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Fatalf("LoadState with a failing ladder store = (present=%v, err=%v), want (false, nil)", present, err)
	}
}

// TestWriter_MarkDoesNotStampAnEmptyLadder — [Writer.Mark] is the
// lifecycle-FREE entry point (the triangulated-composite refusal, which
// owns no ladder). It passes a zero State, and stamping that would
// overwrite a real pair's durable ladder with zeros the reader would then
// restore as "fired just now, 0 extensions" — resetting the escalation
// clock through a side door.
func TestWriter_MarkDoesNotStampAnEmptyLadder(t *testing.T) {
	_, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	// A real, escalated ladder is already on the durable row.
	want := escalatedState(time.Now().UTC())
	if err := ladder.SaveLadder(context.Background(), asset, quote, want); err != nil {
		t.Fatalf("SaveLadder: %v", err)
	}
	saves := ladder.saves

	if err := w.Mark(context.Background(), asset, quote, "0.87", freezeDecision()); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	if ladder.saves != saves {
		t.Errorf("Mark stamped the ladder store (%d → %d saves); the lifecycle-free path must not touch it",
			saves, ladder.saves)
	}
	got, _, err := ladder.LoadLadder(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("LoadLadder: %v", err)
	}
	if !got.Escalated || got.ExtensionsUsed != want.ExtensionsUsed {
		t.Errorf("durable ladder was clobbered by Mark: %+v, want %+v", got, want)
	}
}

// TestWriter_LadderStoreErrorDoesNotFailMarkHold — same best-effort
// contract as the event sink: the Redis write is the serving path's
// authority and has already succeeded by the time the ladder is mirrored.
func TestWriter_LadderStoreErrorDoesNotFailMarkHold(t *testing.T) {
	_, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	ladder.err = errors.New("simulated postgres blip")
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	if err := w.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(),
		escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: ladder-store error must not propagate, got: %v", err)
	}
	if _, ok := rdb.Get(context.Background(), cachekeys.Freeze(asset, quote).String()).Result(); ok != nil {
		t.Errorf("marker missing after MarkHold with a failing ladder store: %v", ok)
	}
}

// TestWriter_ClearRetiresDurableLadder closes the auto-release window.
//
// The durable freeze_events row is closed asynchronously by the recovery
// worker (60s poll), so between an auto-release and that sweep the row is
// still OPEN with a live hold. An aggregator restarting inside that window
// would find no marker, rehydrate from the still-open row, and RE-FREEZE a
// pair whose anomaly had already cleared — serving a stale last-known-good
// price for two more buckets on no evidence.
//
// Clear therefore retires the ladder at the same instant it clears the
// marker, so both authorities agree the freeze is over.
func TestWriter_ClearRetiresDurableLadder(t *testing.T) {
	_, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	if err := w.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(),
		escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	// The auto-release path: the orchestrator Clears the marker. The durable
	// ROW is still open (the recovery worker has not swept yet).
	if err := w.Clear(context.Background(), asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if ladder.closed[ladder.key(asset, quote)] {
		t.Fatal("precondition: the durable row must still be OPEN — this test is about the pre-sweep window")
	}

	if _, present, err := w.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Fatalf("LoadState after Clear = (present=%v, err=%v), want (false, nil) — "+
			"a released pair must not be re-frozen from its own stale open row", present, err)
	}
}

// TestWriter_LadderWriteFailureIsCounted — the failure was always possible;
// the SILENCE is the defect. Neither the Writer nor the timescale sink holds
// a logger, so a persistently failing ladder write produced no signal on any
// surface: every freeze looked healthy right up until a Redis flush needed
// the ladder that had never been written.
//
// The concrete shape is a partially-failed deploy — the pipeline applies
// migrations BEFORE swapping the binary, so a new binary against a schema
// where 0119 did not apply sees every ladder write match zero rows.
//
// Asserts the exact per-call-site delta, and that a HEALTHY write leaves the
// counter alone (a counter that ticks on success is permanently firing and
// therefore useless).
func TestWriter_LadderWriteFailureIsCounted(t *testing.T) {
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)
	state := escalatedState(time.Now().UTC())

	markBefore := testutil.ToFloat64(obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("mark_hold"))
	clearBefore := testutil.ToFloat64(obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("clear"))

	// Healthy store: no failure counted.
	okStore := newFakeLadderStore()
	okWriter, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(okStore, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := okWriter.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(), state, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("mark_hold")); got != markBefore {
		t.Errorf("a successful ladder write moved the failure counter: %v → %v", markBefore, got)
	}

	// Failing store — e.g. migration 0119 not applied: every write matches
	// zero rows and the sink reports ErrNotFound.
	badStore := newFakeLadderStore()
	badStore.err = errors.New("no open row matched (0119 not applied?)")
	badWriter, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(badStore, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := badWriter.MarkHold(context.Background(), asset, quote, "0.87", freezeDecision(), state, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold must stay best-effort: %v", err)
	}
	if err := badWriter.Clear(context.Background(), asset, quote); err != nil {
		t.Fatalf("Clear must stay best-effort: %v", err)
	}

	if got, want := testutil.ToFloat64(obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("mark_hold")), markBefore+1; got != want {
		t.Errorf("ladder_write_failures{op=\"mark_hold\"} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("clear")), clearBefore+1; got != want {
		t.Errorf("ladder_write_failures{op=\"clear\"} = %v, want %v", got, want)
	}
}

// TestWriter_MarkRoundTrip — Mark writes a JSON Marker to the
// expected key with the expected TTL.
func TestWriter_MarkRoundTrip(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 12.5,
		Reason:       "deviation 12.5% exceeds 10% threshold for stablecoin",
	}
	if err := w.Mark(context.Background(), asset, quote, "1.000000000000", decision); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	key := cachekeys.Freeze(asset, quote)
	raw, err := rdb.Get(context.Background(), key.String()).Bytes()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var got freeze.Marker
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AssetID != asset.String() || got.QuoteID != quote.String() {
		t.Errorf("AssetID/QuoteID mismatch: %s/%s", got.AssetID, got.QuoteID)
	}
	if got.Action != anomaly.ActionFreeze {
		t.Errorf("Action = %q, want %q", got.Action, anomaly.ActionFreeze)
	}
	if got.Class != anomaly.ClassStablecoin {
		t.Errorf("Class = %q", got.Class)
	}
	if got.DeviationPct != 12.5 {
		t.Errorf("DeviationPct = %v, want 12.5", got.DeviationPct)
	}
	if got.FrozenAt.IsZero() {
		t.Error("FrozenAt is zero")
	}

	ttl := mr.TTL(key.String())
	if ttl == 0 || ttl > cachekeys.FreezeTTL {
		t.Errorf("TTL = %v, want ≤ %v and > 0", ttl, cachekeys.FreezeTTL)
	}
}

// TestWriter_MarkRefreshesTTL — calling Mark twice for the same
// pair refreshes the TTL (anomaly persists ⇒ freeze stays in
// effect). Mirrors the Redis SET ... EX semantics.
func TestWriter_MarkRefreshesTTL(t *testing.T) {
	mr, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 30*time.Second)
	asset, quote := nativeUSD(t)
	dec := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassDefault}

	if err := w.Mark(context.Background(), asset, quote, "", dec); err != nil {
		t.Fatalf("Mark (first): %v", err)
	}
	mr.FastForward(20 * time.Second)
	if err := w.Mark(context.Background(), asset, quote, "", dec); err != nil {
		t.Fatalf("Mark (refresh): %v", err)
	}

	key := cachekeys.Freeze(asset, quote)
	if ttl := mr.TTL(key.String()); ttl <= 10*time.Second {
		t.Errorf("TTL after refresh = %v, want > 10s (refresh extended it)", ttl)
	}
}

// TestWriter_MarkHoldRoundTrip — the lifecycle write path. The
// marker must carry the freeze [freeze.State] verbatim and expire on
// the caller's TTL (remaining hold + grace), NOT on the writer's flat
// default. A marker that outlived its hold would keep flags.frozen
// set after a release; one that expired inside its hold would let the
// serving path forget a live freeze.
func TestWriter_MarkHoldRoundTrip(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0) // default TTL = 5m
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	firedAt := time.Now().UTC().Truncate(time.Second)
	state := freeze.State{
		FiredAt:        firedAt,
		HoldUntil:      firedAt.Add(30 * time.Minute),
		ExtensionsUsed: 3,
		Escalated:      true,
		UnfreezeStreak: 1,
		Corroborated:   true,
	}
	const holdTTL = 35 * time.Minute
	if err := w.MarkHold(context.Background(), asset, quote, "1.000000000000",
		anomaly.Decision{Action: anomaly.ActionFreeze}, state, holdTTL); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	key := cachekeys.Freeze(asset, quote)
	if ttl := mr.TTL(key.String()); ttl != holdTTL {
		t.Errorf("marker TTL = %v, want the caller's %v (not the writer default %v)",
			ttl, holdTTL, cachekeys.FreezeTTL)
	}

	raw, err := rdb.Get(context.Background(), key.String()).Bytes()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var m freeze.Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !m.State.FiredAt.Equal(state.FiredAt) || !m.State.HoldUntil.Equal(state.HoldUntil) {
		t.Errorf("marker state times = %+v, want %+v", m.State, state)
	}
	if m.State.ExtensionsUsed != 3 || m.State.UnfreezeStreak != 1 || !m.State.Corroborated || !m.State.Escalated {
		t.Errorf("marker state = %+v, want %+v", m.State, state)
	}

	// LoadState must read back exactly what MarkHold wrote — this is
	// how the aggregator recovers the extension ladder after a
	// restart instead of silently restarting the escalation clock.
	got, ok, err := w.LoadState(context.Background(), asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if got.ExtensionsUsed != state.ExtensionsUsed || !got.HoldUntil.Equal(state.HoldUntil) {
		t.Errorf("LoadState = %+v, want %+v", got, state)
	}
	// An escalated freeze holds until manual unfreeze (ADR-0019); losing
	// the flag on rehydrate would let two healthy buckets auto-release it.
	if !got.Escalated {
		t.Errorf("LoadState Escalated = false, want true (escalation lost across the marker round-trip)")
	}
}

// TestWriter_ClearRemovesTheMarker — the auto-unfreeze / operator-
// override path. Letting the TTL lapse instead would keep
// flags.frozen true for the whole remaining hold after the price was
// republished as healthy.
func TestWriter_ClearRemovesTheMarker(t *testing.T) {
	_, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	l, _ := freeze.NewLooker(rdb)
	asset, quote := nativeUSD(t)

	if err := w.MarkHold(context.Background(), asset, quote, "",
		anomaly.Decision{Action: anomaly.ActionFreeze},
		freeze.State{FiredAt: time.Now().UTC()}, time.Hour); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if frozen, _ := l.FrozenForPair(context.Background(), asset, quote); !frozen {
		t.Fatal("setup: marker not present")
	}

	if err := w.Clear(context.Background(), asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if frozen, _ := l.FrozenForPair(context.Background(), asset, quote); frozen {
		t.Error("marker still present after Clear — flags.frozen would stay set " +
			"for the marker's full remaining-hold TTL")
	}
	// Idempotent.
	if err := w.Clear(context.Background(), asset, quote); err != nil {
		t.Errorf("Clear on an absent marker returned %v, want nil", err)
	}
	// And LoadState reports absence, which the orchestrator reads as
	// the ADR-0019 operator force-unfreeze.
	if _, ok, err := w.LoadState(context.Background(), asset, quote); ok || err != nil {
		t.Errorf("LoadState after Clear: ok=%v err=%v, want (false, nil)", ok, err)
	}
}

// TestWriter_LoadState_PreLifecycleMarker — a marker written by the
// flat-TTL Mark path (or by an older build) decodes to a zero State
// rather than erroring, so a rolling deploy doesn't fail ticks.
func TestWriter_LoadState_PreLifecycleMarker(t *testing.T) {
	_, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	asset, quote := nativeUSD(t)

	if err := w.Mark(context.Background(), asset, quote, "",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	st, ok, err := w.LoadState(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !ok {
		t.Fatal("LoadState reported no marker for a Mark-written key")
	}
	if st.Active() {
		t.Errorf("pre-lifecycle marker decoded to an ACTIVE state %+v — the "+
			"aggregator would inherit a hold nobody set", st)
	}
}

// TestWriter_Mark_FiresEventSink — Mark must call the wired sink
// in addition to the Redis write. Production wires the timescale-
// backed sink so the freeze_events hypertable mirrors the Redis
// state; this test pins that the Writer respects the WithEventSink
// option.
func TestWriter_Mark_FiresEventSink(t *testing.T) {
	_, rdb := newRedis(t)
	sink := &recordingSink{}
	w, err := freeze.NewWriter(rdb, 0, freeze.WithEventSink(sink))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 8.5,
		Reason:       "test",
	}
	if err := w.Mark(context.Background(), asset, quote, "0.999500000000", decision); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink fired %d times, want 1", len(sink.calls))
	}
	got := sink.calls[0]
	if got.Asset.String() != asset.String() {
		t.Errorf("asset = %s, want %s", got.Asset.String(), asset.String())
	}
	if got.Quote.String() != quote.String() {
		t.Errorf("quote = %s, want %s", got.Quote.String(), quote.String())
	}
	if got.Decision.DeviationPct != decision.DeviationPct {
		t.Errorf("deviation = %v, want %v", got.Decision.DeviationPct, decision.DeviationPct)
	}
	if got.FrozenValue != "0.999500000000" {
		t.Errorf("frozenValue = %q, want %q", got.FrozenValue, "0.999500000000")
	}
}

// TestWriter_Mark_SinkErrorIsSwallowed — a sink failure must not
// fail the Mark call. The Redis write is the load-bearing operation
// for flags.frozen on the API; the durable mirror is best-effort.
func TestWriter_Mark_SinkErrorIsSwallowed(t *testing.T) {
	_, rdb := newRedis(t)
	sink := &recordingSink{err: errExploded}
	w, err := freeze.NewWriter(rdb, 0, freeze.WithEventSink(sink))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	if err := w.Mark(context.Background(), asset, quote, "",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatalf("Mark: sink error must not propagate, got: %v", err)
	}
}

// ─── the durable ladder is per WINDOW (migration 0163) ───────────
//
// Migration 0119 gave the ADR-0019 ladder a durable home, but on the
// pair's single open `freeze_events` row: four columns, no window. The
// lifecycle runs one state machine per (pair, window), every frozen
// window mirrors its ladder on every tick, and so the durable record was
// whichever window wrote LAST. These tests pin the consequences that
// matter once Redis has lost the marker, which is the only time the
// durable ladder is read at all.

// windowedFakeLadderStore is fakeLadderStore plus the window-aware half.
//
// The pair-level SaveLadder / LoadLadder keep last-writer-wins semantics
// on purpose — that IS the pre-0163 store — so a Writer that still
// mirrors through them reproduces the defect, and one that uses the
// window-aware calls does not. The window-aware half maintains the same
// fail-closed pair-level summary the SQL does, because the recovery
// worker and `stellarindex-ops freeze-unfreeze -list` still read it.
type windowedFakeLadderStore struct {
	*fakeLadderStore
	windows map[string]map[time.Duration]freeze.State
	unowned map[string]freeze.State
}

func newWindowedFakeLadderStore() *windowedFakeLadderStore {
	return &windowedFakeLadderStore{
		fakeLadderStore: newFakeLadderStore(),
		windows:         map[string]map[time.Duration]freeze.State{},
		unowned:         map[string]freeze.State{},
	}
}

// SaveLadder is the pair-level write. An inactive state retires the whole
// durable record (what [freeze.Writer.Clear] relies on).
func (f *windowedFakeLadderStore) SaveLadder(ctx context.Context, asset, quote canonical.Asset, st freeze.State) error {
	if !st.Active() {
		delete(f.windows, f.key(asset, quote))
		delete(f.unowned, f.key(asset, quote))
	}
	return f.fakeLadderStore.SaveLadder(ctx, asset, quote, st)
}

func (f *windowedFakeLadderStore) SaveWindowLadder(
	_ context.Context, asset, quote canonical.Asset, window time.Duration, st freeze.State,
) error {
	k := f.key(asset, quote)
	if f.closed[k] {
		return nil
	}
	entries, ok := f.windows[k]
	if !ok {
		entries = map[time.Duration]freeze.State{}
		f.windows[k] = entries
		// First window-aware write onto a pre-0163 row: the pair-level
		// ladder has no owner, so it is kept as the unowned one.
		if legacy, had := f.states[k]; had && legacy.Active() {
			f.unowned[k] = legacy
		}
	}
	if st.Active() {
		entries[window] = st
	} else {
		delete(entries, window)
	}
	f.states[k] = f.summary(k)
	return nil
}

// summary is the fail-closed pair-level view: the furthest hold, the
// highest rung, escalated if ANY window is.
func (f *windowedFakeLadderStore) summary(k string) freeze.State {
	var out freeze.State
	fold := func(st freeze.State) {
		if !st.Active() {
			return
		}
		if out.FiredAt.IsZero() || st.FiredAt.Before(out.FiredAt) {
			out.FiredAt = st.FiredAt
		}
		if st.HoldUntil.After(out.HoldUntil) {
			out.HoldUntil = st.HoldUntil
		}
		if st.ExtensionsUsed > out.ExtensionsUsed {
			out.ExtensionsUsed = st.ExtensionsUsed
		}
		out.Escalated = out.Escalated || st.Escalated
		out.Corroborated = out.Corroborated || st.Corroborated
	}
	for _, st := range f.windows[k] {
		fold(st)
	}
	fold(f.unowned[k])
	return out
}

func (f *windowedFakeLadderStore) LoadWindowLadders(
	_ context.Context, asset, quote canonical.Asset,
) (map[time.Duration]freeze.State, freeze.State, bool, error) {
	if f.err != nil {
		return nil, freeze.State{}, false, f.err
	}
	k := f.key(asset, quote)
	if f.closed[k] {
		return nil, freeze.State{}, false, nil
	}
	pair, ok := f.states[k]
	if !ok || pair.HoldUntil.IsZero() {
		return nil, freeze.State{}, false, nil
	}
	entries, windowed := f.windows[k]
	if !windowed {
		// A pre-0163 row: one pair-level ladder, owner unknown.
		return map[time.Duration]freeze.State{}, pair, true, nil
	}
	out := make(map[time.Duration]freeze.State, len(entries))
	for w, st := range entries {
		out[w] = st
	}
	return out, f.unowned[k], true, nil
}

func freshState(now time.Time) freeze.State {
	return freeze.State{
		FiredAt:   now.Add(-time.Minute),
		HoldUntil: now.Add(9 * time.Minute),
	}
}

// TestWriter_DurableLadderIsPerWindow is the regression for the durable
// half of the pair-keyed ladder.
//
// The 1h window has climbed the whole ladder and ESCALATED — ADR-0019
// holds it "until manual unfreeze". The 5m window of the same pair then
// fires a fresh freeze of its own. Both mirror their ladder durably on
// every tick. Redis is then lost and the aggregator restarts.
//
// A last-writer-wins durable record would be the 5m window's: the
// ten-minute, zero-extension, un-escalated ladder. Every window
// would rehydrate that — the escalated 1h freeze would come back as an
// ordinary one that auto-unfreezes (the dangerous direction), and the 24h
// window, never frozen, would come back frozen.
func TestWriter_DurableLadderIsPerWindow(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()

	escalated := escalatedState(now)
	fresh := freshState(now)
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.1242",
		freezeDecision(), escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	mr.FlushAll() // Redis is lost…
	restarted, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil { // …and the aggregator restarts.
		t.Fatalf("NewWriter (restart): %v", err)
	}

	gotLong, ok, err := restarted.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = ok=%v err=%v, want present", ok, err)
	}
	if !gotLong.Escalated || gotLong.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h window rehydrated %+v, want its own ESCALATED ladder (extensions=%d): "+
			"the 5m window's later durable write replaced it, so an escalated freeze "+
			"resumes auto-unfreezing", gotLong, freeze.DefaultMaxExtensions)
	}

	gotShort, _, err := restarted.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if !gotShort.Active() || gotShort.Escalated || gotShort.ExtensionsUsed != 0 ||
		!gotShort.HoldUntil.Equal(fresh.HoldUntil) {
		t.Errorf("5m window rehydrated %+v, want its own fresh ladder %+v", gotShort, fresh)
	}

	gotDay, present, err := restarted.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen; the pair-keyed durable "+
			"ladder copied a sibling's freeze onto it", gotDay)
	}
	if !present {
		t.Error("presence must stay pair-wide on the durable side too: the pair IS frozen, " +
			"so a window with no ladder of its own still reads present (a live freeze " +
			"must not read this as the operator override)")
	}
}

// TestWriter_FirstRemarkAfterRedisLossRestoresEveryWindow pins the write
// side of the same recovery. After a flush the first window to re-mark
// rebuilds the marker, and from then on the marker — not the durable
// store — answers every cold sibling. It must therefore be rebuilt with
// EVERY window's durable ladder, or the 1h window's escalation is lost
// one tick later than in the test above instead of not at all.
func TestWriter_FirstRemarkAfterRedisLossRestoresEveryWindow(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()

	escalated := escalatedState(now)
	fresh := freshState(now)
	for _, m := range []struct {
		window time.Duration
		state  freeze.State
	}{{longWindow, escalated}, {shortWindow, fresh}} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, m.window, "0.1242",
			freezeDecision(), m.state, 30*time.Minute); err != nil {
			t.Fatalf("MarkHoldForWindow(%s): %v", m.window, err)
		}
	}
	mr.FlushAll()

	// The 5m window ticks first after the flush and re-marks.
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m) after flush: %v", err)
	}

	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = ok=%v err=%v, want present", ok, err)
	}
	if !gotLong.Escalated || !gotLong.FiredAt.Equal(escalated.FiredAt) {
		t.Errorf("1h window read %+v from the rebuilt marker, want its own escalated ladder %+v",
			gotLong, escalated)
	}
	gotDay, _, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window read %+v from the rebuilt marker — it was never frozen", gotDay)
	}
}

// TestWriter_PreWindowDurableRowStillRehydratesEveryWindow is the
// fail-closed guard for a row written before migration 0163: it carries
// one pair-level ladder and nothing that says whose. Narrowing it to "no
// window owns this" would DROP a freeze that is still running, so it
// keeps answering for every window until window-aware writes replace it.
func TestWriter_PreWindowDurableRowStillRehydratesEveryWindow(t *testing.T) {
	_, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Written by the previous binary: pair-level only.
	if err := store.fakeLadderStore.SaveLadder(ctx, asset, quote, escalatedState(now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, window := range []time.Duration{shortWindow, longWindow, 24 * time.Hour} {
		got, ok, err := w.LoadStateForWindow(ctx, asset, quote, window)
		if err != nil || !ok || !got.Escalated {
			t.Errorf("window %s = (%+v, ok=%v, err=%v), want the pair-level escalated ladder: "+
				"a pre-0163 row has no owner, and dropping it releases a live freeze",
				window, got, ok, err)
		}
	}
}
