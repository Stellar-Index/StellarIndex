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
		TotalSupply: big.NewInt(100_000_000_000),
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
		"AQUA:G...": {AssetKey: "AQUA:G...", TotalSupply: big.NewInt(86_400_000_000_0000000)},
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

// TestCrossCheckRefresher_PartialWrapOverMintFires — the genuine
// violation direction for a partial-wrap pair: SAC total exceeding
// classic total is impossible under correct accounting and MUST still
// fire an Over outcome (2026-07-08 decision: "a genuine
// escrow != minted violation must still fire").
func TestCrossCheckRefresher_PartialWrapOverMintFires(t *testing.T) {
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
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("Tick: got %#v, want one Over", got)
	}
	if got[0].Result.DivergenceStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("divergence stroops: got %s, want 2", got[0].Result.DivergenceStroops)
	}
	if emitter.divergences[0].Stroops != 2 {
		t.Fatalf("emitted divergence: got %v, want 2", emitter.divergences[0].Stroops)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("outcome wrap_class: got %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassPartial)
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
			"BTC:G...": {AssetKey: "BTC:G...", TotalSupply: big.NewInt(50)},
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
