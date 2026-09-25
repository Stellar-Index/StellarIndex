package supply_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// fakeSnapshotReader returns canned per-key supplies. Errors come
// from a parallel map so tests can wire ErrNoSnapshot / transient
// errors per asset_key.
type fakeSnapshotReader struct {
	mu       sync.Mutex
	supplies map[string]supply.Supply
	errs     map[string]error
}

func (f *fakeSnapshotReader) LatestSupply(_ context.Context, k string) (supply.Supply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.errs[k]; ok {
		return supply.Supply{}, e
	}
	if s, ok := f.supplies[k]; ok {
		return s, nil
	}
	return supply.Supply{}, supply.ErrNoSnapshot
}

// captureEmitter records every emission so tests can assert on the
// exact gauge + counter calls.
type captureEmitter struct {
	mu          sync.Mutex
	divergences []divergenceCall
	outcomes    []outcomeCall
	// gauge mirrors the live GaugeVec: Divergence sets a series,
	// ClearDivergence deletes it.
	gauge map[string]float64
}

func gaugeSeries(classicKey string, wrapClass supply.WrapClass) string {
	return classicKey + "|" + string(wrapClass)
}

type divergenceCall struct {
	ClassicKey string
	WrapClass  supply.WrapClass
	Stroops    float64
}

type outcomeCall struct {
	Kind      supply.CrossCheckOutcomeKind
	WrapClass supply.WrapClass
}

func (c *captureEmitter) Divergence(k string, wrapClass supply.WrapClass, s float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.divergences = append(c.divergences, divergenceCall{ClassicKey: k, WrapClass: wrapClass, Stroops: s})
	if c.gauge == nil {
		c.gauge = map[string]float64{}
	}
	c.gauge[gaugeSeries(k, wrapClass)] = s
}

func (c *captureEmitter) ClearDivergence(k string, wrapClass supply.WrapClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.gauge, gaugeSeries(k, wrapClass))
}

func (c *captureEmitter) liveSeries(k string, wrapClass supply.WrapClass) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.gauge[gaugeSeries(k, wrapClass)]
	return v, ok
}

func (c *captureEmitter) Outcome(k supply.CrossCheckOutcomeKind, wrapClass supply.WrapClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, outcomeCall{Kind: k, WrapClass: wrapClass})
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCrossCheckRefresher_NoPairsIsNoOp — empty pairs slice returns
// nil and emits no metrics. No-op-when-unconfigured is the same
// pattern as the watched-set decoders (PR #411-#413).
func TestCrossCheckRefresher_NoPairsIsNoOp(t *testing.T) {
	t.Parallel()
	emitter := &captureEmitter{}
	r, err := supply.NewCrossCheckRefresher(nil, &fakeSnapshotReader{}, emitter, newSilentLogger())
	if err != nil {
		t.Fatalf("NewCrossCheckRefresher: %v", err)
	}
	if got := r.Tick(context.Background()); got != nil {
		t.Fatalf("Tick on empty pairs: got %v, want nil", got)
	}
	if len(emitter.outcomes) != 0 || len(emitter.divergences) != 0 {
		t.Fatalf("emitted on empty pairs: outcomes=%v divergences=%v", emitter.outcomes, emitter.divergences)
	}
}

// TestCrossCheckRefresher_RejectsBadInput — empty keys and duplicate
// classic_keys are operator-config bugs caught at construction.
func TestCrossCheckRefresher_RejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		pairs []supply.CrossCheckPair
	}{
		{"empty classic", []supply.CrossCheckPair{{ClassicKey: "", SACKey: "C..."}}},
		{"empty sac", []supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: ""}}},
		{"duplicate classic", []supply.CrossCheckPair{
			{ClassicKey: "USDC:G...", SACKey: "C1"},
			{ClassicKey: "USDC:G...", SACKey: "C2"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := supply.NewCrossCheckRefresher(tc.pairs, &fakeSnapshotReader{}, &captureEmitter{}, newSilentLogger())
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestCrossCheckRefresher_WithinTolerance — equal totals → "within"
// outcome + gauge=0. Pair's WrapClass is left unset (defaults to
// [supply.WrapClassPartial]); an exact match is Within under either
// class, so this doesn't exercise the class-dependent branch — see
// the WrapClass-specific tests below for that.
func TestCrossCheckRefresher_WithinTolerance(t *testing.T) {
	t.Parallel()
	classic := supply.Supply{
		AssetKey:    "USDC:G...",
		TotalSupply: big.NewInt(100_000_000_000), SACWrappedStroops: big.NewInt(100_000_000_000),
	}
	sac := supply.Supply{
		AssetKey:    "CCONTRACT",
		TotalSupply: big.NewInt(100_000_000_000),
	}
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": classic,
		"CCONTRACT": sac,
	}}
	emitter := &captureEmitter{}
	r, err := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
		reader, emitter, newSilentLogger(),
	)
	if err != nil {
		t.Fatalf("NewCrossCheckRefresher: %v", err)
	}
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within", got)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("outcomes: %v", emitter.outcomes)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("outcome wrap_class = %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassPartial)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 0 {
		t.Fatalf("divergence: %v", emitter.divergences)
	}
}

// TestCrossCheckRefresher_PartialWrapClassicExceedsSacIsBenign is the
// direct regression test for the 2026-07-08 fix (BACKLOG #59): a pair
// with the default (partial-wrap) class where classic total vastly
// exceeds SAC total — the AQUA shape (Alg-2 ≈ 86.4B, Alg-3 ≈ 0) that
// produced 8 standing false positives under the old equality compare
// — must land Within with zero divergence, NOT Over.
func TestCrossCheckRefresher_PartialWrapClassicExceedsSacIsBenign(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"AQUA:G...": {AssetKey: "AQUA:G...", TotalSupply: big.NewInt(86_400_000_000_0000000), SACWrappedStroops: big.NewInt(0)},
		"CAQUASAC":  {AssetKey: "CAQUASAC", TotalSupply: big.NewInt(0)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		// WrapClass intentionally left unset — this is the pre-fix
		// operator config shape; the fix is safe by default.
		[]supply.CrossCheckPair{{ClassicKey: "AQUA:G...", SACKey: "CAQUASAC"}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within (the AQUA false positive must be fixed)", got)
	}
	if got[0].Result.DivergenceStroops.Sign() != 0 {
		t.Fatalf("divergence stroops: got %s, want 0", got[0].Result.DivergenceStroops)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 0 {
		t.Fatalf("emitted divergence: got %v, want 0", emitter.divergences)
	}
	if emitter.divergences[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("emitted wrap_class: got %q, want %q", emitter.divergences[0].WrapClass, supply.WrapClassPartial)
	}
}

// TestCrossCheckRefresher_PartialWrapOverMintIsDiagnostic — the genuine
// violation direction for a partial-wrap pair: SAC total exceeding
// classic total is impossible under correct accounting and MUST still
// fire an Over outcome (2026-07-08 decision: "a genuine
// escrow != minted violation must still fire").
func TestCrossCheckRefresher_PartialWrapOverMintIsDiagnostic(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		// Escrow equals the SAC total, so leg 2 is evaluated and clean.
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), SACWrappedStroops: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	// 2026-08-05: over-mint is diagnostic-only (the BLND/PHO
	// false-positive class) — the outcome stays within-tolerance and
	// the gap is carried on OverMintStroops, not the paging gauge.
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within (over-mint diagnostic)", got)
	}
	if got[0].Result.OverMintStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("over-mint stroops: got %s, want 2", got[0].Result.OverMintStroops)
	}
	if got[0].Result.DivergenceStroops.Sign() != 0 {
		t.Fatalf("divergence stroops: got %s, want 0", got[0].Result.DivergenceStroops)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("outcome wrap_class: got %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassPartial)
	}
}

// A partial-wrap pair whose classic snapshot carries no escrow
// (SACWrappedStroops nil — any asset with no sac_balance_observations
// row) evaluated no paging leg. It must report unchecked and clear the
// gauge, never publish "within" with a divergence of 0.
func TestCrossCheckRefresher_PartialWrapWithoutEscrowIsUnchecked(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeUnchecked {
		t.Fatalf("Tick: got %#v, want one Unchecked", got)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeUnchecked {
		t.Fatalf("emitted outcomes %#v, want one unchecked", emitter.outcomes)
	}
	if len(emitter.divergences) != 0 {
		t.Fatalf("divergence gauge written %#v; an unchecked pair must not publish a verdict", emitter.divergences)
	}
	if _, ok := emitter.gauge[gaugeSeries("USDC:G...", supply.WrapClassPartial)]; ok {
		t.Fatal("divergence gauge series still present; want it cleared")
	}
}

// TestCrossCheckRefresher_MisalignedLedgersNeitherPassesNorPages is
// the MNY-04 guard. Each side's snapshot is read as "the LATEST for
// this asset_key" from its own per-asset refresher, so nothing makes
// them contemporaneous. A SAC snapshot 50k ledgers (~3 days) ahead of
// a stalled classic snapshot made the subset bound report an
// over-mint that never happened — a P3 page on a stale read. It must
// instead report `misaligned`: no verdict, no gauge.
func TestCrossCheckRefresher_MisalignedLedgersNeitherPassesNorPages(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		// Classic snapshot stalled at ledger 50,000,000 with a total
		// that has since grown; SAC is current at 50,050,000.
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), LedgerSequence: 50_000_000},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002), LedgerSequence: 50_050_000},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeMisaligned {
		t.Fatalf("Tick: got %#v, want one Misaligned", got)
	}
	if got[0].Err == nil {
		t.Error("Misaligned outcome carries no Err; operators need the gap in the log line")
	}
	// Neither passes nor pages: no divergence gauge emission at all —
	// recording 0 would read as "checked, agreed", recording 2 pages.
	if len(emitter.divergences) != 0 {
		t.Errorf("emitted divergence gauge on a misaligned pair: %#v", emitter.divergences)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeMisaligned {
		t.Errorf("outcomes = %#v, want one misaligned", emitter.outcomes)
	}
}

// TestCrossCheckRefresher_AlignedLedgersStillCompare — the alignment
// gate must not swallow the real check: a gap inside
// [supply.CrossCheckLedgerTolerance] still produces a verdict, and a
// genuine over-mint at aligned ledgers still pages.
func TestCrossCheckRefresher_AlignedLedgersStillCompare(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), LedgerSequence: 50_000_000, SACWrappedStroops: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002), LedgerSequence: 50_000_000 + supply.CrossCheckLedgerTolerance},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	// Still COMPARED at the ledger-tolerance boundary (that is what
	// this test pins); the over-mint result itself is diagnostic-only
	// since 2026-08-05.
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within — the boundary gap is still comparable", got)
	}
	if got[0].Result.OverMintStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("over-mint stroops: got %s, want 2", got[0].Result.OverMintStroops)
	}
}

// TestCrossCheckRefresher_FullWrapStillAlertsOnMismatch — an operator-
// attested [supply.WrapClassFull] pair keeps the ORIGINAL ADR-0011
// equality semantics: classic exceeding sac by more than tolerance
// still fires, exactly as the pre-fix behaviour did. This is the "so
// fully-wrapped tokens still alert" half of the 2026-07-08 decision.
func TestCrossCheckRefresher_FullWrapStillAlertsOnMismatch(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_000)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassFull}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("Tick: got %#v, want one Over", got)
	}
	if got[0].Result.DivergenceStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("divergence stroops: got %s, want 2", got[0].Result.DivergenceStroops)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassFull {
		t.Fatalf("outcome wrap_class: got %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassFull)
	}
}

// TestCrossCheckRefresher_PartialWrapEscrowExceedsSacFires is the leg-2
// regression test (GH-1207): since the 2026-08-05 leg-1 downgrade to
// diagnostic-only, leg 2 (classic.SACWrappedStroops ≤ sac.TotalSupply)
// is the ONLY direction that can raise
// stellarindex_supply_cross_check_divergence_stroops, yet every other
// partial-wrap fixture in this file leaves SACWrappedStroops nil, so
// CrossCheckSubsetBound's leg 2 never evaluates (SubsetBoundChecked
// stays false) through this refresher's production entry point. A
// classic snapshot recording more escrowed-in-SAC stroops than the
// SAC's own total_supply is impossible under correct accounting and
// MUST page.
func TestCrossCheckRefresher_PartialWrapEscrowExceedsSacFires(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {
			AssetKey:          "USDC:G...",
			TotalSupply:       big.NewInt(100_000_000_000),
			SACWrappedStroops: big.NewInt(100_000_000_010),
		},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_000)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("Tick: got %#v, want one Over (escrow exceeding sac_total must page)", got)
	}
	if !got[0].Result.SubsetBoundChecked {
		t.Fatal("SubsetBoundChecked = false, want true: leg 2 must have evaluated")
	}
	if got[0].Result.EscrowExcessStroops == nil || got[0].Result.EscrowExcessStroops.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("escrow excess stroops: got %v, want 10", got[0].Result.EscrowExcessStroops)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 10 {
		t.Fatalf("emitted divergence: got %v, want 10", emitter.divergences)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("outcomes: %v, want one over", emitter.outcomes)
	}
}

// TestCrossCheckRefresher_MissingSnapshot — no rows yet for either
// side → "missing_snapshot" outcome and NO gauge update (the
// bootstrap state must not look like "checked, agreed").
func TestCrossCheckRefresher_MissingSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		supplies map[string]supply.Supply
	}{
		{
			name:     "neither side has a snapshot",
			supplies: map[string]supply.Supply{},
		},
		{
			name: "only classic has a snapshot",
			supplies: map[string]supply.Supply{
				"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(1)},
			},
		},
		{
			name: "only sac has a snapshot",
			supplies: map[string]supply.Supply{
				"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(1)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeSnapshotReader{supplies: tc.supplies}
			emitter := &captureEmitter{}
			r, _ := supply.NewCrossCheckRefresher(
				[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
				reader, emitter, newSilentLogger(),
			)
			got := r.Tick(context.Background())
			if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeMissing {
				t.Fatalf("Tick: got %#v, want one Missing", got)
			}
			if len(emitter.divergences) != 0 {
				t.Fatalf("expected no gauge update on missing, got %v", emitter.divergences)
			}
		})
	}
}

// TestCrossCheckRefresher_ReadError — non-ErrNoSnapshot read failure
// surfaces as "read_error" without updating the gauge.
func TestCrossCheckRefresher_ReadError(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{
		errs: map[string]error{"USDC:G...": errors.New("connection refused")},
	}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeReadError {
		t.Fatalf("Tick: got %#v, want one ReadError", got)
	}
	if len(emitter.divergences) != 0 {
		t.Fatalf("expected no gauge update on read error, got %v", emitter.divergences)
	}
}

// TestCrossCheckRefresher_PerPairIsolation — one pair failing
// doesn't drop the next pair's cross-check.
func TestCrossCheckRefresher_PerPairIsolation(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{
		supplies: map[string]supply.Supply{
			"BTC:G...": {AssetKey: "BTC:G...", TotalSupply: big.NewInt(50), SACWrappedStroops: big.NewInt(50)},
			"CSACBTC":  {AssetKey: "CSACBTC", TotalSupply: big.NewInt(50)},
		},
		errs: map[string]error{
			"USDC:G...": errors.New("transient"),
		},
	}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{
			{ClassicKey: "USDC:G...", SACKey: "CSACUSDC"},
			{ClassicKey: "BTC:G...", SACKey: "CSACBTC"},
		},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 2 {
		t.Fatalf("Tick: want 2 outcomes, got %d", len(got))
	}
	// Sorted by ClassicKey: BTC:G... before USDC:G...
	if got[0].Pair.ClassicKey != "BTC:G..." || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("got[0]: %#v", got[0])
	}
	if got[1].Pair.ClassicKey != "USDC:G..." || got[1].Kind != supply.CrossCheckOutcomeReadError {
		t.Fatalf("got[1]: %#v", got[1])
	}
}

// TestCrossCheckRefresher_NonEvaluableOutcomeClearsGauge — a pair whose
// last tick agreed must not keep serving that agreement once it stops
// being evaluable: Prometheus re-exports a GaugeVec series on every
// scrape, so a stalled classic refresher would otherwise read as a
// healthy 0 forever while the divergence alert stays silent.
func TestCrossCheckRefresher_NonEvaluableOutcomeClearsGauge(t *testing.T) {
	t.Parallel()
	const classicKey, sacKey = "USDC:G...", "CCONTRACT"
	cases := []struct {
		name  string
		want  supply.CrossCheckOutcomeKind
		stall func(f *fakeSnapshotReader)
	}{
		{"read_error", supply.CrossCheckOutcomeReadError, func(f *fakeSnapshotReader) {
			f.errs = map[string]error{classicKey: errors.New("connection refused")}
		}},
		{"missing_snapshot", supply.CrossCheckOutcomeMissing, func(f *fakeSnapshotReader) {
			delete(f.supplies, sacKey)
		}},
		{"misaligned", supply.CrossCheckOutcomeMisaligned, func(f *fakeSnapshotReader) {
			s := f.supplies[sacKey]
			s.LedgerSequence += supply.CrossCheckLedgerTolerance + 1
			f.supplies[sacKey] = s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
				classicKey: {AssetKey: classicKey, TotalSupply: big.NewInt(100), LedgerSequence: 50_000_000, SACWrappedStroops: big.NewInt(100)},
				sacKey:     {AssetKey: sacKey, TotalSupply: big.NewInt(100), LedgerSequence: 50_000_000},
			}}
			emitter := &captureEmitter{}
			r, err := supply.NewCrossCheckRefresher(
				[]supply.CrossCheckPair{{ClassicKey: classicKey, SACKey: sacKey, WrapClass: supply.WrapClassPartial}},
				reader, emitter, newSilentLogger(),
			)
			if err != nil {
				t.Fatalf("NewCrossCheckRefresher: %v", err)
			}
			if got := r.Tick(context.Background()); got[0].Kind != supply.CrossCheckOutcomeWithin {
				t.Fatalf("first tick: got %q, want within", got[0].Kind)
			}
			if v, ok := emitter.liveSeries(classicKey, supply.WrapClassPartial); !ok || v != 0 {
				t.Fatalf("after within: series=(%v, %v), want (0, true)", v, ok)
			}

			reader.mu.Lock()
			tc.stall(reader)
			reader.mu.Unlock()

			if got := r.Tick(context.Background()); got[0].Kind != tc.want {
				t.Fatalf("second tick: got %q, want %q", got[0].Kind, tc.want)
			}
			if v, ok := emitter.liveSeries(classicKey, supply.WrapClassPartial); ok {
				t.Fatalf("after %s the gauge still serves %v — a stale verdict re-exported every scrape", tc.want, v)
			}
		})
	}
}
