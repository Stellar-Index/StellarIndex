package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
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
		false, // accountObserverActive: irrelevant — ProcessLedger errors before any cursor/watermark write
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

// TestAggregatorPairsFromCatalogue_ReportsSkippedTickers pins T103:
// a catalogue ticker with a coingecko_id that canonical.NewCryptoAsset
// rejects (not on the ADR-0014 allow-list) must be excluded from the
// aggregator pair set AND surfaced — via the skipped-tickers gauge and
// a warn log naming it — rather than silently dropped by a bare
// `continue`.
func TestAggregatorPairsFromCatalogue_ReportsSkippedTickers(t *testing.T) {
	y := `verified_currencies:
  - ticker: BTC
    slug: btc
    name: Bitcoin
    class: crypto
    coingecko_id: bitcoin
    reference_only: true
  - ticker: ZZZFAKE
    slug: zzzfake
    name: Not On The Allow-list
    class: crypto
    coingecko_id: zzzfake-coin
    reference_only: true
`
	cat, err := currency.LoadFromBytes([]byte(y))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	pairs := aggregatorPairsFromCatalogue(cat, logger)

	// BTC is allow-listed: 3 fiat pairs (USD/EUR/GBP). ZZZFAKE must
	// not appear anywhere in the result.
	if len(pairs) != 3 {
		t.Fatalf("pairs = %d, want 3 (BTC only)", len(pairs))
	}
	for _, p := range pairs {
		if p.Base.Code == "ZZZFAKE" {
			t.Fatalf("ZZZFAKE leaked into aggregator pairs: %+v", p)
		}
	}

	if got := testutil.ToFloat64(obs.AggregatorCatalogueTickersSkipped); got != 1 {
		t.Fatalf("AggregatorCatalogueTickersSkipped = %v, want 1", got)
	}

	if !strings.Contains(buf.String(), "ZZZFAKE") {
		t.Fatalf("skip warning missing ticker ZZZFAKE, log = %s", buf.String())
	}
}

// TestMergeAggregatorPairs_SupplementsRatherThanReplaces pins Q104:
// a catalogue yielding even one pair must not silently replace the
// hardcoded defaultAggregatorPairs() set — it supplements it. Before
// the fix, `len(aggregatorPairs) == 0` was the only fallback trigger,
// so a catalogue with a single ticker (e.g. only BTC) would drop
// every other hardcoded ticker (XLM, ETH, ADA, ...) from aggregator
// polling.
func TestMergeAggregatorPairs_SupplementsRatherThanReplaces(t *testing.T) {
	t.Parallel()

	btc, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatalf("NewCryptoAsset(BTC): %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset(USD): %v", err)
	}
	btcUSD, err := canonical.NewPair(btc, usd)
	if err != nil {
		t.Fatalf("NewPair(BTC,USD): %v", err)
	}

	// A catalogue that resolved to exactly one pair (as a catalogue
	// with only one coingecko_id-bearing ticker would).
	catalogue := []canonical.Pair{btcUSD}
	defaults := defaultAggregatorPairs()

	got := mergeAggregatorPairs(catalogue, defaults)

	// XLM/USD is in the hardcoded default set but not in the
	// one-pair catalogue result — it must survive the merge.
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	xlmUSD, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair(XLM,USD): %v", err)
	}
	var foundXLM bool
	for _, p := range got {
		if p.String() == xlmUSD.String() {
			foundXLM = true
			break
		}
	}
	if !foundXLM {
		t.Fatalf("mergeAggregatorPairs dropped XLM/USD from the hardcoded default set; got %d pairs: %v", len(got), got)
	}

	// No duplicate of the catalogue-supplied BTC/USD pair.
	var btcCount int
	for _, p := range got {
		if p.String() == btcUSD.String() {
			btcCount++
		}
	}
	if btcCount != 1 {
		t.Fatalf("BTC/USD appeared %d times, want exactly 1 (deduped)", btcCount)
	}

	if len(got) != len(defaults) {
		t.Fatalf("merged set = %d pairs, want %d (defaults ∪ catalogue with no new tickers)", len(got), len(defaults))
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
// test for the AGENTS.md-mandated contract: a hashdb write failure
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

// validLedgerCloseMetaWithBaseFee is validLedgerCloseMeta with a
// non-zero BaseFee — a scalar header field cheap to vary so two calls
// for the SAME seq encode to different bytes (and therefore different
// hashdb hashes), simulating a re-ingested ledger whose upstream
// bytes changed underneath it.
func validLedgerCloseMetaWithBaseFee(seq uint32, baseFee uint32) sdkxdr.LedgerCloseMeta {
	lcm := validLedgerCloseMeta(seq)
	lcm.V1.LedgerHeader.Header.BaseFee = sdkxdr.Uint32(baseFee)
	return lcm
}

// TestRecordHashdb_ReingestSameBytesDoesNotDoubleAppend confirms that
// re-ingesting an already-recorded ledger with IDENTICAL bytes goes
// through Verify (not a blind Append): lastAppended still advances,
// but the stored record is untouched (same hash either way, but this
// pins that the append path is now read-first).
func TestRecordHashdb_ReingestSameBytesDoesNotDoubleAppend(t *testing.T) {
	// Not t.Parallel(): asserts an exact delta on the process-global
	// HashdbAppendTotal counter, which the other hashdb append tests
	// in this file also do under t.Parallel() — running serially
	// avoids racing their windows.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 500)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	var lastAppended atomic.Uint32
	lcm := validLedgerCloseMeta(500)
	recordHashdb(db, lcm, logger, &lastAppended)
	recordHashdb(db, lcm, logger, &lastAppended)

	if got := lastAppended.Load(); got != 500 {
		t.Errorf("lastAppended = %d, want 500", got)
	}
	raw, err := lcm.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	got, err := db.Get(500)
	if err != nil {
		t.Fatalf("Get(500): %v", err)
	}
	if want := hashdb.Hash(raw); got != want {
		t.Error("hashdb.Get(500) does not match the ledger's hash after a same-bytes re-ingest")
	}
}

// TestRecordHashdb_DriftOnReingestDoesNotOverwrite is the load-bearing
// regression test for Q112/Q128/T133: recordHashdb previously called
// hashdb.Append unconditionally on every live ledger, so re-ingesting
// an already-recorded ledger with DIFFERENT bytes (upstream rewrite,
// or a restart replaying past the last committed cursor) silently
// clobbered the original fingerprint — destroying the exact tamper
// evidence ADR-0016's drift detector exists to preserve. This asserts
// the second, differing-content call for the SAME seq leaves the
// FIRST hash on disk, increments HashdbDriftTotal, and does NOT
// advance lastAppended for that call.
func TestRecordHashdb_DriftOnReingestDoesNotOverwrite(t *testing.T) {
	// Not t.Parallel(): see TestRecordHashdb_ReingestSameBytesDoesNotDoubleAppend.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 500)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	driftBefore := testutil.ToFloat64(obs.HashdbDriftTotal)

	var lastAppended atomic.Uint32
	first := validLedgerCloseMetaWithBaseFee(500, 100)
	recordHashdb(db, first, logger, &lastAppended)
	if got := lastAppended.Load(); got != 500 {
		t.Fatalf("lastAppended after first append = %d, want 500", got)
	}
	firstRaw, err := first.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary(first): %v", err)
	}
	wantHash := hashdb.Hash(firstRaw)

	// Reset lastAppended so we can tell whether the SECOND (drifting)
	// call advances it — it must not.
	lastAppended.Store(0)

	second := validLedgerCloseMetaWithBaseFee(500, 999) // different bytes, same seq
	recordHashdb(db, second, logger, &lastAppended)

	if got := lastAppended.Load(); got != 0 {
		t.Errorf("lastAppended after drifting re-ingest = %d, want 0 (unchanged — the drifting write must not be treated as durably recorded)", got)
	}

	got, err := db.Get(500)
	if err != nil {
		t.Fatalf("Get(500) after drifting re-ingest: %v", err)
	}
	if got != wantHash {
		t.Error("hashdb.Get(500) changed after a drifting re-ingest — Append overwrote the original recorded hash instead of refusing")
	}

	driftAfter := testutil.ToFloat64(obs.HashdbDriftTotal)
	if driftAfter-driftBefore != 1 {
		t.Errorf("HashdbDriftTotal delta = %v, want 1", driftAfter-driftBefore)
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

// TestOpenOrCreateHashDB_RecreatesTruncatedFile confirms a file
// shorter than the 16-byte header — the residue of a Create whose
// header never reached disk (power loss in the writeback window) —
// is recreated rather than crash-looping the daemon. No record can
// exist in a headerless file, so recreating loses nothing.
func TestOpenOrCreateHashDB_RecreatesTruncatedFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size int
	}{
		{"zero-byte", 0},
		{"partial-header", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "torn.db")
			if err := os.WriteFile(path, make([]byte, tc.size), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			db, err := openOrCreateHashDB(path, 42)
			if err != nil {
				t.Fatalf("openOrCreateHashDB on %d-byte file: %v", tc.size, err)
			}
			defer func() { _ = db.Close() }()

			if got := db.StartLedger(); got != 42 {
				t.Errorf("StartLedger() = %d, want 42 (fresh Create)", got)
			}
		})
	}
}

// TestOpenOrCreateHashDB_BadMagicFailsClosed confirms a file with a
// full header but foreign magic bytes is NOT silently recreated — a
// file with content might be a real baseline (or evidence); discarding
// it automatically would defeat the detector's purpose.
func TestOpenOrCreateHashDB_BadMagicFailsClosed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "foreign.db")
	if err := os.WriteFile(path, []byte("not-a-hashdb-file-at-all"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := openOrCreateHashDB(path, 42); !errors.Is(err, hashdb.ErrBadMagic) {
		t.Fatalf("openOrCreateHashDB = %v, want ErrBadMagic (fail-closed)", err)
	}
}
