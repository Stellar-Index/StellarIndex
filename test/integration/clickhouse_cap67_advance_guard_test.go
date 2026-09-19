//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSetCap67MovementsWatermark_OnlyAdvancesOverAProvenWindow pins the
// fail-closed half of the cap67 contiguity gate at the WRITE, where the
// invariant holds for every caller — Cap67Range's clamp only protects the one
// path that goes through it, and the range it resolves is resolved once per
// run while the watermark is the permanent record of what has been derived.
//
// The watermark is read back as max(thru_ledger) and the derive resumes at
// watermark+1 with no trailing re-derive, so an advance over an unproven
// ledger drops that ledger's account movements permanently and invisibly.
// Both refusals are therefore delay, not failure: the lake heals via
// ch-live-catchup and the next run re-derives the window.
//
// Proven red on the unfixed tree (revert the cap67AdvanceProven call in
// SetCap67MovementsWatermark): step 2 records base+10 over the hole at base+6
// and step 4 records base+10 over the never-derived [base+6, base+7].
//
// Note for a pre-fix reconstruction: the fix widened
// SetCap67MovementsWatermark to take the window's lower bound, so this file
// does not compile against the unfixed signature — the wholesale-revert red
// proof lives in clickhouse_cap67_to_clamp_test.go, which drives the same
// defect through the CLI using only unchanged signatures.
func TestSetCap67MovementsWatermark_OnlyAdvancesOverAProvenWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated high ledger range: this test asserts on an absolute
	// watermark, and nothing else in the suite writes here.
	const base = uint32(220_000_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+5], HOLE at base+6, present [base+7, base+10].
	var seqs []uint32
	for seq := base; seq <= base+5; seq++ {
		seqs = append(seqs, seq)
	}
	for seq := base + 7; seq <= base+10; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	readWM := func(what string) uint32 {
		t.Helper()
		wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
		if err != nil {
			t.Fatalf("Cap67MovementsWatermark (%s): %v", what, err)
		}
		return wm
	}

	// 1. A window straddling the hole must be REFUSED, and refused by kind so
	//    the follow loop can read it as "delayed", not "broken".
	err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+10)
	if !errors.Is(err, chstore.ErrCap67MovementsHole) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) over a hole at %d = %v, want ErrCap67MovementsHole",
			base, base+10, base+6, err)
	}
	if wm := readWM("after the refused hole window"); wm != 0 {
		t.Fatalf("watermark = %d after a REFUSED advance, want 0 (unmoved) — recording %d would strand "+
			"ledger %d: max(thru_ledger) never walks back and the derive resumes at watermark+1",
			wm, base+10, base+6)
	}

	// 2. Non-vacuity: the hole-free prefix of the same range DOES advance.
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+5); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) over a contiguous window: %v (the guard must pass "+
			"proven work, not refuse everything)", base, base+5, err)
	}
	if wm := readWM("after the proven window"); wm != base+5 {
		t.Fatalf("watermark = %d, want %d", wm, base+5)
	}

	// 3. A window starting above watermark+1 must be REFUSED even though the
	//    window itself is hole-free: [base+6, base+7] was never derived.
	err = chstore.SetCap67MovementsWatermark(ctx, addr, base+8, base+10)
	if !errors.Is(err, chstore.ErrCap67MovementsSkippedPrefix) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) with the watermark at %d = %v, want "+
			"ErrCap67MovementsSkippedPrefix", base+8, base+10, base+5, err)
	}
	if wm := readWM("after the refused skip"); wm != base+5 {
		t.Fatalf("watermark = %d after a REFUSED skip, want %d (unmoved)", wm, base+5)
	}

	// 4. Heal the hole; the resume window is now proven and advances.
	cap67SeedLedgers(t, ctx, addr, []uint32{base + 6})
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base+6, base+10); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) after healing: %v", base+6, base+10, err)
	}
	if wm := readWM("after healing"); wm != base+10 {
		t.Fatalf("watermark = %d, want %d", wm, base+10)
	}

	// 5. An idempotent re-derive BELOW the watermark is neither refused nor a
	//    walk-back — account_movements is a ReplacingMergeTree and operators
	//    do top up old ranges.
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+5); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) below the watermark: %v, want nil", base, base+5, err)
	}
	if wm := readWM("after a re-derive below the watermark"); wm != base+10 {
		t.Fatalf("watermark = %d after re-deriving [%d,%d], want %d (unchanged)", wm, base, base+5, base+10)
	}
}
