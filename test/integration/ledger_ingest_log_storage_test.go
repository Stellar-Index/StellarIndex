//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// ledgerHashFor builds a deterministic 32-byte hash for a sequence so
// a contiguous run forms a valid chain: row[seq].prev_ledger_hash ==
// ledgerHashFor(seq-1) == row[seq-1].ledger_hash.
func ledgerHashFor(seq uint32) []byte {
	h := make([]byte, 32)
	h[0] = byte(seq)
	h[1] = byte(seq >> 8)
	h[2] = byte(seq >> 16)
	h[3] = byte(seq >> 24)
	h[31] = 0xC0
	return h
}

// TestLedgerIngestLog exercises the ADR-0033 Phase 2 substrate
// queries against real TimescaleDB: upsert (insert + update path),
// gap detection (interior + leading + trailing boundaries), hash-chain
// verification (clean + injected break), and extent.
func TestLedgerIngestLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insert := func(seq uint32) {
		t.Helper()
		row := timescale.LedgerIngestRow{
			LedgerSeq:               seq,
			LedgerCloseTime:         t0.Add(time.Duration(seq) * time.Second),
			LedgerHash:              ledgerHashFor(seq),
			PrevLedgerHash:          ledgerHashFor(seq - 1),
			SorobanEventCount:       int(seq % 7),
			ClassicTradeEffectCount: int(seq % 3),
		}
		if err := store.UpsertLedgerIngestLog(ctx, row); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	// Present: 100..109 and 113..115 — interior gap [110,112].
	for s := uint32(100); s <= 109; s++ {
		insert(s)
	}
	for s := uint32(113); s <= 115; s++ {
		insert(s)
	}

	// ─── Gaps over [100,120]: interior [110,112], trailing [116,120].
	gaps, err := store.FindLedgerIngestGaps(ctx, 100, 120)
	if err != nil {
		t.Fatalf("FindLedgerIngestGaps([100,120]): %v", err)
	}
	wantGaps := []timescale.LedgerGap{
		{Start: 110, End: 112, Size: 3},
		{Start: 116, End: 120, Size: 5},
	}
	assertGaps(t, "[100,120]", gaps, wantGaps)

	// ─── Gaps over [98,115]: leading [98,99], interior [110,112].
	gaps, err = store.FindLedgerIngestGaps(ctx, 98, 115)
	if err != nil {
		t.Fatalf("FindLedgerIngestGaps([98,115]): %v", err)
	}
	assertGaps(t, "[98,115]", gaps, []timescale.LedgerGap{
		{Start: 98, End: 99, Size: 2},
		{Start: 110, End: 112, Size: 3},
	})

	// ─── Hash chain over the present runs: clean (only adjacent pairs
	// both present are checked, so 109→110 and 112→113 boundaries are
	// not chain-checked here — that's FindLedgerIngestGaps's job).
	breaks, err := store.VerifyLedgerHashChain(ctx, 100, 115)
	if err != nil {
		t.Fatalf("VerifyLedgerHashChain: %v", err)
	}
	if len(breaks) != 0 {
		t.Errorf("clean chain: got %d breaks, want 0: %+v", len(breaks), breaks)
	}

	// ─── Inject a break by UPDATING 105's prev to a wrong value
	// (also exercises the ON CONFLICT DO UPDATE path).
	if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
		LedgerSeq:       105,
		LedgerCloseTime: t0.Add(105 * time.Second),
		LedgerHash:      ledgerHashFor(105),
		PrevLedgerHash:  ledgerHashFor(999), // wrong — does not match 104's hash
	}); err != nil {
		t.Fatalf("UpsertLedgerIngestLog(105 update): %v", err)
	}
	breaks, err = store.VerifyLedgerHashChain(ctx, 100, 115)
	if err != nil {
		t.Fatalf("VerifyLedgerHashChain (after break): %v", err)
	}
	if len(breaks) != 1 || breaks[0].LedgerSeq != 105 {
		t.Fatalf("expected exactly one break at 105, got %+v", breaks)
	}

	// ─── Extent.
	lo, hi, ok, err := store.LedgerIngestExtent(ctx)
	if err != nil {
		t.Fatalf("LedgerIngestExtent: %v", err)
	}
	if !ok || lo != 100 || hi != 115 {
		t.Errorf("LedgerIngestExtent = (%d,%d,%v), want (100,115,true)", lo, hi, ok)
	}
}

func assertGaps(t *testing.T, label string, got, want []timescale.LedgerGap) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d gaps %+v, want %d %+v", label, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: gap[%d] = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}
