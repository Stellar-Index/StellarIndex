//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestContiguousWatermark_HoleAtFrom is the HIGH silent-data-loss proof for the
// CH-source projector's anti-skip guard (ADR-0034 #10). ContiguousWatermark is
// the projector's safe upper read bound: it must return from-1 (stall) when the
// lake has a hole AT the lower boundary `from`, so the projector never scans past
// the missing ledger and upserts its cursor beyond it — which would permanently
// drop that ledger's projected sole-writer sep41 mint/burn/transfer rows from the
// served tier.
//
// The bug: the watermark's interior-gap scan (leadInFrame over DISTINCT
// ledger_seq >= from) is blind to a hole exactly at `from`. When `from` is
// absent, the smallest present ledger is from+1 and {from+1, from+2, …} is
// internally contiguous, so the gap scan returns 0 ("no hole") and the pre-fix
// code returned chMax — advancing the projector RIGHT OVER the missing ledger.
//
// This is a real-ClickHouse test (not a query-shape assertion) because the thing
// under test IS what the SQL actually produces for a missing `from`: the fix adds
// a min(ledger_seq >= from) column, and the only way to prove the SQL yields
// min_present == from+1 (not a synthetic firstGap) for a genuine boundary hole is
// to seed one and read it back through the real engine.
//
// Proven red: with the fix reverted (min_present guard removed), the hole-at-from
// query returns chMax (from+5), not from-1.
func TestContiguousWatermark_HoleAtFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated, high ledger range so this test's rows are the global max —
	// ContiguousWatermark's ch_max is max() over the WHOLE ledgers table, and the
	// gap/min scans are scoped to >= from. Nothing else in the suite writes here.
	const from = uint32(215_000_000)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	seed := func(seq uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa00", PrevHash: "bb00", ProtocolVersion: 22, BucketListHash: "cc00",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}

	// Seed [from+1, from+5] — deliberately SKIP `from` itself so there is a hole
	// exactly at the lower boundary. The present set is internally contiguous, so
	// the interior-gap scan reports "no hole".
	for seq := from + 1; seq <= from+5; seq++ {
		seed(seq)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush hole seed: %v", err)
	}

	// Hole at `from` ⟹ watermark must STALL at from-1, not advance to chMax.
	wm, err := chstore.ContiguousWatermark(ctx, addr, from)
	if err != nil {
		t.Fatalf("ContiguousWatermark(hole at from): %v", err)
	}
	if wm != from-1 {
		t.Fatalf("ContiguousWatermark(from=%d) with a hole AT from = %d, want %d (from-1). "+
			"A value >= from means the projector would scan past the missing ledger and drop its "+
			"sole-writer sep41 rows.", from, wm, from-1)
	}

	// Heal the hole: seed `from` itself. Now [from, from+5] is contiguous, so the
	// watermark advances past the boundary — proving the stall was caused by the
	// hole and not a blanket refusal to advance.
	seed(from)
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush heal seed: %v", err)
	}
	healed, err := chstore.ContiguousWatermark(ctx, addr, from)
	if err != nil {
		t.Fatalf("ContiguousWatermark(healed): %v", err)
	}
	if healed < from+5 {
		t.Fatalf("ContiguousWatermark(from=%d) after healing = %d, want >= %d "+
			"(the contiguous run [from, from+5] should let it advance)", from, healed, from+5)
	}
}

// TestCap67Range_StallsAtHole is the money-display correctness proof for the
// cap67 movements derive: its upper bound MUST clamp to the contiguous
// watermark, not the raw lake max. The LiveSink drops whole ledgers under
// pressure, so a near-tip hole can exist; the pre-fix derive read to
// MaxLedger and advanced its watermark PAST the hole with no trailing
// re-derive — permanently dropping that ledger's classic/native account
// movements. This proves Cap67Range now STALLS before an interior hole.
//
// Red-proof: revert the ContiguousWatermark call in Cap67Range back to
// clickhouse.MaxLedger and this fails — `last` jumps to the global max
// (>= from+10), i.e. the derive would step past the hole at from+6.
func TestCap67Range_StallsAtHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated high range, distinct from the other watermark test.
	const from = uint32(216_000_000)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	seed := func(seq uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa01", PrevHash: "bb01", ProtocolVersion: 22, BucketListHash: "cc01",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	// Present [from+1, from+5], HOLE at from+6, present [from+7, from+10] —
	// an interior hole ABOVE the resume point, exactly the near-tip drop shape.
	for seq := from + 1; seq <= from+5; seq++ {
		seed(seq)
	}
	for seq := from + 7; seq <= from+10; seq++ {
		seed(seq)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush hole seed: %v", err)
	}

	// from explicit (skip the watermark read); to=0 → resolve the contiguous tip.
	// floorLedger is unused on this path (from!=0 skips the first-run seed) —
	// pass any valid non-zero value.
	start, last, err := chops.Cap67Range(ctx, addr, from+1, 0, 1)
	if err != nil {
		t.Fatalf("Cap67Range: %v", err)
	}
	if start != from+1 {
		t.Fatalf("start = %d, want %d", start, from+1)
	}
	if last != from+5 {
		t.Fatalf("Cap67Range last = %d, want %d — the derive MUST stall before the "+
			"hole at %d, not step past it (pre-fix MaxLedger would return >= %d and "+
			"permanently drop the hole ledger's movements)", last, from+5, from+6, from+10)
	}
}
