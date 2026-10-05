//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFindPerSourceLedgerGapsSorobanEventsCloseTimeBound is the DB-backed
// proof of INV-1530: soroban_events is partitioned by ledger_close_time,
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
