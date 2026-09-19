//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// This file is the SILENT-DATA-LOSS proof for the cap67 movements watermark,
// driven through the production entry point (chops.Run — the same dispatch
// cmd/stellarindex-ops uses) rather than through a helper: the defect lives in
// how the CLI's own flags reach the range resolver, so a test that called the
// resolver directly would never see it.
//
// The defect, in two shapes of one disease — the watermark advancing to a
// ledger whose coverage was never proven:
//
//  1. `-to N` bypassed the contiguity gate. The ContiguousWatermark clamp sat
//     INSIDE the `to == 0` branch of Cap67Range, so an operator-supplied upper
//     bound was trusted verbatim: the derive read straight past a near-tip lake
//     hole and stamped the watermark above it at every window top.
//  2. `-from N` above watermark+1 advanced the watermark over ledgers the run
//     never derived at all.
//
// Either way the loss is PERMANENT and invisible: the watermark is read back as
// max(thru_ledger), the derive resumes at watermark+1 with no trailing
// re-derive, and the /movements handler floors its Postgres arm at the same
// watermark — so the skipped ledgers' classic/native movements are served by
// neither arm, forever. The raw lake heals itself via ch-live-catchup;
// account_movements never revisits.

// cap67TruncateWatermark restores the single-row watermark table to its
// pristine (never-run) state. Called BEFORE and AFTER each test here so these
// tests are order-independent, and so
// TestCap67Range_FirstRunClampsToTheLakesFirstLedger's "this is a first run"
// precondition still holds whichever of us the runner reaches first.
//
// It owns its context rather than borrowing the test's: it runs from
// t.Cleanup, which fires AFTER the test body's `defer cancel()`, so a borrowed
// context is already cancelled and the restore would silently not happen.
func cap67TruncateWatermark(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.cap67_movements_watermark`); err != nil {
		t.Fatalf("truncate cap67 watermark: %v", err)
	}
}

// cap67SeedLedgers writes the given ledger sequences through the repo's own
// sink, which is what makes them "present in the lake" for every contiguity
// query: stellar.ledgers is the per-ledger commit marker Sink.Flush writes last.
func cap67SeedLedgers(t *testing.T, ctx context.Context, addr string, seqs []uint32) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, seq := range seqs {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa03", PrevHash: "bb03", ProtocolVersion: 23, BucketListHash: "cc03",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush ledger seed: %v", err)
	}
}

// TestCap67Movements_ExplicitToIsClampedToTheContiguousTip runs the real
// subcommand with `-to` pointed ABOVE a lake hole and proves the watermark
// stops BELOW the hole.
//
// Proven red on the unfixed tree: with the ContiguousWatermark clamp back
// inside Cap67Range's `if last == 0` branch, the run derives [base, base+12]
// and the watermark reads base+12 — ledger base+6's movements are then gone
// for good.
func TestCap67Movements_ExplicitToIsClampedToTheContiguousTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated high ledger range: this test asserts on an absolute
	// watermark, and nothing else in the suite writes here.
	const base = uint32(218_000_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+5], HOLE at base+6, present [base+7, base+12] —
	// the near-tip LiveSink bounded-drop shape.
	var seqs []uint32
	for seq := base; seq <= base+5; seq++ {
		seqs = append(seqs, seq)
	}
	for seq := base + 7; seq <= base+12; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	// The operator's post-incident invocation: an explicit window straight
	// across the hole.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10),
		"-to", strconv.FormatUint(uint64(base+12), 10),
		"-write",
	}); err != nil {
		t.Fatalf("ch-cap67-movements -from %d -to %d: %v (the clamp must DELAY the derive, not fail it)",
			base, base+12, err)
	}

	wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark: %v", err)
	}
	if wm != base+5 {
		t.Fatalf("watermark after `-to %d` across a hole at %d = %d, want %d (the contiguous tip). "+
			"A watermark above %d means the derive stepped past the hole and those ledgers' account "+
			"movements are lost permanently: the next run resumes at watermark+1 and the /movements "+
			"handler floors its Postgres arm at this value.", base+12, base+6, wm, base+5, base+6)
	}

	// The resolver itself, for a direct read of the same invariant.
	start, last, err := chops.Cap67Range(ctx, addr, base, base+12, 1)
	if err != nil {
		t.Fatalf("Cap67Range: %v", err)
	}
	if start != base || last != base+5 {
		t.Fatalf("Cap67Range(from=%d, to=%d) = [%d,%d], want [%d,%d] — an explicit -to must be min()'d "+
			"against the contiguous tip, not trusted", base, base+12, start, last, base, base+5)
	}

	// Non-vacuity: heal the hole and the same invocation now advances to the
	// top of the requested window, proving the clamp tracks the lake rather
	// than refusing everything.
	cap67SeedLedgers(t, ctx, addr, []uint32{base + 6})
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-to", strconv.FormatUint(uint64(base+12), 10),
		"-write",
	}); err != nil {
		t.Fatalf("ch-cap67-movements -to %d after healing: %v", base+12, err)
	}
	healed, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (healed): %v", err)
	}
	if healed != base+12 {
		t.Fatalf("watermark after healing the hole = %d, want %d — once the lake is whole the derive "+
			"must catch up through the requested -to", healed, base+12)
	}
}

// TestCap67Movements_RefusesToAdvanceOverUnderivedLedgers is the sibling shape
// of the same defect: `-from` above watermark+1. The window itself is
// hole-free, so the contiguity clamp has nothing to say — yet stamping the
// watermark at that window's top CLAIMS the ledgers between the old watermark
// and `-from` as derived when the run never read them.
//
// Proven red on the unfixed tree: the second run returns nil and the watermark
// jumps to base+10, silently orphaning [base+4, base+5].
func TestCap67Movements_RefusesToAdvanceOverUnderivedLedgers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const base = uint32(219_000_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	var seqs []uint32
	for seq := base; seq <= base+10; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	// A normal run establishes the prefix [base, base+3].
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10),
		"-to", strconv.FormatUint(uint64(base+3), 10),
		"-write",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark: %v", err)
	}
	if wm != base+3 {
		t.Fatalf("watermark after the seed run = %d, want %d", wm, base+3)
	}

	// The over-eager resume: skip [base+4, base+5] entirely.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base+6), 10),
		"-to", strconv.FormatUint(uint64(base+10), 10),
		"-write",
	}); err == nil {
		t.Fatalf("ch-cap67-movements -from %d with the watermark at %d returned nil — it must REFUSE: "+
			"advancing claims [%d,%d] as derived when this run skipped them", base+6, base+3, base+4, base+5)
	}

	after, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (after refusal): %v", err)
	}
	if after != base+3 {
		t.Fatalf("watermark after the refused run = %d, want %d (unmoved) — a refusal that still "+
			"advances the watermark loses [%d,%d] permanently", after, base+3, base+4, base+5)
	}

	// Non-vacuity: the correct resume (no -from, i.e. watermark+1) works and
	// carries the watermark to the contiguous tip.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-to", strconv.FormatUint(uint64(base+10), 10),
		"-write",
	}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	resumed, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (resumed): %v", err)
	}
	if resumed != base+10 {
		t.Fatalf("watermark after the correct resume = %d, want %d", resumed, base+10)
	}
}
