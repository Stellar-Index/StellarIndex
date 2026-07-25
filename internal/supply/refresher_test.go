package supply

import (
	"context"
	"errors"
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

// TestRefresher_StaleComponentRejected pins F-1236 (codex
// audit-2026-05-12): a snapshot whose MinComponentLedger lags
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
// F-1236 wave-60 (codex audit-2026-05-13) strict-mode gate:
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
// (MinComponentLedger) stays put. F-1320.
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

// TestRefresher_DormantAssetNotPermanentlyRejected pins F-1320:
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

// TestRefresher_StalledProducerStillRejected pins the OTHER side
// of the F-1320 dormant/stalled split: when MinComponentLedger is
// still CHANGING tick-over-tick but remains past the threshold
// (an observer that is progressing yet far behind, or one that
// regressed), the gate still rejects — we only accept when the
// component ledger is demonstrably frozen (dormant).
func TestRefresher_StalledProducerStillRejected(t *testing.T) {
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

// TestRefresher_PerAssetStaleComponentOverride pins F-0040
// behaviour: a known-low-activity asset (PHO governance token)
// passes the gate at a more permissive threshold while the
// global default still rejects high-activity assets at the same
// component lag.
//
// Real r1 measurement (aggregator journal 2026-05-26T00:25 +02:00):
// PHO supply rows lagged by gap=1190 ledgers > global threshold
// of 1000. Per-asset override of 5000 (≈7 h) accepts the legitimate
// snapshot without loosening the gate for XLM.
func TestRefresher_PerAssetStaleComponentOverride(t *testing.T) {
	inserter := &stubInserter{}
	r := NewRefresher(
		stubLedgers{ledger: 50_001_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubComputer{out: Supply{
			AssetKey:           "PHO-GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6",
			TotalSupply:        big.NewInt(1_000_000),
			CirculatingSupply:  big.NewInt(900_000),
			Basis:              BasisXLMSDFReserveExclusion,
			LedgerSequence:     50_001_500,
			MinComponentLedger: 50_000_310, // gap = 1190 ledgers
		}},
		inserter,
		discardLogger(),
		WithStaleComponentLedgers(1000), // global default — would reject
		WithStaleComponentLedgersFor("PHO-GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6", 5000),
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
		WithStaleComponentLedgersFor("PHO-GDSTRSHXNGB2NW242WXEPSGRDEABYPMKZWNVTHEMSPZ3K4FPSU7XKZE6", 5000),
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
// R-002 (MNY-04, audit-2026-07-23): the F-1320 dormancy carve-out
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
// horizon a quiet asset is still accepted (F-1320 stays fixed), but
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
	// horizon: accepted as dormant. This half guards F-1320 — the fix
	// must bound the carve-out, not delete it.
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
// pre-R-002 posture for deployments that knowingly watch assets
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
