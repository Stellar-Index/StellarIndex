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
	"os"
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
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
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
// complete. Red against an implementation with unconditional written++
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

// TestReportCHRebuildCountsListsEveryReDerivedSource pins that a re-derived
// source that produced no row still gets a report row and is named, while a
// source outside the run stays out of the report.
func TestReportCHRebuildCountsListsEveryReDerivedSource(t *testing.T) {
	cat := []reconSource{{name: "aquarius"}, {name: "blend"}, {name: "reflector"}}
	var out strings.Builder
	err := reportCHRebuildCounts(&out, cat, []string{"aquarius", "blend"}, nil,
		map[string]int{"aquarius": 5}, map[string]int{"aquarius": 1})
	if err == nil || !strings.Contains(err.Error(), "1 event(s) failed") {
		t.Errorf("err = %v, want the 1-failed error", err)
	}
	got := out.String()
	for _, want := range []string{
		fmt.Sprintf("%-16s %14d %14d\n", "aquarius", 5, 1),
		fmt.Sprintf("%-16s %14d %14d\n", "blend", 0, 0),
		"re-derived NO rows for: blend ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "reflector") {
		t.Errorf("report lists a source outside the run:\n%s", got)
	}
}

// TestReportCHRebuildCountsFailsARequiredSourceThatWroteNothing pins the exit
// gate for an emptied window: a -require-rows source that re-derived no row
// fails the run, even when all it produced failed to write, while an
// unrequired quiet source does not.
func TestReportCHRebuildCountsFailsARequiredSourceThatWroteNothing(t *testing.T) {
	cat := []reconSource{{name: "aquarius"}, {name: "blend"}, {name: "cctp"}}
	reDerived := []string{"aquarius", "blend", "cctp"}
	for _, tc := range []struct {
		name     string
		required []string
		written  map[string]int
		failed   map[string]int
		wantErr  string
	}{
		{"required source empty", []string{"aquarius", "blend"}, map[string]int{"aquarius": 3}, map[string]int{}, "wrote no row for blend,"},
		{"required source only failed", []string{"aquarius"}, map[string]int{}, map[string]int{"aquarius": 2}, "2 event(s) failed to write"},
		{"nothing required, all empty", nil, map[string]int{}, map[string]int{}, ""},
		{"required sources wrote", []string{"aquarius", "blend"}, map[string]int{"aquarius": 3, "blend": 1}, map[string]int{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := reportCHRebuildCounts(io.Discard, cat, reDerived, tc.required, tc.written, tc.failed)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil — a quiet unrequired source must not fail the run", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestCHRebuildUnstorableTradeFailsTheRun pins the drain-to-report path for an
// emptied window: a trade the store refuses fails the run from any source,
// while a one-side-zero fill is stored and so counts as written.
func TestCHRebuildUnstorableTradeFailsTheRun(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := soroswap.SourceName
	cat := []reconSource{{name: src}}
	for _, tc := range []struct {
		name    string
		buf     []consumer.Event
		wantErr string
	}{
		{"one-side-zero fill is written", []consumer.Event{storableTradeEvent(t, 1, 0), oneSideZeroTradeEvent(t, 1, 1)}, ""},
		{"invalid trade fails the run", []consumer.Event{storableTradeEvent(t, 1, 0), negativeTradeEvent(t, 1, 1)}, "1 event(s) failed to write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written, failed := drainAndWrite(context.Background(), logger, okWriter(), tc.buf, true)
			err := reportCHRebuildCounts(io.Discard, cat, []string{src}, []string{src}, written, failed)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil — a stored one-side-zero fill is not a lost row", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q — a refused row in an emptied window would exit 0", err, tc.wantErr)
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

// TestDropReconSources pins the de-dup guard that accompanies the
// sep41 catalogue promotion: buildReconciliationCatalogue
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
// footgun guard: a re-derive that rewrites
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
// since ADR-0048 D3; ch-rebuild must too, because a guard on one of two bulk writers
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

// TestCHRebuild_ModeFlagsFollowTheSharedWriteGate pins ch-rebuild to the
// shared -write/-dry-run convention. The named-source BackfillSafe refusal
// is armed only in write mode, so it tells the two modes apart before any
// datastore is reached; a dry run falls through to the absent config.
func TestCHRebuild_ModeFlagsFollowTheSharedWriteGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode      []string
		wantWrite bool
	}{
		{nil, false},
		{[]string{"-dry-run"}, false},
		{[]string{"-write"}, true},
		{[]string{"-dry-run", "-write"}, true},
	} {
		args := append(append([]string{}, tc.mode...), "-sources", upshift.SourceName)
		err := chRebuild(chRebuildArgs(t, args...))
		if err == nil {
			t.Fatalf("%v: chRebuild succeeded against a config that does not exist", tc.mode)
		}
		if got := strings.Contains(err.Error(), "not BackfillSafe"); got != tc.wantWrite {
			t.Errorf("%v: write-mode refusal = %v, want %v (err: %v)", tc.mode, got, tc.wantWrite, err)
		}
		if !tc.wantWrite && !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("%v: a dry run should reach the config load, got: %v", tc.mode, err)
		}
	}
}

// Every ch-rebuild pass appends into the one in-process buffer, so the
// maxBufferedRange guard must engage for an sdex-only or ContractCall-only
// invocation, not just when an event decoder is selected.
func TestCHRebuildBuffers_EveryPassEngagesTheRangeGuard(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	sep41Cat := []reconSource{{name: sep41supply.SourceName}, {name: sep41transfers.SourceName}}
	var eventSrc, callSrc string
	hasSDEX := false
	for _, src := range cat {
		switch {
		case src.dec != nil && eventSrc == "":
			eventSrc = src.name
		case src.callDec != nil && callSrc == "":
			callSrc = src.name
		case src.name == sdex.SourceName:
			hasSDEX = true
		}
	}
	if eventSrc == "" || callSrc == "" || !hasSDEX {
		t.Fatalf("catalogue lacks a source of each pass kind (event %q, call %q, sdex %v) — this test is asserting nothing",
			eventSrc, callSrc, hasSDEX)
	}
	only := func(name string) func(string) bool { return func(n string) bool { return n == name } }
	all := func(string) bool { return true }

	cases := []struct {
		name    string
		passes  chRebuildPasses
		enabled func(string) bool
		want    bool
	}{
		{"-sdex -sources sdex", chRebuildPasses{sdex: true}, only(sdex.SourceName), true},
		{"-contract-calls -sources " + callSrc, chRebuildPasses{contractCalls: true}, only(callSrc), true},
		{"-sources " + eventSrc, chRebuildPasses{}, only(eventSrc), true},
		{"-sep41", chRebuildPasses{sep41: true}, only(callSrc), true},
		{"no -sources", chRebuildPasses{}, all, true},
		{"-sources sdex without -sdex decodes nothing", chRebuildPasses{}, only(sdex.SourceName), false},
		{"-sources " + callSrc + " without -contract-calls decodes nothing", chRebuildPasses{}, only(callSrc), false},
	}
	for _, tc := range cases {
		if got := chRebuildBuffers(cat, sep41Cat, tc.passes, tc.enabled); got != tc.want {
			t.Errorf("%s: chRebuildBuffers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The predicate is only a guard if chRebuild consults it at the range check.
func TestCHRebuild_RangeGuardUsesThePassSelection(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if !strings.Contains(body, "chRebuildBuffers(cat, sep41Cat, passes, enabled) && *to-*from > maxBufferedRange") {
		t.Fatal("chRebuild's maxBufferedRange guard no longer keys on chRebuildBuffers(cat, sep41Cat, passes, enabled)")
	}
}

// sourcesTestCatalogue is a stand-in for buildReconciliationCatalogue's
// output shaped like the real one: an event source with a decoder, a
// ContractCall source, and the census-only sdex entry. Only the names
// matter to the -sources gate.
func sourcesTestCatalogue() []reconSource {
	return []reconSource{
		{name: "soroswap"},
		{name: "aquarius"},
		{name: "soroswap_router"},
		{name: "sdex", census: true},
	}
}

// TestCheckCHRebuildSources_unknownNameRefused pins the DO-NOTHING
// trap: `ch-rebuild -sources sdx` (a typo for sdex) made enabled() false
// for every real source, so the run decoded nothing, printed its report
// and exited 0 — a rebuild-of-nothing reported as success.
func TestCheckCHRebuildSources_unknownNameRefused(t *testing.T) {
	err := checkCHRebuildSources(sourcesTestCatalogue(), []string{"sdx"})
	if err == nil {
		t.Fatal("checkCHRebuildSources accepted -sources sdx — an unknown name selects nothing and the run exits 0 having re-derived nothing")
	}
	if !strings.Contains(err.Error(), "sdx") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	// The operator has to be able to see what they meant to type.
	if !strings.Contains(err.Error(), "sdex") {
		t.Errorf("refusal must list the known sources; got: %v", err)
	}
}

// TestCheckCHRebuildSources_partialTypoRefused: one good name does not
// launder a typo beside it. The run would re-derive soroswap and silently
// nothing for the misspelling.
func TestCheckCHRebuildSources_partialTypoRefused(t *testing.T) {
	err := checkCHRebuildSources(sourcesTestCatalogue(), []string{"soroswap", "aquaris"})
	if err == nil {
		t.Fatal("checkCHRebuildSources accepted -sources soroswap,aquaris — the misspelled name is silently dropped")
	}
	if !strings.Contains(err.Error(), "aquaris") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	if strings.Contains(err.Error(), "[soroswap ") {
		t.Errorf("refusal must not name the VALID source as unknown; got: %v", err)
	}
}

// TestCheckCHRebuildSources_knownNamesAccepted is the other half of the
// contract, and the one that keeps the gate from over-refusing: a known
// name is accepted even when THIS invocation would not engage its pass.
// scripts/ops/ch-rebuild-projected.sh asks -preflight for its whole source
// set and deletes only the subset the verdict names back, so narrowing
// must stay a legal answer — refusing here would abort that script on
// every window.
func TestCheckCHRebuildSources_knownNamesAccepted(t *testing.T) {
	cat := sourcesTestCatalogue()
	for _, named := range [][]string{
		nil,                         // no -sources: the whole catalogue
		{"soroswap"},                // plain event source
		{"soroswap_router"},         // ContractCall source, no -contract-calls
		{"sdex"},                    // census source, no -sdex
		{"soroswap", "aquarius"},    // several
		{sep41transfers.SourceName}, // sep41 pair, no -sep41 and not promoted
		{sep41supply.SourceName},    //
	} {
		if err := checkCHRebuildSources(cat, named); err != nil {
			t.Errorf("checkCHRebuildSources(%v) = %v, want nil — a known source this run would not engage still narrows legitimately", named, err)
		}
	}
}

// TestCHRebuildSourceUniverse_carriesTheSEP41Pair guards the promotion
// caveat: buildReconciliationCatalogue only promotes the SEP-41 sources
// when [supply] watched_sep41_contracts is configured, so the universe
// must add them unconditionally or `-sources sep41_supply` would be
// refused as unknown on a host without the watched set.
func TestCHRebuildSourceUniverse_carriesTheSEP41Pair(t *testing.T) {
	got := chRebuildSourceUniverse([]reconSource{{name: "soroswap"}})
	want := map[string]bool{
		"soroswap":                false,
		sep41transfers.SourceName: false,
		sep41supply.SourceName:    false,
	}
	for _, name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected source %q in the universe", name)
			continue
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("source %q missing from the universe (got %v)", name, got)
		}
	}
}

// TestCHRebuild_ValidatesSourcesAgainstTheCatalogue pins the CALL SITE the
// gate above is worthless without: chRebuild needs Postgres and ClickHouse
// past the catalogue build, so no test can reach the refusal through the
// entry point. Deleting the call would leave every unit test green while
// `-sources sdx` went back to re-deriving nothing and exiting 0. Mirrors
// TestCHRebuild_BothBackfillSafeLegsAreCalledInOrder's approach.
func TestCHRebuild_ValidatesSourcesAgainstTheCatalogue(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	call := strings.Index(body, "checkCHRebuildSources(cat, parseCSVList(*only))")
	catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
	warm := strings.Index(body, "warmCatalogueGates(")
	writer := strings.Index(body, "drainAndWrite(")
	if catalogue < 0 || warm < 0 || writer < 0 {
		t.Fatalf("anchors not found (catalogue %d, warm %d, writer %d) — this test is asserting nothing", catalogue, warm, writer)
	}
	switch {
	case call < 0:
		t.Error("chRebuild never validates -sources against the catalogue (K015): an unrecognised name makes " +
			"enabled() false for every real source, so the run decodes nothing, reports a clean count and exits 0")
	case call < catalogue:
		t.Error("the -sources check runs BEFORE the catalogue is built — it cannot know which names exist")
	case call > warm:
		t.Error("the -sources check runs AFTER the gate warm-up — a typo pays for the registry reads before being refused")
	case call > writer:
		t.Error("the -sources check runs AFTER the writer")
	}
}

type recordedWindow = timescale.ProjectionDirtyWindow

// fakeDirtyWindowRecorder stands in for the Postgres store at the one call
// [recordCHRebuildDirtyWindows] makes. The SQL itself is
// timescale.Store.RecordProjectionDirtyWindow's, unchanged and already
// covered; what is new is WHAT this path asks it to write.
type fakeDirtyWindowRecorder struct {
	got    []recordedWindow
	failOn string
}

func (f *fakeDirtyWindowRecorder) RecordProjectionDirtyWindow(_ context.Context, w recordedWindow) error {
	if w.Source == f.failOn {
		return errors.New("connection refused")
	}
	f.got = append(f.got, w)
	return nil
}

func TestRecordCHRebuildDirtyWindows_OneWindowPerDeletedSource(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 61_000_000, 61_999_999,
		[]string{"soroswap", "cctp"}, timescale.CHRebuildEmptiedReason); err != nil {
		t.Fatalf("recordCHRebuildDirtyWindows: %v", err)
	}
	if len(rec.got) != 2 {
		t.Fatalf("recorded %d window(s), want one per source: %+v", len(rec.got), rec.got)
	}
	for i, want := range []string{"soroswap", "cctp"} {
		got := rec.got[i]
		if got.Source != want {
			t.Errorf("window %d source = %q, want %q", i, got.Source, want)
		}
		if got.From != 61_000_000 || got.To != 61_999_999 {
			t.Errorf("window %d range = [%d,%d], want [61000000,61999999] — the emptied window, not the whole run",
				i, got.From, got.To)
		}
		if want := "ch-rebuild-projected emptied [61000000,61999999]"; got.Reason != want {
			t.Errorf("window %d reason = %q, want %q", i, got.Reason, want)
		}
		if got.IsProjectorReplay() {
			t.Errorf("window %d classifies as a projector-replay rewind — that would SUPPRESS the projector lag alert for a source whose rows are missing", i)
		}
	}
	if line := out.String(); !strings.Contains(line, "[61000000,61999999]") || !strings.Contains(line, "soroswap,cctp") {
		t.Errorf("report line = %q, want the range and the sources", line)
	}
}

// A store that refuses must not be reported as a filed obligation: the
// caller is deciding whether a hole is still uncertified.
func TestRecordCHRebuildDirtyWindows_StoreFailureIsNotSwallowed(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{failOn: "cctp"}
	var out strings.Builder
	err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 61_000_000, 61_999_999,
		[]string{"soroswap", "cctp"}, timescale.CHRebuildEmptiedReason)
	if err == nil {
		t.Fatal("a store that refused the write reported success")
	}
	if !strings.Contains(err.Error(), "cctp") {
		t.Errorf("error %q does not name the source whose window was NOT filed", err)
	}
	if out.String() != "" {
		t.Errorf("printed %q although the obligation was not filed", out.String())
	}
}

// Recording nothing is not a quiet success — the caller asked for an
// obligation and would otherwise believe it exists.
func TestRecordCHRebuildDirtyWindows_EmptySourceSetIsAnError(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 1, 2, nil, timescale.CHRebuildEmptiedReason); err == nil {
		t.Fatal("recording zero windows reported success")
	}
	if len(rec.got) != 0 {
		t.Errorf("wrote %+v", rec.got)
	}
}

func TestChRebuildRecordDirtySources(t *testing.T) {
	t.Parallel()
	cat := []reconSource{{name: "soroswap"}, {name: "cctp"}, {name: "blend"}}
	t.Run("named catalogue sources pass through in order", func(t *testing.T) {
		t.Parallel()
		got, err := chRebuildRecordDirtySources(cat, []string{"cctp", "soroswap"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Join(got, ",") != "cctp,soroswap" {
			t.Errorf("got %v, want the named sources verbatim", got)
		}
	})
	t.Run("a name no catalogue entry carries is refused", func(t *testing.T) {
		t.Parallel()
		// A window filed under a name no verdict reads is a silent
		// no-op dressed as an obligation — worse than no record, because
		// the operator believes the hole is covered.
		_, err := chRebuildRecordDirtySources(cat, []string{"cctp", "sorosw4p"})
		if err == nil {
			t.Fatal("a non-catalogue source was accepted")
		}
		if !strings.Contains(err.Error(), "sorosw4p") {
			t.Errorf("error %q does not name the offender", err)
		}
	})
	t.Run("no -sources is refused rather than filing the whole catalogue", func(t *testing.T) {
		t.Parallel()
		if _, err := chRebuildRecordDirtySources(cat, nil); err == nil {
			t.Fatal("an empty source list was accepted: every catalogue source would owe a full re-reconcile")
		}
	})
}

func TestCheckCHRebuildRecordDirtyFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                          string
		recordDirty, preflight, write bool
		wantErr                       bool
	}{
		{name: "the documented invocation", recordDirty: true},
		{name: "with -write", recordDirty: true, write: true, wantErr: true},
		{name: "with -preflight", recordDirty: true, preflight: true, wantErr: true},
		{name: "an ordinary -write run is untouched", write: true},
		{name: "a preflight is untouched", preflight: true, write: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCHRebuildRecordDirtyFlags(tc.recordDirty, tc.preflight, tc.write)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkCHRebuildRecordDirtyFlags(%v,%v,%v) = %v, wantErr %v",
					tc.recordDirty, tc.preflight, tc.write, err, tc.wantErr)
			}
		})
	}
}

// A -write run rewrites served rows below the completeness watermark. Before
// it writes, it must file the same obligation projector-replay and
// projected-rebuild file, one per re-derived source, or the next verdict
// carries its prior clean claim over the rewritten range.
func TestCHRebuild_WriteRecordsDirtyWindowBeforeWriting(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	record := strings.Index(body, "recordCHRebuildDirtyWindows(ctx, store, os.Stderr, lo, hi, rederived, timescale.CHRebuildWriteReason)")
	preflight := strings.Index(body, "return reportCHRebuildPreflight(")
	writer := strings.Index(body, "drainAndWrite(")
	if preflight < 0 || writer < 0 {
		t.Fatalf("anchors not found (preflight %d, writer %d) — this test is asserting nothing", preflight, writer)
	}
	switch {
	case record < 0:
		t.Fatal("chRebuild -write records no projection dirty window: the next completeness verdict carries its prior clean claim over the rewritten range")
	case record < preflight:
		t.Error("the record precedes the -preflight stop: a preflight, which writes nothing, would file an obligation")
	case record > writer:
		t.Error("the record follows the writer: a run that fails partway leaves rewritten rows certified by stale evidence")
	}
	if !strings.Contains(body[record-300:record], "if write {") {
		t.Error("the record is not gated on -write: a dry run would file an obligation")
	}
	if !strings.Contains(body[record:record+300], "return ") {
		t.Error("a failed record does not stop the run: it would write without the obligation filed")
	}
}

func TestRecordCHRebuildDirtyWindows_WriteReason(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 62_000_000, 62_999_999,
		[]string{"sdex"}, timescale.CHRebuildWriteReason); err != nil {
		t.Fatalf("recordCHRebuildDirtyWindows: %v", err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("recorded %+v, want one window", rec.got)
	}
	got := rec.got[0]
	if want := "ch-rebuild -write [62000000,62999999]"; got.Reason != want {
		t.Errorf("reason = %q, want %q", got.Reason, want)
	}
	if got.IsProjectorReplay() {
		t.Error("a ch-rebuild -write window classifies as a projector-replay rewind — it would suppress the projector lag alert")
	}
}
