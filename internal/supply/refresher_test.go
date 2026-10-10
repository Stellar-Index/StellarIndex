package supply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"
)

type stubLedgers struct {
	ledger     uint32
	observedAt time.Time
	err        error
}

func (s stubLedgers) LatestKnownLedger(_ context.Context) (uint32, time.Time, error) {
	return s.ledger, s.observedAt, s.err
}

type stubComputer struct {
	out Supply
	err error
}

func (s stubComputer) Compute(_ context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	if s.err != nil {
		return Supply{}, s.err
	}
	out := s.out
	out.LedgerSequence = ledger
	out.ObservedAt = observedAt
	return out, nil
}

type stubInserter struct {
	calls int
	err   error
}

func (s *stubInserter) InsertSupply(_ context.Context, _ Supply) error {
	s.calls++
	return s.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRefresher_HappyPath(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:          "XLM",
			TotalSupply:       big.NewInt(1_000_000),
			CirculatingSupply: big.NewInt(900_000),
			Basis:             BasisXLMSDFReserveExclusion,
		}},
		inserter,
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s, want ok; err=%v", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1", inserter.calls)
	}
	if out.Snapshot.LedgerSequence != 50_000_000 {
		t.Errorf("snapshot ledger=%d want 50000000", out.Snapshot.LedgerSequence)
	}
}

// TestRefresher_StaleComponentRejected pins that a snapshot whose MinComponentLedger lags
// the snapshot ledger by more than the threshold is rejected
// with OutcomeKindStaleComponent. The inserter is NOT called.
func TestRefresher_StaleComponentRejected(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_001_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_001_500,
			MinComponentLedger: 50_000_000, // 1500 ledgers behind
		}},
		inserter,
		discardLogger(),
		// threshold 1000 — gap 1500 > 1000, must reject.
		WithStaleComponentLedgers(1000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("kind=%s, want %s (err=%v)", out.Kind, OutcomeKindStaleComponent, out.Err)
	}
	if inserter.calls != 0 {
		t.Errorf("inserter called on stale-component snapshot (want 0, got %d)", inserter.calls)
	}
}

// TestRefresher_StaleComponentBelowThresholdAccepted pins the
// happy-path branch: a snapshot whose component lag is within
// the threshold inserts cleanly.
func TestRefresher_StaleComponentBelowThresholdAccepted(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_000_500,
			MinComponentLedger: 50_000_000, // 500 ledgers behind — within threshold
		}},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s, want ok (err=%v)", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1", inserter.calls)
	}
}

// TestRefresher_StaleComponentZeroDisablesGate pins the
// legacy-compat branch: when the computer doesn't populate
// MinComponentLedger (legacy / non-storage-backed paths) the
// gate is skipped and snapshots insert as before.
func TestRefresher_StaleComponentZeroDisablesGate(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:          "XLM",
			TotalSupply:       big.NewInt(1_000_000),
			CirculatingSupply: big.NewInt(900_000),
			Basis:             BasisXLMSDFReserveExclusion,
			LedgerSequence:    50_000_500,
			// MinComponentLedger left zero — legacy computer.
		}},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s, want ok (err=%v)", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1", inserter.calls)
	}
}

// TestRefresher_StrictFreshness_RejectsZeroAnchor pins the
// strict-mode gate:
// a snapshot with `MinComponentLedger == 0` (no freshness
// anchor) is rejected with `OutcomeKindMissingFreshness` when
// `WithStrictFreshnessRequired(true)` is wired. The inserter
// is NOT called.
func TestRefresher_StrictFreshness_RejectsZeroAnchor(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_000_000,
			MinComponentLedger: 0, // no freshness signal — the audit's risk shape
		}},
		inserter,
		discardLogger(),
		WithStrictFreshnessRequired(true),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindMissingFreshness {
		t.Fatalf("kind=%s, want %s (err=%v)", out.Kind, OutcomeKindMissingFreshness, out.Err)
	}
	if inserter.calls != 0 {
		t.Errorf("inserter called on freshness-less snapshot under strict mode (want 0, got %d)", inserter.calls)
	}
}

// TestRefresher_StrictFreshness_AcceptsAnchored — the strict-
// mode gate ONLY rejects zero-anchor snapshots; a snapshot
// with a real `MinComponentLedger` (and within the
// stale-component window) still inserts cleanly.
func TestRefresher_StrictFreshness_AcceptsAnchored(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_000_500,
			MinComponentLedger: 50_000_000, // anchored, 500 ledgers behind
		}},
		inserter,
		discardLogger(),
		WithStrictFreshnessRequired(true),
		WithStaleComponentLedgers(1000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s, want ok (err=%v)", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1", inserter.calls)
	}
}

// TestRefresher_StrictFreshness_DefaultOff — without
// `WithStrictFreshnessRequired(true)`, a freshness-less
// snapshot still publishes (legacy permissive behaviour
// preserved). This pins the backwards-compat default so a
// future operator can't quietly tighten without a config flip.
func TestRefresher_StrictFreshness_DefaultOff(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_000_000,
			MinComponentLedger: 0,
		}},
		inserter,
		discardLogger(),
		// No WithStrictFreshnessRequired — default false.
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s, want ok (default permissive); err=%v", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1 (default permissive must publish)", inserter.calls)
	}
}

// dynComputer returns a Supply whose MinComponentLedger is fixed
// but whose LedgerSequence tracks the (advancing) chain tip the
// stubLedgers feeds in. It models a DORMANT asset: the chain tip
// climbs every tick while the asset's last balance-change ledger
// (MinComponentLedger) stays put.
type dynComputer struct {
	assetKey           string
	minComponentLedger uint32
}

func (c dynComputer) Compute(_ context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	return Supply{
		AssetKey:           c.assetKey,
		TotalSupply:        big.NewInt(1_000_000),
		CirculatingSupply:  big.NewInt(900_000),
		Basis:              BasisXLMSDFReserveExclusion,
		LedgerSequence:     ledger,
		ObservedAt:         observedAt,
		MinComponentLedger: c.minComponentLedger,
	}, nil
}

// mutableLedgers lets a test advance the chain tip between ticks
// to simulate the network closing ledgers while an asset sits
// dormant.
type mutableLedgers struct {
	ledger     uint32
	observedAt time.Time
}

func (m *mutableLedgers) LatestKnownLedger(_ context.Context) (uint32, time.Time, error) {
	return m.ledger, m.observedAt, nil
}

// TestRefresher_DormantAssetNotPermanentlyRejected pins that
// a DORMANT asset (MinComponentLedger frozen because it had no
// balance change) whose chain-tip gap grows past the threshold is
// NOT permanently rejected. The FIRST tick that crosses the
// threshold is rejected once (cold start — we can't yet tell
// dormant from a freshly-stalled producer), but every subsequent
// tick with the SAME unchanged MinComponentLedger is recognised
// as dormant and accepted (OutcomeKindDormant), so the supply row
// never freezes silently.
//
// This is the exact live-PHO shape from the finding: gap grew
// 1017 -> 1324 while MinComponentLedger never moved.
func TestRefresher_DormantAssetNotPermanentlyRejected(t *testing.T) {
	const minComp = 50_000_000
	ledgers := &mutableLedgers{
		ledger:     minComp + 1017, // gap 1017 > 1000 default
		observedAt: time.Unix(1_770_000_000, 0).UTC(),
	}
	inserter := &stubInserter{}
	r := NewRefresher(
		ledgers,
		dynComputer{assetKey: "XLM", minComponentLedger: minComp},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
	)

	// Tick 1: first time we've seen this asset; it's already
	// lagging. We can't distinguish dormant from stalled yet, so
	// reject once (conservative cold-start).
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick1 kind=%s, want %s (first-observation cold start should reject; err=%v)", out.Kind, OutcomeKindStaleComponent, out.Err)
	}
	if inserter.calls != 0 {
		t.Fatalf("tick1 inserter.calls=%d want 0 (cold-start rejection must not insert)", inserter.calls)
	}

	// Tick 2: tip advanced (gap now 1324), MinComponentLedger
	// UNCHANGED → recognised as dormant → accepted + inserted.
	ledgers.ledger = minComp + 1324
	out = r.Tick(context.Background())
	if out.Kind != OutcomeKindDormant {
		t.Fatalf("tick2 kind=%s, want %s (unchanged MinComponentLedger is a dormant asset, must accept; err=%v)", out.Kind, OutcomeKindDormant, out.Err)
	}
	if inserter.calls != 1 {
		t.Fatalf("tick2 inserter.calls=%d want 1 (dormant snapshot must be inserted)", inserter.calls)
	}
	if out.Snapshot.LedgerSequence != minComp+1324 {
		t.Errorf("tick2 snapshot ledger=%d want %d", out.Snapshot.LedgerSequence, minComp+1324)
	}

	// Tick 3+: still dormant, gap keeps growing — must keep
	// accepting, never regress to a permanent rejection.
	ledgers.ledger = minComp + 5000
	out = r.Tick(context.Background())
	if out.Kind != OutcomeKindDormant {
		t.Fatalf("tick3 kind=%s, want %s (a quiet asset must never permanently freeze)", out.Kind, OutcomeKindDormant)
	}
	if inserter.calls != 2 {
		t.Errorf("tick3 inserter.calls=%d want 2", inserter.calls)
	}
}

// TestRefresher_AdvancingLaggingWatermarkRejected pins the changed-
// watermark branch of the gate: when MinComponentLedger is still
// CHANGING tick-over-tick but remains past the threshold (an observer
// that is progressing yet far behind, or one that regressed), the gate
// rejects. A producer that dies with a frozen watermark does not reach
// this branch; see TestRefresher_DormantAfterHealthyWindow.
func TestRefresher_AdvancingLaggingWatermarkRejected(t *testing.T) {
	ledgers := &mutableLedgers{
		ledger:     50_002_000,
		observedAt: time.Unix(1_770_000_000, 0).UTC(),
	}
	inserter := &stubInserter{}
	// First tick: gap 2000 > 1000, first observation → reject.
	comp := dynComputer{assetKey: "XLM", minComponentLedger: 50_000_000}
	r := NewRefresher(ledgers, comp, inserter, discardLogger(), WithStaleComponentLedgers(1000))
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick1 kind=%s want %s", out.Kind, OutcomeKindStaleComponent)
	}

	// Second tick: MinComponentLedger advanced (producer is
	// moving) but is STILL past the threshold. Changed value →
	// not dormant → still reject. Swap the computer for one with
	// a newer-but-still-lagging component ledger.
	r.computer = dynComputer{assetKey: "XLM", minComponentLedger: 50_000_500}
	ledgers.ledger = 50_002_500 // gap 2000, still > 1000
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick2 kind=%s want %s (advancing-but-lagging producer must still reject)", out.Kind, OutcomeKindStaleComponent)
	}
	if inserter.calls != 0 {
		t.Errorf("inserter.calls=%d want 0 (no insert on either rejection)", inserter.calls)
	}
}

// TestRefresher_DormantAfterHealthyWindow pins the realistic
// timeline: an asset that was fresh (within threshold) for a
// while and THEN goes dormant must be accepted as dormant on the
// very first tick the gap crosses the threshold — because the
// in-threshold ticks already recorded the (unchanged)
// MinComponentLedger, so the cross is recognised as "unchanged →
// dormant", NOT as a first-observation cold start.
//
// A producer that dies after a healthy window emits the same input, so
// the only thing that stops it is the dormancy horizon: once the frozen
// gap crosses it the same input must reach stale_component.
func TestRefresher_DormantAfterHealthyWindow(t *testing.T) {
	const minComp = 50_000_000
	ledgers := &mutableLedgers{
		ledger:     minComp + 500, // gap 500 < 1000 → fresh
		observedAt: time.Unix(1_770_000_000, 0).UTC(),
	}
	inserter := &stubInserter{}
	r := NewRefresher(
		ledgers,
		dynComputer{assetKey: "XLM", minComponentLedger: minComp},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
	)
	// Tick 1: fresh, inserts normally, records MinComponentLedger.
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindOK {
		t.Fatalf("tick1 kind=%s want ok (within threshold)", out.Kind)
	}
	// Tick 2: asset went quiet; tip advanced past threshold but
	// MinComponentLedger is unchanged from the fresh tick → must
	// be recognised as dormant immediately (NO cold-start reject),
	// because the healthy tick already recorded the value.
	ledgers.ledger = minComp + 1500
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindDormant {
		t.Fatalf("tick2 kind=%s want %s (dormant after a healthy window must not cold-start reject)", out.Kind, OutcomeKindDormant)
	}
	if inserter.calls != 2 {
		t.Errorf("inserter.calls=%d want 2 (both the fresh tick and the dormant tick insert)", inserter.calls)
	}
	// Tick 3: same frozen watermark, now past the default dormancy
	// horizon — a dead producer looks exactly like this, so reject.
	ledgers.ledger = minComp + DefaultMaxDormantComponentLedgers + 1
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick3 kind=%s want %s (a watermark frozen past the dormancy horizon must fail closed)", out.Kind, OutcomeKindStaleComponent)
	}
	if inserter.calls != 2 {
		t.Errorf("inserter.calls=%d want 2 (the past-horizon tick must not insert)", inserter.calls)
	}
}

func TestRefresher_NoLedger(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{err: errors.New("no cursors yet")},
		stubComputer{},
		inserter,
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindNoLedger {
		t.Errorf("kind=%s want %s", out.Kind, OutcomeKindNoLedger)
	}
	if inserter.calls != 0 {
		t.Errorf("inserter called on no-ledger outcome")
	}
}

// TestRefresher_NoObservation — ErrNoObservation surfaces as the
// dedicated outcome so the bootstrap-progress signal is chartable.
func TestRefresher_NoObservation(t *testing.T) {
	r := NewRefresher(
		stubLedgers{ledger: 1, observedAt: time.Now()},
		stubComputer{err: ErrNoObservation},
		&stubInserter{},
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindNoObservation {
		t.Errorf("kind=%s want %s", out.Kind, OutcomeKindNoObservation)
	}
}

// TestRefresher_GenesisBaselineNotSeeded — an unseeded SAC wrapper routes to
// the benign missing_baseline outcome, not the paging compute_error.
func TestRefresher_GenesisBaselineNotSeeded(t *testing.T) {
	r := NewRefresher(
		stubLedgers{ledger: 1, observedAt: time.Now()},
		stubComputer{err: ErrGenesisBaselineNotSeeded},
		&stubInserter{},
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindMissingBaseline {
		t.Errorf("kind=%s want %s", out.Kind, OutcomeKindMissingBaseline)
	}
}

// TestRefresher_GenericComputeError — non-observation errors map
// to compute_error.
func TestRefresher_GenericComputeError(t *testing.T) {
	r := NewRefresher(
		stubLedgers{ledger: 1, observedAt: time.Now()},
		stubComputer{err: errors.New("computer is broken")},
		&stubInserter{},
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindComputeError {
		t.Errorf("kind=%s want %s", out.Kind, OutcomeKindComputeError)
	}
}

func TestRefresher_WriteError(t *testing.T) {
	inserter := &stubInserter{err: errors.New("DB unreachable")}
	r := NewRefresher(
		stubLedgers{ledger: 1, observedAt: time.Now()},
		stubComputer{out: Supply{
			AssetKey:          "XLM",
			TotalSupply:       big.NewInt(1),
			CirculatingSupply: big.NewInt(1),
		}},
		inserter,
		discardLogger(),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindWriteError {
		t.Errorf("kind=%s want %s", out.Kind, OutcomeKindWriteError)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter should have been called once before failing")
	}
}

// TestRefresher_PerAssetStaleComponentOverride pins
// per-asset override behaviour: a known-low-activity asset (PHO governance token)
// passes the gate at a more permissive threshold while the
// global default still rejects high-activity assets at the same
// component lag.
//
// PHO supply rows lag by ~1190 ledgers, past the global threshold
// of 1000. Per-asset override of 5000 (≈7 h) accepts the legitimate
// snapshot without loosening the gate for XLM.
func TestRefresher_PerAssetStaleComponentOverride(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_001_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "PHO:GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_001_500,
			MinComponentLedger: 50_000_310, // gap = 1190 ledgers
		}},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000), // global default — would reject
		WithStaleComponentLedgersFor("PHO:GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6", 5000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindOK {
		t.Fatalf("kind=%s want ok (per-asset override should have accepted gap=1190 under PHO's 5000 threshold; err=%v)", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter calls = %d, want 1 (snapshot should have been inserted)", inserter.calls)
	}
}

// TestRefresher_PerAssetStaleComponentDoesNotLoosenOthers pins the
// inverse invariant: the per-asset override for PHO must NOT
// relax the gate for a different asset (XLM here) which still
// uses the global threshold.
func TestRefresher_PerAssetStaleComponentDoesNotLoosenOthers(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_001_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_001_500,
			MinComponentLedger: 50_000_000, // gap = 1500 > global 1000
		}},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
		WithStaleComponentLedgersFor("PHO:GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6", 5000),
	)
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("kind=%s want %s (XLM should still hit the global threshold; per-asset override is for PHO only)", out.Kind, OutcomeKindStaleComponent)
	}
	if inserter.calls != 0 {
		t.Errorf("inserter called on stale-component snapshot (want 0, got %d)", inserter.calls)
	}
}

// recordingInserter captures the snapshots that actually reached the
// persistence layer, so a test can assert WHICH supply figure was
// published at WHICH ledger — not merely how many writes happened.
type recordingInserter struct {
	snaps []Supply
	err   error
}

func (r *recordingInserter) InsertSupply(_ context.Context, snap Supply) error {
	if r.err != nil {
		return r.err
	}
	r.snaps = append(r.snaps, snap)
	return nil
}

// TestRefresher_StalledObserverNotReStampedForeverAsDormant pins
// that the dormancy carve-out
// accepts a snapshot whenever MinComponentLedger is UNCHANGED
// tick-over-tick — but a STALLED component observer (one that died
// and stopped writing observations) produces exactly that signal,
// forever. Unbounded, the gate therefore re-stamps a frozen
// circulating supply at the ever-advancing chain tip for as long as
// the observer stays dead, and reports it as the benign `dormant`
// outcome the supply-refresh alert deliberately excludes — a stale
// money figure served as fresh, with nothing paging.
//
// The dormancy benefit-of-the-doubt is therefore BOUNDED: within the
// horizon a quiet asset is still accepted, but
// once the component anchor has been frozen for longer than the
// horizon we can no longer defend "the last observation IS the
// current supply", so the gate fails closed with the alertable
// stale_component outcome instead of publishing.
func TestRefresher_StalledObserverNotReStampedForeverAsDormant(t *testing.T) {
	const minComp = 50_000_000
	ledgers := &mutableLedgers{
		ledger:     minComp + 1017, // gap 1017 > 1000 threshold
		observedAt: time.Unix(1_770_000_000, 0).UTC(),
	}
	inserter := &recordingInserter{}
	r := NewRefresher(
		ledgers,
		// The observer is DEAD: minComponentLedger never moves again.
		dynComputer{assetKey: "XLM", minComponentLedger: minComp},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
		// Dormancy horizon left at DefaultMaxDormantComponentLedgers
		// (17 280 ledgers ≈ 24 h at 5 s close cadence) on purpose, so
		// this test pins the SHIPPED default posture, not a
		// test-only tuning.
	)

	// Tick 1 — cold start, already lagging: rejected (unchanged
	// pre-existing behaviour).
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick1 kind=%s want %s (cold-start lagging must reject)", out.Kind, OutcomeKindStaleComponent)
	}

	// Tick 2 — anchor frozen, gap 5000 but still INSIDE the dormancy
	// horizon: accepted as dormant. This half guards that the
	// carve-out is bounded, not deleted.
	ledgers.ledger = minComp + 5_000
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindDormant {
		t.Fatalf("tick2 kind=%s want %s (in-horizon dormancy must still be accepted; F-1320 must not regress)", out.Kind, OutcomeKindDormant)
	}
	if len(inserter.snaps) != 1 {
		t.Fatalf("tick2 inserts=%d want 1 (in-horizon dormant snapshot is published)", len(inserter.snaps))
	}

	// Tick 3 — the anchor has now been frozen for 20 000 ledgers
	// (~28 h at 5 s close cadence), past the 17 280 horizon. This is
	// the stalled-observer shape and MUST fail closed.
	ledgers.ledger = minComp + 20_000
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick3 kind=%s want %s (an anchor frozen past the dormancy horizon is a stalled observer, not a dormant asset — it must not be re-stamped fresh)", out.Kind, OutcomeKindStaleComponent)
	}
	if out.Err == nil {
		t.Error("tick3 Err=nil, want a non-nil rejection error for operator diagnosis")
	}

	// The money assertion: nothing was published at the far-future
	// tip. The most recent row remains the in-horizon one, so a
	// consumer computing market cap can still see the supply figure
	// stop advancing instead of being handed a frozen number wearing
	// a fresh ledger stamp.
	if len(inserter.snaps) != 1 {
		t.Fatalf("tick3 inserts=%d want 1 (past-horizon snapshot must NOT be published)", len(inserter.snaps))
	}
	if got := inserter.snaps[0].LedgerSequence; got != minComp+5_000 {
		t.Errorf("last published ledger=%d want %d (a frozen supply must never be re-stamped at the current tip)", got, minComp+5_000)
	}
}

// TestRefresher_MaxDormantComponentLedgersZeroDisablesHorizon pins
// the operator escape hatch: passing 0 restores the unbounded
// posture for deployments that knowingly watch assets
// dormant for longer than any horizon and prefer a re-stamped row
// to a gap. Explicit opt-in, never the default.
func TestRefresher_MaxDormantComponentLedgersZeroDisablesHorizon(t *testing.T) {
	const minComp = 50_000_000
	ledgers := &mutableLedgers{
		ledger:     minComp + 1017,
		observedAt: time.Unix(1_770_000_000, 0).UTC(),
	}
	inserter := &stubInserter{}
	r := NewRefresher(
		ledgers,
		dynComputer{assetKey: "XLM", minComponentLedger: minComp},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000),
		WithMaxDormantComponentLedgers(0), // unbounded — legacy posture
	)
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindStaleComponent {
		t.Fatalf("tick1 kind=%s want %s", out.Kind, OutcomeKindStaleComponent)
	}
	ledgers.ledger = minComp + 10_000_000 // absurdly far past any horizon
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindDormant {
		t.Fatalf("tick2 kind=%s want %s (horizon disabled → unbounded dormancy accept)", out.Kind, OutcomeKindDormant)
	}
	if inserter.calls != 1 {
		t.Errorf("inserter.calls=%d want 1", inserter.calls)
	}
}

// Both freshness bounds are inclusive: a gap equal to the threshold is
// fresh, and an anchor frozen for exactly the dormancy horizon is still
// dormant. One ledger past either bound rejects.
func TestRefresher_StaleComponentBoundsAreInclusive(t *testing.T) {
	const minComp = 50_000_000
	tick := func(r *Refresher, ledgers *mutableLedgers, gap uint32) OutcomeKind {
		ledgers.ledger = minComp + gap
		return r.Tick(context.Background()).Kind
	}
	newRefresher := func() (*Refresher, *mutableLedgers) {
		ledgers := &mutableLedgers{observedAt: time.Unix(1_770_000_000, 0).UTC()}
		return NewRefresher(ledgers, dynComputer{assetKey: "XLM", minComponentLedger: minComp},
			&stubInserter{}, discardLogger(),
			WithStaleComponentLedgers(1000), WithMaxDormantComponentLedgers(2000)), ledgers
	}

	r, ledgers := newRefresher()
	if got := tick(r, ledgers, 1000); got != OutcomeKindOK {
		t.Errorf("gap == threshold: kind=%s want %s", got, OutcomeKindOK)
	}
	r, ledgers = newRefresher()
	if got := tick(r, ledgers, 1001); got != OutcomeKindStaleComponent {
		t.Errorf("gap == threshold+1 on cold start: kind=%s want %s", got, OutcomeKindStaleComponent)
	}
	if got := tick(r, ledgers, 2000); got != OutcomeKindDormant {
		t.Errorf("frozen for exactly the horizon: kind=%s want %s", got, OutcomeKindDormant)
	}
	if got := tick(r, ledgers, 2001); got != OutcomeKindStaleComponent {
		t.Errorf("frozen one ledger past the horizon: kind=%s want %s", got, OutcomeKindStaleComponent)
	}
}

// seqComputer returns one total per Compute call, in order.
type seqComputer struct {
	totals []int64
	i      int
}

func (s *seqComputer) Compute(_ context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	total := big.NewInt(s.totals[s.i])
	s.i++
	return Supply{
		AssetKey:          "CODE:GISSUER",
		TotalSupply:       total,
		CirculatingSupply: new(big.Int).Set(total),
		LedgerSequence:    ledger,
		ObservedAt:        observedAt,
	}, nil
}

func TestRefresher_WriteBandFlagsTenfoldMoveButStillWrites(t *testing.T) {
	cases := []struct {
		name   string
		totals []int64
		want   []string
	}{
		{"up just past 10x", []int64{1_000_000, 10_000_001}, []string{"", "up"}},
		{"up just inside 10x", []int64{1_000_000, 9_999_999}, []string{"", ""}},
		{"exactly 10x", []int64{1_000_000, 10_000_000}, []string{"", ""}},
		{"down past 1/10", []int64{1_000_000, 99_999}, []string{"", "down"}},
		{"exactly 1/10", []int64{1_000_000, 100_000}, []string{"", ""}},
		{"zero previous never fires", []int64{0, 10_000_001}, []string{"", ""}},
		{"compares against the last write", []int64{1_000_000, 10_000_001, 10_000_002}, []string{"", "up", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inserter := &stubInserter{}
			r := NewRefresher(
				stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
				&seqComputer{totals: tc.totals},
				inserter,
				discardLogger(),
			)
			for i, want := range tc.want {
				out := r.Tick(context.Background())
				if out.Kind != OutcomeKindOK {
					t.Fatalf("tick %d: kind=%s, want ok; err=%v", i, out.Kind, out.Err)
				}
				if out.BandBreach != want {
					t.Errorf("tick %d: BandBreach=%q want %q", i, out.BandBreach, want)
				}
			}
			if inserter.calls != len(tc.totals) {
				t.Errorf("inserter.calls=%d want %d: a breach must not refuse the write", inserter.calls, len(tc.totals))
			}
		})
	}
}

func TestRefresher_WriteBandIgnoresFailedWrite(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		&seqComputer{totals: []int64{1_000_000, 50_000_000, 10_000_001}},
		inserter,
		discardLogger(),
	)
	r.Tick(context.Background())
	inserter.err = errors.New("boom")
	if out := r.Tick(context.Background()); out.Kind != OutcomeKindWriteError || out.BandBreach != "" {
		t.Fatalf("failed write: kind=%s breach=%q, want write_error with no breach", out.Kind, out.BandBreach)
	}
	inserter.err = nil
	if out := r.Tick(context.Background()); out.BandBreach != "up" {
		t.Fatalf("BandBreach=%q want up: the band compares against the last successful write", out.BandBreach)
	}
}

// liveArmDown is a live reserve reader whose balance read falls through
// (one reserve account unobserved) while its observer watermark is
// healthy — the shape in which the static map must not be published
// under the live basis with a fresh-looking anchor.
type liveArmDown struct{ watermark uint32 }

func (l liveArmDown) ReserveBalanceTotal(_ context.Context, _ []string, _ uint32) (*big.Int, error) {
	return nil, fmt.Errorf("%w: account GA2: not found", ErrNoObservation)
}

func (l liveArmDown) MinReserveAccountLedger(_ context.Context, _ []string, _ uint32) (uint32, error) {
	return l.watermark, nil
}

func staticArmComputer(t *testing.T) *XLMComputer {
	t.Helper()
	static, err := NewConfigReserveBalanceReader(map[string]string{"GA1": "100", "GA2": "200"}, time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("static reader: %v", err)
	}
	c, err := NewXLMComputer([]string{"GA1", "GA2"}, NewChainedReserveBalanceReader(liveArmDown{watermark: 49_999_990}, static))
	if err != nil {
		t.Fatalf("computer: %v", err)
	}
	return c
}

// A snapshot built from the static map must say so on the wire and must
// not borrow the live observer's freshness anchor.
func TestXLMCompute_StaticArmHasDistinctBasisAndNoAnchor(t *testing.T) {
	got, err := staticArmComputer(t).Compute(context.Background(), 50_000_000, time.Now())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got.Basis != BasisXLMSDFReserveExclusionStatic {
		t.Errorf("Basis = %q, want %q", got.Basis, BasisXLMSDFReserveExclusionStatic)
	}
	if got.MinComponentLedger != 0 {
		t.Errorf("MinComponentLedger = %d, want 0: a hand-entered map has no freshness anchor", got.MinComponentLedger)
	}
	want := new(big.Int).Sub(XLMTotalSupplyStroops(), big.NewInt(300))
	if got.CirculatingSupply.Cmp(want) != 0 {
		t.Errorf("circulating = %s, want %s", got.CirculatingSupply, want)
	}
}

func TestRefresher_StaticReserveArm(t *testing.T) {
	ledgers := stubLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()}

	t.Run("strict freshness refuses it", func(t *testing.T) {
		ins := &stubInserter{}
		r := NewRefresher(ledgers, staticArmComputer(t), ins, discardLogger(), WithStrictFreshnessRequired(true))
		out := r.Tick(context.Background())
		if out.Kind != OutcomeKindMissingFreshness || ins.calls != 0 {
			t.Fatalf("outcome %q, inserts %d; want missing_freshness and no insert", out.Kind, ins.calls)
		}
	})

	t.Run("permissive publishes it under a non-ok outcome", func(t *testing.T) {
		ins := &stubInserter{}
		r := NewRefresher(ledgers, staticArmComputer(t), ins, discardLogger())
		out := r.Tick(context.Background())
		if out.Kind != OutcomeKindStaticReserve || ins.calls != 1 {
			t.Fatalf("outcome %q, inserts %d; want static_reserve and one insert", out.Kind, ins.calls)
		}
	})

	t.Run("strict refuses a static basis even with an anchor", func(t *testing.T) {
		ins := &stubInserter{}
		comp := stubComputer{out: Supply{
			AssetKey:           "XLM",
			TotalSupply:        big.NewInt(1_000),
			CirculatingSupply:  big.NewInt(900),
			Basis:              BasisXLMSDFReserveExclusionStatic,
			MinComponentLedger: 49_999_999,
		}}
		r := NewRefresher(ledgers, comp, ins, discardLogger(), WithStrictFreshnessRequired(true))
		if out := r.Tick(context.Background()); out.Kind != OutcomeKindMissingFreshness || ins.calls != 0 {
			t.Fatalf("outcome %q, inserts %d; want missing_freshness and no insert", out.Kind, ins.calls)
		}
	})
}

// An expired static map must fail the tick rather than fall back.
func TestXLMCompute_ExpiredStaticArmFails(t *testing.T) {
	static, err := NewConfigReserveBalanceReader(map[string]string{"GA1": "100", "GA2": "200"},
		time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("static reader: %v", err)
	}
	static.now = func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) }
	c, err := NewXLMComputer([]string{"GA1", "GA2"}, NewChainedReserveBalanceReader(liveArmDown{}, static))
	if err != nil {
		t.Fatalf("computer: %v", err)
	}
	ins := &stubInserter{}
	r := NewRefresher(stubLedgers{ledger: 50_000_000, observedAt: time.Now()}, c, ins, discardLogger())
	out := r.Tick(context.Background())
	if out.Kind != OutcomeKindComputeError || ins.calls != 0 {
		t.Fatalf("outcome %q, inserts %d; want compute_error and no insert (err %v)", out.Kind, ins.calls, out.Err)
	}
}
