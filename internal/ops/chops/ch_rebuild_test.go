// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
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
		buf := []consumer.Event{soroswap.TradeEvent{}, soroswap.TradeEvent{}}
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
		buf := []consumer.Event{soroswap.TradeEvent{}, soroswap.TradeEvent{}}
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
		buf := []consumer.Event{fakeProtoEvent{src: "reflector"}, soroswap.TradeEvent{}}
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
