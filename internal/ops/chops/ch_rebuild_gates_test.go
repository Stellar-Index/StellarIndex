package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// A -write re-derive across a lake hole drops the hole's served rows (ch-rebuild-projected.sh DELETEs the window first) and
// exits 0, so the write-time lake gate must refuse a range the lake does not hold contiguously, and a -to above its tip.
func TestGateCHRebuildLake_RefusesNonContiguousRange(t *testing.T) {
	stubContiguousTip(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		lo, hi uint32
		refuse bool
	}{
		{"window across the hole", topHoleLedger - 100, topHoleLedger + 100, true},
		{"from on the hole", topHoleLedger, topHoleLedger + 100, true},
		{"to above the contiguous tip", topLiveCaptureFrom, topRawLakeMax + 1, true},
		{"contiguous window below the hole", topHoleLedger - 100, topContiguousTip, false},
	} {
		err := gateCHRebuildLake(ctx, config.Config{}, "", nil, nil, tc.lo, tc.hi)
		if got := err != nil; got != tc.refuse {
			t.Errorf("%s [%d,%d]: refused=%v (err %v), want refused=%v", tc.name, tc.lo, tc.hi, got, err, tc.refuse)
		}
	}
}

// -sdex-gaps / -sdex-reconcile only take effect inside the -sdex pass: without it the documented recovery exits 0 having
// re-derived nothing, and with both the gaps pass silently wins.
func TestCHRebuild_RefusesSDEXModeFlagsThePassWouldIgnore(t *testing.T) {
	t.Parallel()
	for _, extra := range [][]string{
		{"-sdex-gaps"},
		{"-sdex-reconcile"},
		{"-sources", "sdex", "-sdex-gaps", "-write"},
		{"-sdex", "-sdex-gaps", "-sdex-reconcile"},
	} {
		err := chRebuild(chRebuildArgs(t, extra...))
		if err == nil || !strings.Contains(err.Error(), "-sdex-gaps and -sdex-reconcile") {
			t.Errorf("%v: err = %v, want the SDEX mode-flag refusal before the config load", extra, err)
		}
	}
	if err := chRebuild(chRebuildArgs(t, "-sdex", "-sdex-gaps")); err == nil || !strings.Contains(err.Error(), "absent.toml") {
		t.Errorf("-sdex -sdex-gaps is valid and should reach the config load, got: %v", err)
	}
}
