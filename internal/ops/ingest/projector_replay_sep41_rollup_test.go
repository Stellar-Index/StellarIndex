package ingest

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"testing"

	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
)

// fakeSEP41RollupResetter records ResetSEP41SupplyRollupFold calls so a
// test can assert whether — and with what contract scope — a replay
// reset the rollup fold.
type fakeSEP41RollupResetter struct {
	calls       int
	gotContract [][]string
	n           int64
	err         error
}

func (f *fakeSEP41RollupResetter) ResetSEP41SupplyRollupFold(_ context.Context, contractIDs []string) (int64, error) {
	f.calls++
	f.gotContract = append(f.gotContract, contractIDs)
	return f.n, f.err
}

// TestResetSEP41RollupAfterReplay_ResetsOnlyForTheSEP41SupplySource is
// the regression for finding F024 (audit 2026-09-02): a replay of the
// sep41_supply source re-drives rows a held-row retry gave up on
// (quarantined) or corrects rows already written, exactly at-or-below
// the ledger the cursor is rewound below. AdvanceSEP41SupplyRollup only
// ever folds `ledger > last_ledger`, so without a fold reset those rows
// are invisible to served SEP-41 supply forever, no matter how many
// times the replay runs. This must fold a FULL reset (nil contract
// scope), because a source-level replay re-walks every watched
// contract's events over the range, not just one.
func TestResetSEP41RollupAfterReplay_ResetsOnlyForTheSEP41SupplySource(t *testing.T) {
	f := &fakeSEP41RollupResetter{n: 7}
	reset, n, err := resetSEP41RollupAfterReplay(context.Background(), f, sep41supply.SourceName)
	if err != nil {
		t.Fatalf("resetSEP41RollupAfterReplay: %v", err)
	}
	if !reset {
		t.Fatal("want reset=true for a sep41_supply replay")
	}
	if n != 7 {
		t.Errorf("n = %d, want the resetter's reported row count 7", n)
	}
	if f.calls != 1 {
		t.Fatalf("ResetSEP41SupplyRollupFold called %d times, want exactly 1", f.calls)
	}
	if got := f.gotContract[0]; got != nil {
		t.Errorf("contract scope = %v, want nil (FULL reset — a source-level replay is not scoped to one contract)", got)
	}
}

// A replay of any OTHER source (trades sources: cctp, phoenix, blend,
// …) never touches sep41_supply_events, so resetting the SEP-41 rollup
// would be a no-op at best and a spurious "off the fast path" window for
// every watched contract at worst. It must not be called.
func TestResetSEP41RollupAfterReplay_NoopsForOtherSources(t *testing.T) {
	f := &fakeSEP41RollupResetter{}
	reset, n, err := resetSEP41RollupAfterReplay(context.Background(), f, "cctp")
	if err != nil {
		t.Fatalf("resetSEP41RollupAfterReplay: %v", err)
	}
	if reset {
		t.Error("want reset=false for a non-sep41_supply replay")
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if f.calls != 0 {
		t.Errorf("ResetSEP41SupplyRollupFold called %d times for source=cctp, want 0", f.calls)
	}
}

func TestResetSEP41RollupAfterReplay_PropagatesResetError(t *testing.T) {
	f := &fakeSEP41RollupResetter{err: errors.New("boom")}
	reset, _, err := resetSEP41RollupAfterReplay(context.Background(), f, sep41supply.SourceName)
	if err == nil {
		t.Fatal("want the resetter's error propagated")
	}
	if !reset {
		t.Error("want reset=true even on error — the caller needs to know a reset was attempted and owed")
	}
}

// TestProjectorReplay_ResetsTheSEP41RollupAfterRewinding is the wiring
// guard: resetSEP41RollupAfterReplay is only worth anything if
// projectorReplay actually reaches it after the cursor rewind lands. The
// call lives one hop down, in reportSEP41RollupReset (split out to keep
// projectorReplay under the cognitive-complexity limit), so BOTH hops are
// pinned — mirrors
// TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange's guard for
// the sibling CAGG-refresh wiring. Read from the AST rather than a list so
// the assertion cannot drift from the code it describes.
func TestProjectorReplay_ResetsTheSEP41RollupAfterRewinding(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projector.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projector.go: %v", err)
	}
	const why = "a replay of -source sep41_supply rewinds and re-walks rows the sep41_supply_rollup fold may " +
		"already have checkpointed past, and AdvanceSEP41SupplyRollup never looks back down for them, so served " +
		"SEP-41 supply stays wrong forever"
	hops := []struct {
		fn    string
		wants []string
	}{
		{"projectorReplay", []string{"reportSEP41RollupReset"}},
		{"reportSEP41RollupReset", []string{"resetSEP41RollupAfterReplay"}},
	}
	for _, hop := range hops {
		called, ok := callsInFunc(file, hop.fn)
		if !ok {
			t.Fatalf("%s is gone from projector.go — this guard has moved", hop.fn)
		}
		for _, want := range hop.wants {
			if !called[want] {
				t.Errorf("%s never calls %s — %s", hop.fn, want, why)
			}
		}
	}
}
