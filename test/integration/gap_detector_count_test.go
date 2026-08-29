//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCountDistinctLedgersSorobanEventsReadsCensus is the DB-backed
// proof of the 2026-08-28 r1 incident fix: the soroban-events density
// numerator is answered by the ledger_ingest_log census (PK range scan)
// and NOT by a scan of soroban_events. The fixture leaves soroban_events
// EMPTY and writes a census with a known number of event-carrying
// ledgers in the window; pre-fix the count is 0 (observed rows), post-
// fix it is the census count. A non-overridden target over the same
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
