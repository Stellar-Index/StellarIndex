package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
)

func TestProcessAndPersistCursor_ReturnsDispatcherErrorBeforeCursorWrite(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := dispatcher.New()
	events := make(chan consumer.Event)
	defer close(events)

	err := processAndPersistCursor(
		context.Background(),
		disp,
		events,
		nil,
		logger,
		invalidLedgerCloseMeta(42),
		"not-a-real-network-passphrase",
	)
	if err == nil {
		t.Fatal("expected dispatcher/build-reader error for invalid ledger meta")
	}
}

func TestRecordCursorMetric_UsesSingleLabelCardinality(t *testing.T) {
	t.Parallel()

	// The live cursor gauge is declared with one `source` label.
	// This helper exists to keep the write path pinned to that
	// cardinality and avoid reintroducing the prior panic-class
	// mismatch.
	recordCursorMetric(123)
}

func TestEmitDiscoveryDropMetricDelta_AddsOnlyNewDrops(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	before := testutil.ToFloat64(obs.DiscoveryDroppedHitsTotal)

	last := emitDiscoveryDropMetricDelta(2, 5, logger)
	if last != 5 {
		t.Fatalf("last = %d, want 5", last)
	}
	mid := testutil.ToFloat64(obs.DiscoveryDroppedHitsTotal)
	if got := mid - before; got != 3 {
		t.Fatalf("counter delta after first emit = %v, want 3", got)
	}

	last = emitDiscoveryDropMetricDelta(last, 5, logger)
	if last != 5 {
		t.Fatalf("last after no-op emit = %d, want 5", last)
	}
	after := testutil.ToFloat64(obs.DiscoveryDroppedHitsTotal)
	if got := after - mid; got != 0 {
		t.Fatalf("counter delta after second emit = %v, want 0", got)
	}
}

func invalidLedgerCloseMeta(seq uint32) sdkxdr.LedgerCloseMeta {
	component := sdkxdr.TxSetComponent{
		Type: sdkxdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
		TxsMaybeDiscountedFee: &sdkxdr.TxSetComponentTxsMaybeDiscountedFee{
			Txs: []sdkxdr.TransactionEnvelope{{}},
		},
	}
	components := []sdkxdr.TxSetComponent{component}
	return sdkxdr.LedgerCloseMeta{
		V: 1,
		V1: &sdkxdr.LedgerCloseMetaV1{
			LedgerHeader: sdkxdr.LedgerHeaderHistoryEntry{
				Header: sdkxdr.LedgerHeader{
					LedgerSeq: sdkxdr.Uint32(seq),
				},
			},
			TxSet: sdkxdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &sdkxdr.TransactionSetV1{
					Phases: []sdkxdr.TransactionPhase{{
						V:            0,
						V0Components: &components,
					}},
				},
			},
		},
	}
}

// validLedgerCloseMeta returns a minimal but XDR-ENCODABLE LCM with
// the given ledger sequence — unlike invalidLedgerCloseMeta (used
// elsewhere in this file to exercise dispatcher error paths before
// any encoding happens), this one must survive MarshalBinary because
// recordHashdb calls it directly. An empty tx-set phase list encodes
// cleanly; invalidLedgerCloseMeta's zero-value TransactionEnvelope
// inside a non-empty phase list does not (its union discriminant is
// unset, which panics deep in the generated EncodeTo — exactly the
// kind of malformed input dispatcher.ProcessLedger is meant to
// reject early, before ever reaching an encode).
func validLedgerCloseMeta(seq uint32) sdkxdr.LedgerCloseMeta {
	return sdkxdr.LedgerCloseMeta{
		V: 1,
		V1: &sdkxdr.LedgerCloseMetaV1{
			LedgerHeader: sdkxdr.LedgerHeaderHistoryEntry{
				Header: sdkxdr.LedgerHeader{
					LedgerSeq: sdkxdr.Uint32(seq),
				},
			},
			TxSet: sdkxdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &sdkxdr.TransactionSetV1{
					Phases: []sdkxdr.TransactionPhase{},
				},
			},
		},
	}
}

// TestRecordHashdb_OK confirms the happy path: Append succeeds,
// HashdbAppendTotal{"ok"} + HashdbAppendDurationSeconds{"ok"} both
// advance, and lastAppended is updated to the ledger's sequence
// (which the periodic verify sweep's window computation depends on).
func TestRecordHashdb_OK(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 500)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	before := testutil.ToFloat64(obs.HashdbAppendTotal.WithLabelValues("ok"))
	beforeDur := obstest.HistogramSampleCount(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")

	var lastAppended atomic.Uint32
	lcm := validLedgerCloseMeta(500)
	recordHashdb(db, lcm, logger, &lastAppended)

	after := testutil.ToFloat64(obs.HashdbAppendTotal.WithLabelValues("ok"))
	if after-before != 1 {
		t.Errorf("HashdbAppendTotal{ok} delta = %v, want 1", after-before)
	}
	afterDur := obstest.HistogramSampleCount(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")
	if afterDur <= beforeDur {
		t.Errorf("HashdbAppendDurationSeconds{ok} sample count did not advance (before=%d after=%d)", beforeDur, afterDur)
	}
	if got := lastAppended.Load(); got != 500 {
		t.Errorf("lastAppended = %d, want 500", got)
	}

	raw, err := lcm.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	got, err := db.Get(500)
	if err != nil {
		t.Fatalf("Get(500) after recordHashdb: %v", err)
	}
	if want := hashdb.Hash(raw); got != want {
		t.Error("hashdb.Get(500) does not match hashdb.Hash(the ledger's marshaled bytes) — recordHashdb hashed the wrong thing")
	}
}

// TestRecordHashdb_AppendErrorIsFailureTolerant is the load-bearing
// test for the CLAUDE.md-mandated contract: a hashdb write failure
// must log + count, and MUST NOT propagate or otherwise stall/fail
// ingest. Forces hashdb.Append to fail with ErrOutOfRange (ledger
// below the file's startLedger) and confirms recordHashdb returns
// normally (no panic, no return value to check — it's void by
// design) while incrementing the error outcome and leaving
// lastAppended untouched (the verify sweep must never believe a
// ledger was durably recorded when it wasn't).
func TestRecordHashdb_AppendErrorIsFailureTolerant(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 1000)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	before := testutil.ToFloat64(obs.HashdbAppendTotal.WithLabelValues("error"))
	beforeDur := obstest.HistogramSampleCount(t, obs.HashdbAppendDurationSeconds, "outcome", "error")

	var lastAppended atomic.Uint32
	// Ledger 500 is below the hashdb's startLedger (1000) — Append
	// returns hashdb.ErrOutOfRange. This must not panic or block;
	// the call below returning at all (rather than the test hanging
	// or crashing) is itself part of what's being asserted.
	recordHashdb(db, validLedgerCloseMeta(500), logger, &lastAppended)

	after := testutil.ToFloat64(obs.HashdbAppendTotal.WithLabelValues("error"))
	if after-before != 1 {
		t.Errorf("HashdbAppendTotal{error} delta = %v, want 1", after-before)
	}
	afterDur := obstest.HistogramSampleCount(t, obs.HashdbAppendDurationSeconds, "outcome", "error")
	if afterDur <= beforeDur {
		t.Errorf("HashdbAppendDurationSeconds{error} sample count did not advance (before=%d after=%d)", beforeDur, afterDur)
	}
	if got := lastAppended.Load(); got != 0 {
		t.Errorf("lastAppended = %d, want 0 (untouched — the append failed, nothing was durably recorded)", got)
	}
}

// TestOpenOrCreateHashDB_CreatesWhenMissing covers first-ever-run:
// an operator flipping cfg.HashDB.Enabled=true on a region with no
// existing hashdb file shouldn't need a separate bootstrap step.
func TestOpenOrCreateHashDB_CreatesWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist-yet.db")
	db, err := openOrCreateHashDB(path, 42)
	if err != nil {
		t.Fatalf("openOrCreateHashDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := db.StartLedger(); got != 42 {
		t.Errorf("StartLedger() = %d, want 42", got)
	}
}

// TestOpenOrCreateHashDB_OpensExisting confirms a restart (the file
// already exists from a prior run) opens the existing file rather
// than erroring or re-creating — and that the ORIGINAL startLedger
// wins, not whatever this call happened to pass (a restart passes
// the CURRENT resolved `from`, which drifts over time as the indexer
// catches up; that must never re-stamp the file's coverage floor).
func TestOpenOrCreateHashDB_OpensExisting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "drift.db")
	created, err := hashdb.Create(path, 42)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openOrCreateHashDB(path, 99999) // different startLedger — must be ignored
	if err != nil {
		t.Fatalf("openOrCreateHashDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := db.StartLedger(); got != 42 {
		t.Errorf("StartLedger() = %d, want 42 (the original Create value, not the openOrCreateHashDB call's startLedger arg)", got)
	}
}
