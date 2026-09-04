package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// fakeSupplyCursors is a DB-free supplyCursorLister.
type fakeSupplyCursors struct {
	cursors []timescale.Cursor
	err     error
}

func (f *fakeSupplyCursors) ListCursors(context.Context) ([]timescale.Cursor, error) {
	return f.cursors, f.err
}

// fakeLakeTip is a DB-free ledgerCloseTimeReader serving one canned
// landed tip, recording the bound it was asked to clamp under.
type fakeLakeTip struct {
	ledger    uint32
	closeTime time.Time
	found     bool
	err       error

	gotMaxSeq uint32
	calls     int
}

func (f *fakeLakeTip) LatestLedgerAtOrBefore(_ context.Context, maxSeq uint32) (uint32, time.Time, bool, error) {
	f.calls++
	f.gotMaxSeq = maxSeq
	if f.ledger > maxSeq {
		// The lake never returns a row past the requested bound; keep the
		// fake honest so a test can't accidentally assert on an
		// impossible reading.
		return 0, time.Time{}, false, f.err
	}
	return f.ledger, f.closeTime, f.found, f.err
}

// capturingSupplyInserter records the snapshot the Refresher wrote.
type capturingSupplyInserter struct {
	got   supply.Supply
	calls int
}

func (c *capturingSupplyInserter) InsertSupply(_ context.Context, snap supply.Supply) error {
	c.calls++
	c.got = snap
	return nil
}

// TestSupplyAggregatorLedgers_ClampsToLandedLakeTip is the 2026-09-04 r1
// regression. ingestion_cursors (Postgres, realtime) leads ClickHouse
// stellar.ledgers (CH sink) by design: measured on r1, the ledgerstream
// cursor sat at 64274510 while max(stellar.ledgers) was 64274509. The
// aggregator resolved the snapshot ledger as the cursor's own value and
// then demanded an exact stellar.ledgers row for it, so every refresh
// landing in that window failed closed as `no_ledger` — 9.9 % of all
// ticks over a 2 h window, bursty enough to push whole cohorts of
// watched assets past the per-asset error_dominant threshold together.
//
// Resolution must clamp to the newest LANDED ledger at or before the
// cursor and stamp THAT ledger's real close time, so the tick produces a
// snapshot instead of an error.
func TestSupplyAggregatorLedgers_ClampsToLandedLakeTip(t *testing.T) {
	const (
		cursorLedger = uint32(64_274_510)
		lakeLedger   = uint32(64_274_509)
	)
	lakeClose := time.Date(2026, 9, 4, 1, 15, 20, 0, time.UTC)
	lake := &fakeLakeTip{ledger: lakeLedger, closeTime: lakeClose, found: true}
	ledgers := supplyAggregatorLedgers{
		s: &fakeSupplyCursors{cursors: []timescale.Cursor{
			{Source: "ledgerstream", LastLedger: cursorLedger},
			{Source: "projector", Sub: "trades", LastLedger: lakeLedger},
		}},
		closeTimes: lake,
	}

	gotLedger, gotObservedAt, err := ledgers.LatestKnownLedger(context.Background())
	if err != nil {
		t.Fatalf("LatestKnownLedger: %v (the one-ledger landing race must clamp, not fail closed)", err)
	}
	if gotLedger != lakeLedger {
		t.Errorf("ledger = %d, want the lake's landed tip %d", gotLedger, lakeLedger)
	}
	if lake.gotMaxSeq != cursorLedger {
		t.Errorf("lake lookup bounded by %d, want the chain cursor %d", lake.gotMaxSeq, cursorLedger)
	}
	if !gotObservedAt.Equal(lakeClose) {
		t.Errorf("ObservedAt = %s, want the landed ledger's close time %s", gotObservedAt, lakeClose)
	}

	// End to end through the real Refresher: the tick must now produce an
	// `ok` snapshot stamped at the landed ledger with that ledger's real
	// close time — not a `no_ledger` outcome, and never a wall clock.
	inserter := &capturingSupplyInserter{}
	r := supply.NewRefresher(
		ledgers,
		stubSupplyComputer{out: supply.Supply{
			AssetKey:          "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
			TotalSupply:       big.NewInt(1_000_000),
			CirculatingSupply: big.NewInt(900_000),
			Basis:             supply.BasisIssuerExclusion,
		}},
		inserter,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	out := r.Tick(context.Background())
	if out.Kind != supply.OutcomeKindOK {
		t.Fatalf("Tick outcome = %q (err=%v), want %q", out.Kind, out.Err, supply.OutcomeKindOK)
	}
	if inserter.calls != 1 {
		t.Fatalf("InsertSupply calls = %d, want 1", inserter.calls)
	}
	if inserter.got.LedgerSequence != lakeLedger {
		t.Errorf("snapshot ledger_sequence = %d, want the landed ledger %d", inserter.got.LedgerSequence, lakeLedger)
	}
	if !inserter.got.ObservedAt.Equal(lakeClose) {
		t.Errorf("snapshot ObservedAt = %s, want the landed ledger's close time %s", inserter.got.ObservedAt, lakeClose)
	}
	if time.Since(inserter.got.ObservedAt) < time.Hour {
		t.Errorf("snapshot ObservedAt %s is suspiciously close to now — resolution stamped wall-clock instead of the ledger close time", inserter.got.ObservedAt)
	}
}

// TestSupplyAggregatorLedgers_FailsClosedOnEmptyLake pins the fail-closed
// half: no landed row at or before the cursor means the lake is empty or
// wholly gapped, and resolution must error (retryable `no_ledger`) rather
// than fall back to a wall-clock stamp.
func TestSupplyAggregatorLedgers_FailsClosedOnEmptyLake(t *testing.T) {
	ledgers := supplyAggregatorLedgers{
		s:          &fakeSupplyCursors{cursors: []timescale.Cursor{{Source: "ledgerstream", LastLedger: 64_274_510}}},
		closeTimes: &fakeLakeTip{found: false},
	}
	if _, _, err := ledgers.LatestKnownLedger(context.Background()); err == nil {
		t.Fatal("expected an error when the lake has no row at or before the cursor — a wall-clock fallback would corrupt point-in-time supply queries")
	}

	inserter := &capturingSupplyInserter{}
	r := supply.NewRefresher(
		ledgers,
		stubSupplyComputer{out: supply.Supply{AssetKey: "XLM", TotalSupply: big.NewInt(1), CirculatingSupply: big.NewInt(1)}},
		inserter,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if out := r.Tick(context.Background()); out.Kind != supply.OutcomeKindNoLedger {
		t.Errorf("Tick outcome = %q, want %q", out.Kind, supply.OutcomeKindNoLedger)
	}
	if inserter.calls != 0 {
		t.Errorf("InsertSupply calls = %d, want 0 — an empty lake must never produce a snapshot", inserter.calls)
	}
}

// TestSupplyAggregatorLedgers_FailsClosedOnStalledLake bounds the clamp: a
// lake trailing the cursor by more than maxSupplyLakeClampLedgers is a
// stalled sink, not a landing race, and silently stamping a snapshot that
// far behind the chain would hide the stall.
func TestSupplyAggregatorLedgers_FailsClosedOnStalledLake(t *testing.T) {
	const cursorLedger = uint32(64_274_510)
	ledgers := supplyAggregatorLedgers{
		s: &fakeSupplyCursors{cursors: []timescale.Cursor{{Source: "ledgerstream", LastLedger: cursorLedger}}},
		closeTimes: &fakeLakeTip{
			ledger:    cursorLedger - maxSupplyLakeClampLedgers - 1,
			closeTime: time.Date(2026, 9, 4, 0, 30, 0, 0, time.UTC),
			found:     true,
		},
	}
	_, _, err := ledgers.LatestKnownLedger(context.Background())
	if err == nil {
		t.Fatal("expected an error when the lake trails the cursor beyond the clamp bound — a silent clamp would hide a stalled lake")
	}
	if !strings.Contains(err.Error(), "stalled lake") {
		t.Errorf("error = %q, want it to name the stalled-lake condition so the runbook split is obvious", err)
	}

	// The boundary itself still resolves: exactly maxSupplyLakeClampLedgers
	// behind is a (large) landing race, not a stall.
	atBound := supplyAggregatorLedgers{
		s: &fakeSupplyCursors{cursors: []timescale.Cursor{{Source: "ledgerstream", LastLedger: cursorLedger}}},
		closeTimes: &fakeLakeTip{
			ledger:    cursorLedger - maxSupplyLakeClampLedgers,
			closeTime: time.Date(2026, 9, 4, 0, 30, 0, 0, time.UTC),
			found:     true,
		},
	}
	got, _, err := atBound.LatestKnownLedger(context.Background())
	if err != nil {
		t.Fatalf("LatestKnownLedger at the clamp bound: %v, want it to resolve", err)
	}
	if got != cursorLedger-maxSupplyLakeClampLedgers {
		t.Errorf("ledger = %d, want %d", got, cursorLedger-maxSupplyLakeClampLedgers)
	}
}

// TestSupplyAggregatorLedgers_BoundsByTheChainCursor is C4-033 applied to
// the aggregator path. ingestion_cursors is a table of JOB positions, not
// chain positions: with the indexer behind (restart, re-derive,
// maintenance) and an operator backfilling near the tip, MAX(last_ledger)
// hands the backfill job's position to the clamp — so the snapshot is
// bounded by, and can be stamped at, a ledger no component balance was
// observed at, and the stalled-lake bound is measured against a job's
// progress rather than the chain's.
func TestSupplyAggregatorLedgers_BoundsByTheChainCursor(t *testing.T) {
	const chainLedger = uint32(63_000_000)
	lakeClose := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	lake := &fakeLakeTip{ledger: chainLedger, closeTime: lakeClose, found: true}
	ledgers := supplyAggregatorLedgers{
		s: &fakeSupplyCursors{cursors: []timescale.Cursor{
			{Source: "backfill", Sub: "63500000-63900000", LastLedger: 63_900_000},
			{Source: "ledgerstream", LastLedger: chainLedger},
			{Source: "gap-detector-high-water", Sub: "trades:sdex", LastLedger: 63_100_000},
		}},
		closeTimes: lake,
	}

	got, observedAt, err := ledgers.LatestKnownLedger(context.Background())
	if err != nil {
		t.Fatalf("LatestKnownLedger: %v", err)
	}
	if lake.gotMaxSeq != chainLedger {
		t.Errorf("lake lookup bounded by %d, want the ledgerstream chain cursor %d (the backfill job's 63900000 would bound the snapshot by a position no component balance was observed at)", lake.gotMaxSeq, chainLedger)
	}
	if got != chainLedger {
		t.Errorf("ledger = %d, want %d", got, chainLedger)
	}
	if !observedAt.Equal(lakeClose) {
		t.Errorf("ObservedAt = %s, want %s", observedAt, lakeClose)
	}
}

// TestSupplyChainCursorLedger_FallbackNamesTheJobCursor keeps the
// pre-first-run case working (an aggregator started before the indexer has
// written its ledgerstream row) and requires the fallback to NAME itself,
// so the resolution error an operator reads states that a job cursor
// supplied the bound.
func TestSupplyChainCursorLedger_FallbackNamesTheJobCursor(t *testing.T) {
	ledger, source := supplyChainCursorLedger([]timescale.Cursor{
		{Source: "backfill", Sub: "1-100", LastLedger: 100},
		{Source: "census-backfill", Sub: "sdex", LastLedger: 250},
	})
	if ledger != 250 {
		t.Errorf("ledger = %d, want the MAX fallback 250", ledger)
	}
	if !strings.Contains(source, "census-backfill/sdex") || !strings.Contains(source, "FALLBACK") {
		t.Errorf("source = %q, want it to name the job cursor AND mark itself a fallback", source)
	}

	if ledger, _ := supplyChainCursorLedger(nil); ledger != 0 {
		t.Errorf("ledger = %d for an empty cursor table, want 0 so the caller fails closed", ledger)
	}
}

// TestSupplyAggregatorLedgers_PropagatesCursorReadError — a Postgres read
// failure must surface, never resolve to a ledger.
func TestSupplyAggregatorLedgers_PropagatesCursorReadError(t *testing.T) {
	sentinel := errors.New("postgres down")
	ledgers := supplyAggregatorLedgers{
		s:          &fakeSupplyCursors{err: sentinel},
		closeTimes: &fakeLakeTip{ledger: 1, found: true},
	}
	if _, _, err := ledgers.LatestKnownLedger(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap %v", err, sentinel)
	}
}
