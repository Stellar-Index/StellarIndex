// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// writeOptsFor returns -write options over a decoder that emits outs for every
// row. Store is nil on purpose: the poison trade below is rejected by
// Trade.Validate before the store is touched, and the non-trade output's
// nil-store panic is recovered by pipeline.HandleEvent into exactly the
// generic, unclassified error a real failed insert surfaces as.
func writeOptsFor(outs ...consumer.Event) ProjectedRebuildOptions {
	return ProjectedRebuildOptions{
		Write:  true,
		Source: projector.Source{Name: "soroswap", Decoder: &fakeDecoder{matches: true, decodeOuts: outs}},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// poisonSoroswapTrade is a trade the store can never hold (the zero-value
// canonical.Trade fails Validate), so pipeline.HandleEvent — the production
// sink this tool writes through — returns a *pipeline.TradeDroppedError.
func poisonSoroswapTrade() consumer.Event {
	return soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap", Ledger: 77, TxHash: "deadbeef"}}
}

// TestApplyProjectedEvent_PermanentlyDroppedTradeDoesNotHoldTheWindow pins the
// projected-rebuild half of RLT-132. Once HandleEvent REPORTS a permanently
// dropped trade (instead of folding it into nil), counting every non-nil
// return as a window-holding insert error leaves a deterministically poison
// trade's window un-checkpointed on EVERY resumed run, under a log line that
// says "re-run to retry it" — which can never succeed.
//
// Corrected: the drop is counted on its own and contributes NOTHING to the
// window-holding count, so the window checkpoints, as it did before the drop
// was reported — the difference is that the loss is now visible.
func TestApplyProjectedEvent_PermanentlyDroppedTradeDoesNotHoldTheWindow(t *testing.T) {
	counters := newProjectedRebuildCounters()
	emitted, insertErrs := applyProjectedEvent(context.Background(),
		writeOptsFor(poisonSoroswapTrade()), quietLogger(), counters, events.Event{Ledger: 77})

	if emitted != 1 {
		t.Errorf("emitted = %d, want 1", emitted)
	}
	if insertErrs != 0 {
		t.Errorf("window-holding insert errors = %d, want 0 — a deterministic store rejection must not hold the window: no re-run can land it, so every resumed run would redo this window forever", insertErrs)
	}
	if got := counters.permanentDrops.Load(); got != 1 {
		t.Errorf("permanentDrops = %d, want 1 — the drop must be counted, not vanish", got)
	}
}

// TestApplyProjectedEvent_OtherFailuresStillHoldTheWindow is the guard on the
// other side: COR-09 is untouched. A failure that is NOT a permanent trade
// drop still counts toward holding the window, a poison trade beside it does
// not mask it, and every output of the row is still offered to the sink.
func TestApplyProjectedEvent_OtherFailuresStillHoldTheWindow(t *testing.T) {
	counters := newProjectedRebuildCounters()
	emitted, insertErrs := applyProjectedEvent(context.Background(),
		writeOptsFor(poisonSoroswapTrade(), rozo.Event{}, poisonSoroswapTrade()),
		quietLogger(), counters, events.Event{Ledger: 77})

	if emitted != 3 {
		t.Errorf("emitted = %d, want 3 — every output of the row is offered to the sink", emitted)
	}
	if insertErrs != 1 {
		t.Errorf("window-holding insert errors = %d, want 1 (the failed non-trade insert, and only it)", insertErrs)
	}
	if got := counters.permanentDrops.Load(); got != 2 {
		t.Errorf("permanentDrops = %d, want 2 — one per dropped trade", got)
	}
}

// TestProjectedRebuildLossLines_NeverTellsTheOperatorToReRunAPermanentDrop —
// the two loss classes need OPPOSITE operator actions, so the summary must not
// blur them: a held window says re-run; a permanent drop must say a re-run
// cannot land it.
func TestProjectedRebuildLossLines_NeverTellsTheOperatorToReRunAPermanentDrop(t *testing.T) {
	if got := projectedRebuildLossLines(ProjectedRebuildResult{}); len(got) != 0 {
		t.Errorf("clean run printed loss lines: %q", got)
	}

	drops := strings.Join(projectedRebuildLossLines(ProjectedRebuildResult{PermanentDrops: 3}), "\n")
	for _, want := range []string{"3 trade(s) PERMANENTLY rejected", "ARE checkpointed", "re-run CANNOT land them", "-resume=false"} {
		if !strings.Contains(drops, want) {
			t.Errorf("permanent-drop report is missing %q:\n%s", want, drops)
		}
	}
	if strings.Contains(drops, "NOT checkpointed") || strings.Contains(drops, "RE-RUN the same command") {
		t.Errorf("permanent-drop report must not claim a held window or prescribe a plain re-run:\n%s", drops)
	}

	held := strings.Join(projectedRebuildLossLines(ProjectedRebuildResult{WindowsHeld: 2, InsertErrors: 5}), "\n")
	for _, want := range []string{"2 window(s) NOT checkpointed", "5 insert(s) failed", "RE-RUN the same command"} {
		if !strings.Contains(held, want) {
			t.Errorf("held-window report is missing %q:\n%s", want, held)
		}
	}
	if strings.Contains(held, "PERMANENTLY rejected") {
		t.Errorf("held-window report must not mention permanent drops when there are none:\n%s", held)
	}
}
