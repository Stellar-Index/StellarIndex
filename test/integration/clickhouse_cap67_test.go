//go:build integration

package integration_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
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
// Note: the guard widens
// SetCap67MovementsWatermark to take the window's lower bound, so this file
// does not compile against the unfixed signature — the wholesale-revert red
// proof lives in clickhouse_cap67_to_clamp_test.go, which drives the same
// defect through the CLI using only unchanged signatures.
func TestSetCap67MovementsWatermark_OnlyAdvancesOverAProvenWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated ledger range nothing else in the suite writes to, below the
	// suite's global-tip claimants — see the note in
	// clickhouse_cap67_to_clamp_test.go.
	const base = uint32(160_700_000)

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

// TestSetCap67MovementsWatermark_RefusesAnEventShortfall pins the event half
// of the proof: a window whose ledgers are all present but whose
// contract_events fall short of Σ soroban_event_count (a dropped or unrestored
// partition) must not advance, and advances once the events are back.
func TestSetCap67MovementsWatermark_RefusesAnEventShortfall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const base = uint32(160_710_000)
	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	closeTime := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	ledger := func(seq, events uint32) chstore.LedgerExtract {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: closeTime, SorobanEventCount: events,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		for i := range events {
			ext.Events = append(ext.Events, chstore.ContractEventRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: "dd04", EventIndex: i,
				ContractID: "CAAA", EventType: "contract", InSuccessfulCall: 1,
			})
		}
		return ext
	}
	flush := func(exts ...chstore.LedgerExtract) {
		t.Helper()
		for _, ext := range exts {
			if err := sink.Add(ctx, ext); err != nil {
				t.Fatalf("sink add ledger %d: %v", ext.Ledger.LedgerSeq, err)
			}
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}

	// Ledger base+2 declares two events that contract_events does not hold.
	shortfall := ledger(base+2, 2)
	shortfall.Events = nil
	flush(ledger(base, 0), ledger(base+1, 0), shortfall, ledger(base+3, 0))

	err = chstore.SetCap67MovementsWatermark(ctx, addr, base, base+3)
	if !errors.Is(err, chstore.ErrCap67MovementsEventShortfall) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) missing ledger %d's events = %v, want ErrCap67MovementsEventShortfall",
			base, base+3, base+2, err)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != 0 {
		t.Fatalf("watermark = %d (%v) after a REFUSED advance, want 0 (unmoved)", wm, err)
	}

	// Restore the events; the same window is now proven and advances.
	flush(ledger(base+2, 2))
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+3); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) with events restored: %v", base, base+3, err)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != base+3 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+3)
	}
}

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
			LedgerSeq: seq, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC),
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

	// An isolated ledger range nothing else in the suite writes to, kept
	// BELOW the two tests that claim the lake's global tip
	// (TestNetworkThroughput_DedupsReingestedLedger at 200M,
	// TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled at 217M) — and with a
	// close_time below theirs, since NetworkThroughput anchors its day window
	// to max(close_time) over the whole table. The assertions here need only
	// a hole above `base`, never the global maximum.
	const base = uint32(160_500_000)

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

	// Below the suite's global-tip claimants — see the note on the previous
	// test's base.
	const base = uint32(160_600_000)

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
