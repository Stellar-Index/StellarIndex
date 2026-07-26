package freeze_test

import (
	"context"
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
// the ladder it used to carry was escalated — otherwise a human could not
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
