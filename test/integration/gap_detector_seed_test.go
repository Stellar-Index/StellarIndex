//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFindPerSourceLedgerGapsSeedClosesWindowBoundary is the DB-backed
// proof of the CA2-A10/A11 audit fix: a gap detector cycle only scans
// [from, tip], so a writer that halts and resumes later can leave its
// last pre-halt row in one cycle's window and its first post-resume row
// in the NEXT cycle's window — two disjoint LAG-over-DISTINCT scans,
// neither of which ever sees both endpoints, so the gap is never
// reported at all. Seeding the scan with the previous cycle's
// highest-observed ledger restores the pairing across that boundary.
//
// Fixture: a writer active at ledgers 1000-1005, then silent, then
// active again at 81005-81010 (an 80,000-ledger halt). Cycle A scans
// [1000, 41000] and only sees the first burst — nothing to pair, no
// gap, correctly so (nothing is missing INSIDE that window). Cycle B
// scans [41001, 81010] and only sees the second burst: with no seed,
// the resumed rows never pair with the pre-halt rows and the 80,000-
// ledger gap is silently dropped forever, exactly as CA2-A10's
// blend_positions walkthrough describes. With B seeded from A's
// highest-observed ledger (1005, persisted as the "last present"
// cursor), the same window pairs the seed against 81005 and reports
// the gap.
func TestFindPerSourceLedgerGapsSeedClosesWindowBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	writeLedger := func(seq uint32) {
		t.Helper()
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq:         seq,
			LedgerCloseTime:   time.Date(2026, 9, 1, 0, 0, int(seq), 0, time.UTC),
			LedgerHash:        hash(seq),
			PrevLedgerHash:    hash(seq - 1),
			SorobanEventCount: 1,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}
	for _, seq := range []uint32{1000, 1001, 1002, 1003, 1004, 1005} {
		writeLedger(seq)
	}
	for _, seq := range []uint32{81005, 81006, 81007, 81008, 81009, 81010} {
		writeLedger(seq)
	}

	target := timescale.GapDetectorTarget{Source: "test-seed-src", Table: "ledger_ingest_log", LedgerColumn: "ledger_seq"}
	const minGapSize = int64(50000)

	// Cycle A: [1000, 41000]. Only the first burst is in range — no gap.
	gapsA, err := store.FindPerSourceLedgerGaps(ctx, target, 1000, 41000, minGapSize, 0)
	if err != nil {
		t.Fatalf("cycle A: %v", err)
	}
	if len(gapsA) != 0 {
		t.Fatalf("cycle A found %d gaps; want 0 (nothing missing inside [1000,41000])", len(gapsA))
	}

	// Cycle B, UNSEEDED (seed=0): reproduces today's behaviour. Only the
	// resumed burst is in range, so there is no pairing at all and the
	// 80,000-ledger halt is invisible.
	gapsBUnseeded, err := store.FindPerSourceLedgerGaps(ctx, target, 41001, 81010, minGapSize, 0)
	if err != nil {
		t.Fatalf("cycle B unseeded: %v", err)
	}
	if len(gapsBUnseeded) != 0 {
		t.Fatalf("cycle B unseeded found %d gaps; want 0 — this pins the DEFECT (a real 80,000-ledger halt reads as clean)", len(gapsBUnseeded))
	}

	// Cycle B, SEEDED with the highest ledger cycle A actually observed
	// (1005 — obtainable via MaxLedgerInWindow, exactly what the gap
	// detector persists between cycles).
	maxA, ok, err := store.MaxLedgerInWindow(ctx, target, 1000, 41000)
	if err != nil {
		t.Fatalf("MaxLedgerInWindow: %v", err)
	}
	if !ok || maxA != 1005 {
		t.Fatalf("MaxLedgerInWindow(cycle A window) = (%d,%v); want (1005,true)", maxA, ok)
	}

	gapsBSeeded, err := store.FindPerSourceLedgerGaps(ctx, target, 41001, 81010, minGapSize, maxA)
	if err != nil {
		t.Fatalf("cycle B seeded: %v", err)
	}
	if len(gapsBSeeded) != 1 {
		t.Fatalf("cycle B seeded found %d gaps; want exactly 1 (the fix must recover the boundary-spanning halt)", len(gapsBSeeded))
	}
	g := gapsBSeeded[0]
	if g.Start != 1006 || g.End != 81004 {
		t.Errorf("gap = [%d,%d]; want [1006,81004]", g.Start, g.End)
	}
	if want := int64(81004 - 1006 + 1); g.Size != want {
		t.Errorf("gap size = %d; want %d", g.Size, want)
	}
}
