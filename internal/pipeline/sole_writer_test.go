// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// soleWriterEvents is the set of consumer.Event types the projector has
// earned sole-writer status for: the sep41 pair and rozo. Kept in one
// place so the invariant tests below all exercise the same set.
func soleWriterEvents() []consumer.Event {
	return []consumer.Event{
		sep41_supply.Event{},
		sep41_transfers.Event{},
		rozo.Event{},
	}
}

// TestIsSoleWriterProjected_Membership pins the sole-writer membership:
// exactly the sep41 pair and rozo, and nothing else. A projected but
// NOT-yet-promoted source (soroswap, comet, cctp) must be false — it still
// double-writes in Phase-3 parallel; a non-projected source must be false
// too.
func TestIsSoleWriterProjected_Membership(t *testing.T) {
	cases := []struct {
		name       string
		event      consumer.Event
		soleWriter bool
	}{
		{"sep41_supply.Event", sep41_supply.Event{}, true},
		{"sep41_transfers.Event", sep41_transfers.Event{}, true},
		{"rozo.Event", rozo.Event{}, true},

		// Projected but un-promoted → still Phase-3 parallel.
		{"soroswap.TradeEvent", soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap"}}, false},
		{"comet.TradeEvent", comet.TradeEvent{Trade: canonical.Trade{Source: "comet"}}, false},
		{"cctp.Event", cctp.Event{}, false},

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

// TestSpec_SoleWriterSubsetOfProjected — every sole-writer event MUST
// also be projected. If it weren't, the dispatcher's events goroutine
// would skip it (per skipInSink) while no projector source wrote it:
// total silent loss.
func TestSpec_SoleWriterSubsetOfProjected(t *testing.T) {
	for _, ev := range soleWriterEvents() {
		if !IsSoleWriterProjected(ev) {
			t.Fatalf("%T is expected to be a sole-writer event but IsSoleWriterProjected=false", ev)
		}
	}
	for _, spec := range specs {
		for _, ev := range spec.Events {
			if IsSoleWriterProjected(ev) && !IsProjectedEvent(ev) {
				t.Errorf("%T (spec %s) is sole-writer but NOT projected — skipInSink would drop it with no projector writer", ev, spec.Name)
			}
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
		{true, true, SinkModeSkipSoleWriter}, // Phase-3 parallel (SoleWriter specs projector-only)
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

// TestSinkModeForProjector_SoleWriterInvariant is the foot-gun closure:
// for EVERY combination of the two projector config booleans,
// a sole-writer event is written EXACTLY ONCE — never zero (silent loss)
// and never twice (double-write).
//
//   - the dispatcher's events-goroutine writes it iff skipInSink is false;
//   - the projector writes it iff the projector is enabled: both writers
//     build from the same enabled_sources (rozo) or watched set (sep41),
//     and an empty set makes BOTH paths emit nothing, so the invariant is
//     vacuously satisfied there and not exercised here.
//
// Before this change, `persist_per_source` left at its zero-value
// (false) while the projector was enabled selected sole-writer mode for
// a projector that could not serve sep41 → zero writers → total loss.
func TestSinkModeForProjector_SoleWriterInvariant(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, pps := range []bool{false, true} {
			mode := SinkModeForProjector(enabled, pps)
			for _, ev := range soleWriterEvents() {
				writtenBySink := !skipInSink(ev, mode)
				writtenByProjector := enabled // projector owns it whenever running

				writers := 0
				if writtenBySink {
					writers++
				}
				if writtenByProjector {
					writers++
				}
				if writers != 1 {
					t.Errorf("%T with enabled=%v persist_per_source=%v: %d writers (sink=%v, projector=%v); want exactly 1",
						ev, enabled, pps, writers, writtenBySink, writtenByProjector)
				}
			}
		}
	}
}

// TestSkipInSink_SoleWriterAlwaysProjectorOwnedWhenEnabled — the direct
// statement of the fix: whenever the projector is enabled, the
// dispatcher's events-goroutine SKIPS a sole-writer event,
// regardless of persist_per_source. When the projector is disabled it
// does NOT skip them (it is the only writer, so a skip would lose them).
func TestSkipInSink_SoleWriterAlwaysProjectorOwnedWhenEnabled(t *testing.T) {
	for _, ev := range soleWriterEvents() {
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

// TestRozo_ProjectorSoleWriterInPhase3 pins rozo's promotion: in the
// Phase-3 mode r1 runs, the dispatcher skips rozo.Event and the projector
// still builds and owns a rozo source, so the event has exactly one writer.
func TestRozo_ProjectorSoleWriterInPhase3(t *testing.T) {
	ev := rozo.Event{}
	phase3 := SinkModeForProjector(true, true)
	if !skipInSink(ev, phase3) {
		t.Error("dispatcher still writes rozo.Event in Phase 3: two writers for rozo_events")
	}
	spec, ok := SpecByName(rozo.SourceName)
	if !ok || spec.Projector == nil || !spec.Projector.SoleWriter {
		t.Fatalf("rozo spec = %+v, want a SoleWriter projector", spec)
	}
	dec, err := spec.NewDecoder(BuildArgs{})
	if err != nil || dec == nil {
		t.Fatalf("rozo projector decoder = %v, %v; want one built from no config", dec, err)
	}
	if skipInSink(ev, SinkModeForProjector(false, true)) {
		t.Error("rozo.Event skipped with the projector disabled: no writer")
	}
}
