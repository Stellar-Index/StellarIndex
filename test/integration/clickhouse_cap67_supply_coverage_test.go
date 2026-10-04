//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestCap67SupplyCoverage_GrowsOnlyOverProvenAdjacentWindows pins the
// supply-kind range the movements note reports against the real table: no
// rows read as no range (not [0, 0] covering genesis), the first window
// starts it, adjacent windows extend it, and a window over a lake hole is
// refused without moving it.
func TestCap67SupplyCoverage_GrowsOnlyOverProvenAdjacentWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated from the other cap67 tests' ranges.
	const base = uint32(160_720_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+2], HOLE at base+3, present [base+4, base+20].
	var seqs []uint32
	for seq := base; seq <= base+20; seq++ {
		if seq != base+3 {
			seqs = append(seqs, seq)
		}
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	readRange := func(what string) (uint32, uint32, bool) {
		t.Helper()
		cov, err := chstore.Cap67MovementsCoverage(ctx, addr)
		if err != nil {
			t.Fatalf("Cap67MovementsCoverage (%s): %v", what, err)
		}
		return cov.SupplyRange()
	}
	extend := func(lo, hi uint32) error {
		t.Helper()
		return chstore.ExtendCap67SupplyCoverage(ctx, addr, lo, hi)
	}

	if from, thru, ok := readRange("empty"); ok {
		t.Fatalf("empty table: supply range = [%d,%d], want none", from, thru)
	}

	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base+4, base+20); err != nil {
		t.Fatalf("SetCap67MovementsWatermark: %v", err)
	}
	if err := extend(base+15, base+20); err != nil {
		t.Fatalf("first window: %v", err)
	}
	if err := extend(base+10, base+14); err != nil {
		t.Fatalf("adjacent window below: %v", err)
	}
	if from, thru, ok := readRange("after two windows"); !ok || from != base+10 || thru != base+20 {
		t.Fatalf("supply range = [%d,%d] ok=%v, want [%d,%d]", from, thru, ok, base+10, base+20)
	}

	if err := extend(base+2, base+9); !errors.Is(err, chstore.ErrCap67MovementsHole) {
		t.Fatalf("window over the hole at %d = %v, want ErrCap67MovementsHole", base+3, err)
	}
	if from, _, _ := readRange("after refusal"); from != base+10 {
		t.Fatalf("a refused window moved the supply floor to %d, want %d", from, base+10)
	}

	if err := extend(base+4, base+9); err != nil {
		t.Fatalf("contiguous window below: %v", err)
	}
	if from, thru, ok := readRange("after backfill"); !ok || from != base+4 || thru != base+20 {
		t.Fatalf("supply range = [%d,%d] ok=%v, want [%d,%d]", from, thru, ok, base+4, base+20)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != base+20 {
		t.Fatalf("main watermark = %d (%v), want %d: the supply rows must not move it", wm, err, base+20)
	}
}
