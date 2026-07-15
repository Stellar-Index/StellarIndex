// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// sep41Events is the set of consumer.Event types the two sep41 sources
// emit — the domain the projector has earned sole-writer status for
// (TASK #16b). Kept in one place so the invariant tests below all
// exercise the same set.
func sep41Events() []consumer.Event {
	return []consumer.Event{
		sep41_supply.Event{},
		sep41_transfers.Event{},
	}
}

// TestIsSoleWriterProjected_OnlySep41 pins the sole-writer membership:
// exactly the two sep41 event types, and nothing else. A projected but
// NOT-yet-promoted source (soroswap, comet) must be false — it still
// double-writes in Phase-3 parallel; a non-projected source (sdex) must
// be false too.
func TestIsSoleWriterProjected_OnlySep41(t *testing.T) {
	cases := []struct {
		name       string
		event      consumer.Event
		soleWriter bool
	}{
		{"sep41_supply.Event", sep41_supply.Event{}, true},
		{"sep41_transfers.Event", sep41_transfers.Event{}, true},

		// Projected but un-promoted → still Phase-3 parallel.
		{"soroswap.TradeEvent", soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap"}}, false},
		{"comet.TradeEvent", comet.TradeEvent{Trade: canonical.Trade{Source: "comet"}}, false},

		// Not projected at all.
		{"fakeEvent", fakeEvent{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSoleWriterProjected(tc.event); got != tc.soleWriter {
				t.Errorf("IsSoleWriterProjected(%T) = %v; want %v", tc.event, got, tc.soleWriter)
			}
		})
	}
}

// TestSoleWriter_SubsetOfProjected — invariant: every sole-writer event
// MUST also be a projected event. If it weren't, the dispatcher's
// events-goroutine would skip it (per skipInSink) while NO projector
// arm handled it → total silent loss. This is the structural guard the
// F-1316 class demands.
func TestSoleWriter_SubsetOfProjected(t *testing.T) {
	for _, ev := range sep41Events() {
		if !IsSoleWriterProjected(ev) {
			t.Fatalf("%T is expected to be a sole-writer event but IsSoleWriterProjected=false", ev)
		}
		if !IsProjectedEvent(ev) {
			t.Errorf("%T is sole-writer but NOT projected — skipInSink would drop it with no projector writer", ev)
		}
	}
}

// TestSinkModeForProjector_TruthTable pins the mode selection for every
// combination of the two projector booleans.
func TestSinkModeForProjector_TruthTable(t *testing.T) {
	cases := []struct {
		enabled          bool
		persistPerSource bool
		want             SinkMode
	}{
		{false, false, SinkModeAll},          // projector off → events-goroutine writes all
		{false, true, SinkModeAll},           // projector off → flag irrelevant
		{true, true, SinkModeSkipSoleWriter}, // Phase-3 parallel (sep41 sole-writer)
		{true, false, SinkModeSkipProjected}, // Phase-4 (projector sole writer for all)
	}
	for _, tc := range cases {
		got := SinkModeForProjector(tc.enabled, tc.persistPerSource)
		if got != tc.want {
			t.Errorf("SinkModeForProjector(enabled=%v, persistPerSource=%v) = %v; want %v",
				tc.enabled, tc.persistPerSource, got, tc.want)
		}
	}
}

// TestSinkModeForProjector_Sep41SoleWriterInvariant is the foot-gun
// closure (F-1316): for EVERY combination of the two projector config
// booleans, a sep41 event is written EXACTLY ONCE — never zero (silent
// loss) and never twice (double-write).
//
//   - the dispatcher's events-goroutine writes it iff skipInSink is false;
//   - the projector writes it iff the projector is enabled (its registry
//     always includes sep41 when the watched set is non-empty; when the
//     watched set is empty BOTH paths emit nothing, so the invariant is
//     vacuously satisfied and not exercised here).
//
// Before this change, `persist_per_source` left at its zero-value
// (false) while the projector was enabled selected sole-writer mode for
// a projector that could not serve sep41 → zero writers → total loss.
func TestSinkModeForProjector_Sep41SoleWriterInvariant(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, pps := range []bool{false, true} {
			mode := SinkModeForProjector(enabled, pps)
			for _, ev := range sep41Events() {
				writtenBySink := !skipInSink(ev, mode)
				writtenByProjector := enabled // projector owns sep41 whenever running

				writers := 0
				if writtenBySink {
					writers++
				}
				if writtenByProjector {
					writers++
				}
				if writers != 1 {
					t.Errorf("sep41 %T with enabled=%v persist_per_source=%v: %d writers (sink=%v, projector=%v); want exactly 1",
						ev, enabled, pps, writers, writtenBySink, writtenByProjector)
				}
			}
		}
	}
}

// TestSkipInSink_Sep41AlwaysProjectorOwnedWhenEnabled — the direct
// statement of the fix: whenever the projector is enabled, the
// dispatcher's events-goroutine SKIPS sep41 (projector is sole writer),
// regardless of persist_per_source. When the projector is disabled it
// does NOT skip them (it is the only writer, so a skip would lose them).
func TestSkipInSink_Sep41AlwaysProjectorOwnedWhenEnabled(t *testing.T) {
	for _, ev := range sep41Events() {
		// Projector disabled → must NOT skip (only writer).
		if skipInSink(ev, SinkModeForProjector(false, false)) {
			t.Errorf("%T skipped by sink with projector DISABLED — would be lost", ev)
		}
		if skipInSink(ev, SinkModeForProjector(false, true)) {
			t.Errorf("%T skipped by sink with projector DISABLED — would be lost", ev)
		}
		// Projector enabled (either flag) → must skip (projector sole writer).
		if !skipInSink(ev, SinkModeForProjector(true, true)) {
			t.Errorf("%T NOT skipped by sink in Phase-3 (projector enabled, persist_per_source=true) — would double-write", ev)
		}
		if !skipInSink(ev, SinkModeForProjector(true, false)) {
			t.Errorf("%T NOT skipped by sink in Phase-4 (projector enabled, persist_per_source=false)", ev)
		}
	}
}

// TestSkipInSink_UnpromotedProjectedStillDoubleWritesInPhase3 — the
// scoping guarantee: a projected-but-un-promoted source (soroswap) is
// still written by the dispatcher in Phase-3 parallel mode (double-write
// with the projector, ON CONFLICT dedup), and only stops in Phase-4.
// This proves the sep41 promotion did NOT change any other source's
// behavior.
func TestSkipInSink_UnpromotedProjectedStillDoubleWritesInPhase3(t *testing.T) {
	ev := soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap"}}

	// Phase-3 (SinkModeSkipSoleWriter): dispatcher STILL writes it.
	if skipInSink(ev, SinkModeForProjector(true, true)) {
		t.Errorf("soroswap.TradeEvent skipped in Phase-3 — un-promoted sources must still double-write")
	}
	// Phase-4 (SinkModeSkipProjected): dispatcher stops writing it.
	if !skipInSink(ev, SinkModeForProjector(true, false)) {
		t.Errorf("soroswap.TradeEvent NOT skipped in Phase-4 — projector should be sole writer")
	}
	// Projector off: dispatcher writes it (only writer).
	if skipInSink(ev, SinkModeForProjector(false, true)) {
		t.Errorf("soroswap.TradeEvent skipped with projector disabled — would be lost")
	}
}
