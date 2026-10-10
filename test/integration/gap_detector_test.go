//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCountDistinctLedgersSorobanEventsReadsCensus is the DB-backed
// proof that the soroban-events density
// numerator is answered by the ledger_ingest_log census (PK range scan)
// and NOT by a scan of soroban_events. The fixture leaves soroban_events
// EMPTY and writes a census with a known number of event-carrying
// ledgers in the window; a scan of observed rows would count 0, and the
// census count is the right one. A non-overridden target over the same
// table proves the generic path is untouched.
func TestCountDistinctLedgersSorobanEventsReadsCensus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Ledgers 1000..1009: seven carry Soroban events, three are quiet.
	// Plus one event-carrying ledger OUTSIDE the window (1010) that a
	// correct BETWEEN must exclude.
	quiet := map[uint32]bool{1002: true, 1005: true, 1008: true}
	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	for seq := uint32(1000); seq <= 1010; seq++ {
		n := 3
		if quiet[seq] {
			n = 0
		}
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq:         seq,
			LedgerCloseTime:   time.Date(2026, 8, 28, 18, 0, int(seq-1000)*5, 0, time.UTC),
			LedgerHash:        hash(seq),
			PrevLedgerHash:    hash(seq - 1),
			SorobanEventCount: n,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	var sorobanTarget timescale.GapDetectorTarget
	for _, target := range timescale.DefaultGapDetectorTargets {
		if target.Source == "soroban-events" {
			sorobanTarget = target
		}
	}
	if sorobanTarget.Table != "soroban_events" {
		t.Fatalf("soroban-events target not registered: %+v", sorobanTarget)
	}

	got, err := store.CountDistinctLedgers(ctx, sorobanTarget, 1000, 1009)
	if err != nil {
		t.Fatalf("CountDistinctLedgers(soroban-events): %v", err)
	}
	if want := int64(7); got != want {
		t.Errorf("soroban-events distinct ledgers = %d; want %d (ledger_ingest_log census: 10 in window, 3 quiet). "+
			"0 means the count still reads the (empty) soroban_events hypertable", got, want)
	}

	// Differential: the generic path over the same table, no override —
	// counts distinct ledger_seq rows in the window regardless of census.
	generic := timescale.GapDetectorTarget{Source: "census-rows", Table: "ledger_ingest_log", LedgerColumn: "ledger_seq"}
	got, err = store.CountDistinctLedgers(ctx, generic, 1000, 1009)
	if err != nil {
		t.Fatalf("CountDistinctLedgers(generic): %v", err)
	}
	if want := int64(10); got != want {
		t.Errorf("generic COUNT(DISTINCT) = %d; want %d", got, want)
	}
}

// TestFindPerSourceLedgerGapsSeedClosesWindowBoundary is the DB-backed
// proof that a gap detector cycle only scans
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

// TestFindPerSourceLedgerGapsSorobanEventsCloseTimeBound is the DB-backed
// proof that soroban_events is partitioned by ledger_close_time,
// so the gap scan bounds that column by the close times ledger_ingest_log
// records around [from, to]. The bound must stay correct when the census
// has holes (the scan runs exactly when coverage may be broken): anchors
// are the nearest logged ledgers outside the window, and a missing anchor
// leaves that side open.
func TestFindPerSourceLedgerGapsSorobanEventsCloseTimeBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 5 s per ledger: 400,000 ledgers span ~23 days, i.e. several of
	// soroban_events' 7-day chunks.
	genesis := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	closeTime := func(seq uint32) time.Time { return genesis.Add(time.Duration(seq) * 5 * time.Second) }
	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	event := func(seq uint32, at time.Time) domain.SorobanEventRow {
		return domain.SorobanEventRow{
			Ledger: seq, LedgerCloseTime: at, TxHash: hash(seq),
			ContractID: "CTEST", ContractIDHex: hash(1), TopicCount: 1,
			Topic0XDR: []byte{0}, TopicsXDR: [][]byte{{0}}, BodyXDR: []byte{0},
		}
	}

	// Events at 1000 and 201005-201006: one 200,004-ledger gap [1001, 201004].
	if err := store.InsertSorobanEventsBatch(ctx, []domain.SorobanEventRow{
		event(1000, closeTime(1000)), event(201005, closeTime(201005)), event(201006, closeTime(201006)),
	}); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}
	// Sparse census: nothing below 1000, a hole across the whole gap.
	for _, seq := range []uint32{1000, 201005, 400000} {
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq: seq, LedgerCloseTime: closeTime(seq),
			LedgerHash: hash(seq), PrevLedgerHash: hash(seq - 1), SorobanEventCount: 1,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	var target timescale.GapDetectorTarget
	for _, tg := range timescale.DefaultGapDetectorTargets {
		if tg.Table == "soroban_events" {
			target = tg
		}
	}
	if target.CloseTimeColumn == "" {
		t.Fatalf("soroban_events target has no CloseTimeColumn: %+v", target)
	}
	const minGap = int64(100000)

	assertOneGap := func(name string, from, to int64, wantStart, wantEnd int64) {
		t.Helper()
		gaps, err := store.FindPerSourceLedgerGaps(ctx, target, from, to, minGap, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(gaps) != 1 || gaps[0].Start != wantStart || gaps[0].End != wantEnd {
			t.Fatalf("%s [%d,%d]: gaps = %+v; want exactly [%d,%d]", name, from, to, gaps, wantStart, wantEnd)
		}
	}

	// Anchors exactly at the window edges: the bound is inclusive, so the
	// edge rows that bracket the gap stay in the scan.
	assertOneGap("exact anchors", 1000, 201005, 1001, 201004)
	// No logged ledger at `to` (the tip is not yet in the census): the
	// next logged ledger above it anchors the upper side.
	assertOneGap("upper anchor beyond window", 1000, 300000, 1001, 201004)
	// No logged ledger at or below `from`: lower side unbounded.
	assertOneGap("no lower anchor", 500, 201006, 1001, 201004)

	// The scan reads only rows inside the enclosing close-time window. A
	// row at ledger 100000 stamped years before its anchors is outside it;
	// were it read, it would split the gap into [1001,99999] (below
	// minGap) and [100001,201004].
	if err := store.InsertSorobanEventsBatch(ctx, []domain.SorobanEventRow{
		event(100000, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Fatalf("InsertSorobanEventsBatch(out-of-window row): %v", err)
	}
	assertOneGap("close-time bound applied", 1000, 201005, 1001, 201004)
}
