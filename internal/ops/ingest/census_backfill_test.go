package ingest

import "testing"

// TestContiguousWatermark_FreezesOnGap is the C2-14 proof for the
// census-backfill resume checkpoint: a mid-range skipped/failed ledger must
// NOT let the checkpoint stride past it. The pre-fix code checkpointed the
// last WRITTEN ledger (`lastProcessed`), which advanced straight over a gap
// — leaving a permanent substrate hole on resume. The watermark instead
// freezes at the last contiguous ledger before the first gap.
func TestContiguousWatermark_FreezesOnGap(t *testing.T) {
	t.Run("no gaps advances to the last ledger", func(t *testing.T) {
		var wm contiguousWatermark
		for seq := uint32(100); seq <= 105; seq++ {
			wm.persisted(seq)
		}
		if wm.seq != 105 {
			t.Fatalf("watermark = %d, want 105 (all persisted, no gap)", wm.seq)
		}
	})

	t.Run("mid-range gap freezes the checkpoint before it", func(t *testing.T) {
		// Ledgers 100,101 persist; 102 is skipped (read error); 103,104
		// persist. The durable checkpoint MUST be 101 (last ledger with no
		// preceding gap), so a resume re-reads from 102 (the gap) — NOT 104,
		// which would strand ledger 102 forever.
		var wm contiguousWatermark
		wm.persisted(100)
		wm.persisted(101)
		wm.gap() // ledger 102 skipped / failed
		wm.persisted(103)
		wm.persisted(104)

		if wm.seq != 101 {
			t.Fatalf("watermark = %d, want 101 — checkpoint strode past the gap at 102 (C2-14 regression)", wm.seq)
		}
		// Resume start = checkpoint + 1 must land on the gap, not beyond it.
		if resume := wm.seq + 1; resume != 102 {
			t.Fatalf("resume start = %d, want 102 (re-read the gap)", resume)
		}
	})

	t.Run("gap on the first ledger writes no checkpoint", func(t *testing.T) {
		// If the very first ledger of the run is un-persisted, seq stays 0 so
		// the caller writes no checkpoint and the existing cursor is untouched
		// — resume restarts from the same place and re-reads the gap.
		var wm contiguousWatermark
		wm.gap() // first ledger skipped
		wm.persisted(201)
		wm.persisted(202)
		if wm.seq != 0 {
			t.Fatalf("watermark = %d, want 0 (first ledger was a gap)", wm.seq)
		}
	})

	t.Run("second gap after freeze does not un-freeze", func(t *testing.T) {
		var wm contiguousWatermark
		wm.persisted(300)
		wm.gap()
		wm.persisted(302)
		wm.gap()
		wm.persisted(304)
		if wm.seq != 300 {
			t.Fatalf("watermark = %d, want 300 (frozen at first gap)", wm.seq)
		}
	})
}
