// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeProtoEvent is a non-trade, non-sep41 consumer.Event so drainAndWrite
// routes it through the HandleEvent path (the RA-1 discarded-error site).
type fakeProtoEvent struct{ src string }

func (fakeProtoEvent) EventKind() string { return "test.proto" }
func (e fakeProtoEvent) Source() string  { return e.src }

// okWriter returns an eventWriter whose every insert succeeds; individual
// tests override the fields they want to fail.
func okWriter() eventWriter {
	return eventWriter{
		batchTrades: func(context.Context, []canonical.Trade) error { return nil },
		insertTrade: func(context.Context, canonical.Trade) error { return nil },
		copyXfer:    func(context.Context, []timescale.SEP41TransferRow) error { return nil },
		insertXfer:  func(context.Context, timescale.SEP41TransferRow) error { return nil },
		copySup:     func(context.Context, []timescale.SEP41SupplyEvent) error { return nil },
		insertSup:   func(context.Context, timescale.SEP41SupplyEvent) error { return nil },
		handle:      func(context.Context, consumer.Event) error { return nil },
	}
}

var errInsert = errors.New("insert failed")

// TestDrainAndWriteCountsOnlyWritten pins RA-1: an event whose insert fails
// must NOT be counted in written[] and MUST appear in failed[], so the
// completion report + exit code cannot claim a partially-failed recovery as
// complete. Proven red against the pre-fix behavior (unconditional written++
// and `_ = HandleEvent`).
func TestDrainAndWriteCountsOnlyWritten(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	t.Run("HandleEvent failure counts as failed, not written", func(t *testing.T) {
		buf := []consumer.Event{
			fakeProtoEvent{src: "reflector"},
			fakeProtoEvent{src: "reflector"},
			fakeProtoEvent{src: "reflector"},
		}
		w := okWriter()
		w.handle = func(context.Context, consumer.Event) error { return errInsert }

		written, failed := drainAndWrite(ctx, logger, w, buf, true)
		if written["reflector"] != 0 {
			t.Errorf("written[reflector] = %d, want 0 (all inserts failed)", written["reflector"])
		}
		if failed["reflector"] != 3 {
			t.Errorf("failed[reflector] = %d, want 3", failed["reflector"])
		}
	})

	t.Run("HandleEvent success counts as written", func(t *testing.T) {
		buf := []consumer.Event{fakeProtoEvent{src: "reflector"}, fakeProtoEvent{src: "reflector"}}
		written, failed := drainAndWrite(ctx, logger, okWriter(), buf, true)
		if written["reflector"] != 2 {
			t.Errorf("written[reflector] = %d, want 2", written["reflector"])
		}
		if failed["reflector"] != 0 {
			t.Errorf("failed[reflector] = %d, want 0", failed["reflector"])
		}
	})

	t.Run("trade batch + per-row fallback both fail => failed, not written", func(t *testing.T) {
		buf := []consumer.Event{storableTradeEvent(t, 1, 0), storableTradeEvent(t, 1, 1)}
		w := okWriter()
		w.batchTrades = func(context.Context, []canonical.Trade) error { return errInsert }
		w.insertTrade = func(context.Context, canonical.Trade) error { return errInsert }

		written, failed := drainAndWrite(ctx, logger, w, buf, true)
		src := soroswap.SourceName
		if written[src] != 0 {
			t.Errorf("written[%s] = %d, want 0 (batch + per-row both failed)", src, written[src])
		}
		if failed[src] != 2 {
			t.Errorf("failed[%s] = %d, want 2", src, failed[src])
		}
	})

	t.Run("trade batch fails but per-row fallback succeeds => written", func(t *testing.T) {
		buf := []consumer.Event{storableTradeEvent(t, 1, 0), storableTradeEvent(t, 1, 1)}
		w := okWriter()
		w.batchTrades = func(context.Context, []canonical.Trade) error { return errInsert }
		// insertTrade stays nil (succeeds)
		written, failed := drainAndWrite(ctx, logger, w, buf, true)
		src := soroswap.SourceName
		if written[src] != 2 {
			t.Errorf("written[%s] = %d, want 2 (per-row fallback recovered)", src, written[src])
		}
		if failed[src] != 0 {
			t.Errorf("failed[%s] = %d, want 0", src, failed[src])
		}
	})

	t.Run("dry-run counts every event as would-write and persists nothing", func(t *testing.T) {
		buf := []consumer.Event{fakeProtoEvent{src: "reflector"}, storableTradeEvent(t, 1, 0)}
		handleCalls := 0
		w := okWriter()
		w.handle = func(context.Context, consumer.Event) error { handleCalls++; return nil }
		written, failed := drainAndWrite(ctx, logger, w, buf, false)
		if handleCalls != 0 {
			t.Errorf("handle called %d times in dry-run, want 0", handleCalls)
		}
		if written["reflector"] != 1 || written[soroswap.SourceName] != 1 {
			t.Errorf("dry-run written = %v, want each source counted once", written)
		}
		if len(failed) != 0 {
			t.Errorf("dry-run failed = %v, want empty", failed)
		}
	})
}

// storableTradeEvent is a soroswap trade that passes canonical.Trade.Validate,
// i.e. one the served trades tier would actually hold.
func storableTradeEvent(t *testing.T, ledger, op uint32) soroswap.TradeEvent {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	btc, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(xlm, btc)
	if err != nil {
		t.Fatal(err)
	}
	return soroswap.TradeEvent{Trade: canonical.Trade{
		Source:      soroswap.SourceName,
		Ledger:      ledger,
		TxHash:      fmt.Sprintf("%064x", uint64(ledger)<<32|uint64(op)),
		OpIndex:     op,
		Timestamp:   time.Unix(1_700_000_000+int64(ledger)*5, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(10)),
		QuoteAmount: canonical.NewAmount(big.NewInt(20)),
	}}
}

// oneSideZeroTradeEvent is the SDEX-style fill whose quote leg rounded to 0:
// it passes Validate and the store persists it.
func oneSideZeroTradeEvent(t *testing.T, ledger, op uint32) soroswap.TradeEvent {
	t.Helper()
	ev := storableTradeEvent(t, ledger, op)
	ev.Trade.QuoteAmount = canonical.NewAmount(big.NewInt(0))
	return ev
}

// negativeTradeEvent is invalid: a decoder bug the run must report as failed.
func negativeTradeEvent(t *testing.T, ledger, op uint32) soroswap.TradeEvent {
	t.Helper()
	ev := storableTradeEvent(t, ledger, op)
	ev.Trade.BaseAmount = canonical.NewAmount(big.NewInt(-1))
	return ev
}

// TestDrainAndWriteCountsOneSideZeroAsWritten pins that a one-side-zero fill
// is tallied as written (the store persists it) on every path, and that a
// Validate-failing trade is tallied as failed.
func TestDrainAndWriteCountsOneSideZeroAsWritten(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	src := soroswap.SourceName
	buf := []consumer.Event{
		storableTradeEvent(t, 1, 0),
		oneSideZeroTradeEvent(t, 1, 1),
		storableTradeEvent(t, 2, 0),
		negativeTradeEvent(t, 2, 1),
	}

	cases := []struct {
		name       string
		batchErr   error
		write      bool
		wantPerRow int // insertTrade calls
	}{
		{name: "batch succeeds", write: true},
		{name: "batch fails, per-row fallback", batchErr: errInsert, write: true, wantPerRow: 4},
		{name: "dry-run predicts the same split"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := okWriter()
			w.batchTrades = func(context.Context, []canonical.Trade) error { return tc.batchErr }
			perRow := 0
			w.insertTrade = func(_ context.Context, tr canonical.Trade) error {
				perRow++
				return tr.Validate()
			}
			written, failed := drainAndWrite(ctx, logger, w, buf, tc.write)
			if written[src] != 3 || failed[src] != 1 {
				t.Errorf("written=%d failed=%d, want 3/1 (the one-side-zero fill is written)", written[src], failed[src])
			}
			if perRow != tc.wantPerRow {
				t.Errorf("insertTrade calls = %d, want %d", perRow, tc.wantPerRow)
			}
		})
	}
}

// TestDrainAndWriteCutsTradeBatchesOnLedgerBoundary pins that no ledger's
// trades are split across two batches. The bulk COPY writer's emptiness probe
// is `ledger BETWEEN min AND max`; a ledger split across batches N and N+1
// makes N+1 find N's tail and refuse the COPY on every batch.
func TestDrainAndWriteCutsTradeBatchesOnLedgerBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	perLedger := []struct{ ledger, n uint32 }{
		{1, upsertTradeBatchN * 6 / 10},
		{2, upsertTradeBatchN * 9 / 10},
		{3, upsertTradeBatchN / 5},
		{4, upsertTradeBatchN},
		{5, 1},
	}
	var buf []consumer.Event
	for _, pl := range perLedger {
		for op := range pl.n {
			buf = append(buf, storableTradeEvent(t, pl.ledger, op))
		}
	}

	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprintf("bulk=%v", bulk), func(t *testing.T) {
			var batches [][]canonical.Trade
			record := func(b []canonical.Trade) {
				batches = append(batches, append([]canonical.Trade(nil), b...))
			}
			w := okWriter()
			w.batchTrades = func(_ context.Context, b []canonical.Trade) error { record(b); return nil }
			if bulk {
				w.bulkTrades = func(_ context.Context, b []canonical.Trade) (timescale.BulkBackfillResult, error) {
					record(b)
					return timescale.BulkBackfillResult{Path: timescale.BulkBackfillPathCopy}, nil
				}
			}
			written, failed := drainAndWrite(ctx, logger, w, buf, true)
			if written[soroswap.SourceName] != len(buf) || len(failed) != 0 {
				t.Fatalf("written=%v failed=%v, want all %d written", written, failed, len(buf))
			}
			batchOf := map[uint32]int{}
			total := 0
			for i, b := range batches {
				total += len(b)
				for _, tr := range b {
					if prev, seen := batchOf[tr.Ledger]; seen && prev != i {
						t.Fatalf("ledger %d spans batches %d and %d", tr.Ledger, prev, i)
					}
					batchOf[tr.Ledger] = i
				}
			}
			if total != len(buf) {
				t.Errorf("batched %d rows, want %d", total, len(buf))
			}
		})
	}
}

// TestParseCSVList pins the -contracts flag parse: trimmed, order-preserving,
// de-duplicated, empty entries dropped. An operator pastes the affected
// contract C-strkeys as a comma list (often with stray whitespace from a
// spreadsheet), and the scoped recovery reads exactly that subset.
func TestParseCSVList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace-only", "  ,  , ", nil},
		{"single", "CBH4M45T", []string{"CBH4M45T"}},
		{
			"trimmed-and-ordered",
			" CBH4M45T , CDLZFC3S ,CCW67TSZ",
			[]string{"CBH4M45T", "CDLZFC3S", "CCW67TSZ"},
		},
		{
			"dedup-preserves-first",
			"CBH4M45T,CDLZFC3S,CBH4M45T",
			[]string{"CBH4M45T", "CDLZFC3S"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCSVList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseCSVList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestContractAllowed pins the -contracts scope gate applied in the general
// event pass (the containsStr prefilter): no override lets every contract
// through; a non-empty override admits ONLY its members, so a scoped recovery
// decodes just the affected contracts' events and skips the rest.
func TestContractAllowed(t *testing.T) {
	const (
		affected = "CBH4M45TOCKF"
		other    = "CDLZFC3SYJYD"
	)
	// No override: every contract passes (the default full-firehose behaviour).
	if !contractAllowed(nil, affected) {
		t.Error("empty override must allow all contracts")
	}
	if !contractAllowed([]string{}, other) {
		t.Error("empty override must allow all contracts")
	}
	// Override present: only listed contracts pass; the gate skips the rest.
	override := []string{affected}
	if !contractAllowed(override, affected) {
		t.Errorf("override %v must admit its own member %q", override, affected)
	}
	if contractAllowed(override, other) {
		t.Errorf("override %v must skip non-member %q (prefilter not applied)", override, other)
	}
}

// TestDropReconSources pins the de-dup guard added alongside the
// 2026-07-11 sep41 catalogue promotion: buildReconciliationCatalogue now
// hands ch-rebuild a `cat` that may already carry sep41_transfers/
// sep41_supply (when a watched set is configured), but ch-rebuild's
// -sep41 pass folds in its OWN freshly-built sep41Cat afterward — so the
// stale, promoted copies must be dropped first, or the final report loop
// (keyed by written[src.name]) double-prints and double-totals them.
// Order of the surviving entries must be preserved (report output order
// is otherwise stable and operator-relied-on).
func TestDropReconSources(t *testing.T) {
	cat := []reconSource{
		{name: "soroswap"},
		{name: "sep41_transfers"},
		{name: "phoenix"},
		{name: "sep41_supply"},
		{name: "sdex"},
	}
	got := dropReconSources(cat, "sep41_transfers", "sep41_supply")
	want := []string{"soroswap", "phoenix", "sdex"}
	if len(got) != len(want) {
		t.Fatalf("dropReconSources: got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, src := range got {
		if src.name != want[i] {
			t.Errorf("dropReconSources[%d] = %q, want %q (order must be preserved)", i, src.name, want[i])
		}
	}

	// Names absent from cat are a no-op, not an error.
	if got := dropReconSources(cat, "nonexistent"); len(got) != len(cat) {
		t.Errorf("dropping an absent name should be a no-op: got %d entries, want %d", len(got), len(cat))
	}
}

// TestSEP41RollupResetPlan pins WHEN a -sep41 -write run resets the
// sep41_supply_rollup fold checkpoint, and for WHICH contracts. This is the
// footgun guard from incident 2026-07-06: a re-derive that rewrites
// sep41_supply_events below the worker's checkpoint must reset the fold, or the
// worker double-counts a full re-derive (KALE 2×) / undercounts a scoped
// recovery. The reset must fire ONLY when the SUPPLY source is actually being
// written, and scope to exactly the CH read set (nil = FULL/all rows, the
// -contracts override = scoped).
func TestSEP41RollupResetPlan(t *testing.T) {
	affected := []string{"CBH4M45TOCKF", "CDLZFC3SYJYD"}
	cases := []struct {
		name          string
		includeSEP41  bool
		write         bool
		supplyEnabled bool
		override      []string
		wantReset     bool
		wantContracts []string
	}{
		{"dry-run never resets", true, false, true, nil, false, nil},
		{"non-sep41 run never resets", false, true, true, nil, false, nil},
		{"transfers-only (supply not enabled) never resets", true, true, false, nil, false, nil},
		{"full re-derive resets ALL rows (nil scope)", true, true, true, nil, true, nil},
		{"scoped recovery resets only the override", true, true, true, affected, true, affected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset, contracts := sep41RollupResetPlan(tc.includeSEP41, tc.write, tc.supplyEnabled, tc.override)
			if reset != tc.wantReset {
				t.Errorf("reset = %v, want %v", reset, tc.wantReset)
			}
			if !reflect.DeepEqual(contracts, tc.wantContracts) {
				t.Errorf("contracts = %v, want %v", contracts, tc.wantContracts)
			}
		})
	}
}

// ─── checkCHRebuildLiveOverlap: the ADR-0048 D3 contract on ch-rebuild ────
//
// The hazard the guard removes: `ch-rebuild -sources <projected> -to <tip>
// -write` is a SECOND writer of a domain ADR-0031/0032 gives the projector
// alone, and it stamps a positive derive_generation so its rows win the
// upsert over the live projector's. projected-rebuild has refused that
// since ADR-0048 D3; ch-rebuild did not (grep `cursor` in ch_rebuild.go
// before this change: zero hits), and a guard on one of two bulk writers
// is not a guard. Scripted cursor, mirroring TestCheckLiveCursorGuard.
func TestCheckCHRebuildLiveOverlap(t *testing.T) {
	// The live projector is at 63,000,000 for aquarius, is still behind at
	// 61,000,000 for blend, and has never run for rozo.
	cursors := map[string]uint32{"aquarius": 63_000_000, "blend": 61_000_000}
	read := func(source string) (uint32, bool, error) {
		last, ok := cursors[source]
		return last, ok, nil
	}
	cases := []struct {
		name         string
		sources      []string
		to           uint32
		allowOverlap bool
		wantErr      bool
		wantInMsg    []string
	}{
		{name: "live-above-to: allowed", sources: []string{"aquarius"}, to: 62_894_000},
		{name: "live-exactly-at-to: allowed (boundary)", sources: []string{"aquarius"}, to: 63_000_000},
		{
			name: "live-below-to: refused", sources: []string{"blend"}, to: 62_894_000,
			wantErr: true, wantInMsg: []string{"blend", "61000000", "62894000", "allow-live-overlap"},
		},
		{
			name: "no-cursor-at-all: refused by default", sources: []string{"rozo"}, to: 62_894_000,
			wantErr: true, wantInMsg: []string{"rozo", "never run"},
		},
		{
			// The r1 shape: one lagging source among several current ones
			// must refuse the whole run, and name the lagging one.
			name: "mixed set refuses on the one lagging source", sources: []string{"aquarius", "blend"}, to: 62_894_000,
			wantErr: true, wantInMsg: []string{"blend"},
		},
		{name: "override allows the overlap", sources: []string{"blend", "rozo"}, to: 62_894_000, allowOverlap: true},
		{name: "no projected sources in the run: nothing to guard", sources: nil, to: 63_500_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCHRebuildLiveOverlap(tc.sources, tc.to, tc.allowOverlap, read)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCHRebuildLiveOverlap(%v,%d,%v) = nil, want an error", tc.sources, tc.to, tc.allowOverlap)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("checkCHRebuildLiveOverlap(%v,%d,%v) = %v, want nil", tc.sources, tc.to, tc.allowOverlap, err)
				}
				return
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// A mixed run must not be allowed by its CURRENT sources: the guard reads
// every projected source in the run, not the first one that passes.
func TestCheckCHRebuildLiveOverlap_ReadsEverySource(t *testing.T) {
	var seen []string
	read := func(source string) (uint32, bool, error) {
		seen = append(seen, source)
		return 63_000_000, true, nil
	}
	if err := checkCHRebuildLiveOverlap([]string{"aquarius", "soroswap", "phoenix"}, 62_000_000, false, read); err != nil {
		t.Fatalf("all cursors above -to should pass: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{"aquarius", "soroswap", "phoenix"}) {
		t.Errorf("cursor reads = %v, want every projected source in the run", seen)
	}
}

// A cursor read that FAILS must abort the run, never be read as "no
// overlap" — the fail-closed half of the contract.
func TestCheckCHRebuildLiveOverlap_CursorReadErrorRefuses(t *testing.T) {
	boom := errors.New("connection refused")
	err := checkCHRebuildLiveOverlap([]string{"aquarius"}, 62_000_000, false, func(string) (uint32, bool, error) {
		return 0, false, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("guard error = %v, want it to wrap the cursor read failure", err)
	}
}

// projectedSourcesInRun must RESOLVE the projected set from
// projector.BuildRegistry — the same registry.go the indexer's projector
// builds from — so the guard covers a source the moment the projector
// does, and leaves the non-projected domains (sdex census, band /
// soroswap-router ContractCall) alone: nothing else writes those, so
// refusing there would be pure breakage.
func TestProjectedSourcesInRun_SplitsProjectedFromNot(t *testing.T) {
	cat := []reconSource{
		{name: "aquarius", dec: aquarius.NewDecoder()},
		{name: "soroswap", dec: soroswap.NewDecoder()},
		{name: "sdex", census: true},                              // dec == nil: op-derived census, not projected
		{name: "band", callDec: band.NewDecoder("CBANDCONTRACT")}, // ContractCall source, not projected
		// A decoder-bearing entry whose NAME the projector does not own.
		// soroswap-router is ContractCall-derived and explicitly excluded
		// from pipeline.IsProjectedEvent (AGENTS.md invariant 7), so its
		// presence in the event catalogue must not arm the guard: only
		// the registry lookup can tell these apart, a name list cannot.
		{name: soroswap_router.SourceName, dec: soroswap.NewDecoder()},
		{name: "phoenix", dec: phoenix.NewDecoder()},
	}
	all := func(string) bool { return true }
	got := projectedSourcesInRun(config.Config{}, cat, nil, false, all)
	want := []string{"aquarius", "soroswap", "phoenix"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("projected sources = %v, want %v", got, want)
	}

	// -sources narrows the run, and so must narrow the guard.
	only := func(name string) bool { return name == "phoenix" }
	got = projectedSourcesInRun(config.Config{}, cat, nil, false, only)
	if !reflect.DeepEqual(got, []string{"phoenix"}) {
		t.Errorf("filtered projected sources = %v, want [phoenix]", got)
	}
}

// The sep41 pair is projected only when contracts are actually WATCHED
// (projector.BuildRegistry skips them otherwise, and the dispatcher then
// writes nothing either) — so the guard's answer is config-dependent, not
// name-dependent. Pins that the resolution reads the live registry rather
// than a list someone typed here.
func TestProjectedSourcesInRun_SEP41FollowsTheWatchedSet(t *testing.T) {
	sep41Cat := []reconSource{{name: sep41transfers.SourceName}, {name: sep41supply.SourceName}}
	all := func(string) bool { return true }

	got := projectedSourcesInRun(config.Config{}, nil, sep41Cat, true, all)
	if len(got) != 0 {
		t.Errorf("with no watched contracts the projector writes no sep41 rows, so nothing to guard; got %v", got)
	}

	var cfg config.Config
	cfg.Supply.WatchedSEP41Contracts = []string{"CWATCHEDCONTRACT0000000000000000000000000000000000000000"}
	got = projectedSourcesInRun(cfg, nil, sep41Cat, true, all)
	want := []string{sep41transfers.SourceName, sep41supply.SourceName}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("watched sep41 sources = %v, want %v", got, want)
	}

	// -sep41 not passed: that pass does not run, so it is not guarded.
	got = projectedSourcesInRun(cfg, nil, sep41Cat, false, all)
	if len(got) != 0 {
		t.Errorf("sep41 pass disabled → nothing to guard; got %v", got)
	}
}
