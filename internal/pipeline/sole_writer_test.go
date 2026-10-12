// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// TestIsSoleWriterProjected_Membership pins membership: exactly the sep41 pair and rozo; projected-but-unpromoted (soroswap, comet, cctp) and non-projected sources are false.
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

// TestSpec_SoleWriterSubsetOfProjected: every sole-writer event MUST also be projected, or the dispatcher's events
// goroutine skips it (per skipInSink) while no projector writes it: total silent loss.
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

// TestSinkModeForProjector_SoleWriterInvariant: for EVERY combination of the two projector booleans a sole-writer event is written EXACTLY ONCE, never zero (loss) or twice.
// The events-goroutine writes iff skipInSink is false; the projector writes iff enabled. persist_per_source=false with the projector on must not select sole-writer mode.
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

// TestSkipInSink_SoleWriterAlwaysProjectorOwnedWhenEnabled: with the projector enabled the dispatcher SKIPS sole-writer events regardless of persist_per_source; disabled, it must not skip.
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

// TestSkipInSink_UnpromotedProjectedStillDoubleWritesInPhase3: a projected-but-unpromoted source (soroswap) still double-writes in Phase-3 (ON CONFLICT dedup) and stops only in Phase-4.
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

func TestSpec_NamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range specs {
		if seen[s.Name] {
			t.Errorf("two specs named %q: SpecByName returns only the first", s.Name)
		}
		seen[s.Name] = true
	}
}

type fakeCAGGWindows struct {
	windows []timescale.CAGGRefreshWindow
	err     error
	calls   int
}

func (f *fakeCAGGWindows) CAGGRefreshWindows(context.Context) ([]timescale.CAGGRefreshWindow, error) {
	f.calls++
	return f.windows, f.err
}

func policy(view string, startOffset time.Duration) timescale.CAGGRefreshWindow {
	return timescale.CAGGRefreshWindow{View: view, HasPolicy: true, StartOffset: startOffset}
}

// TestVerifySoleWriterCAGGCoverage_RefusesShortLookback pins the pre-flip gate on the shipped policies: oracle_prices_1m still has a 5-minute lookback,
// so a projector stall would leave its buckets short and the flip must be refused, naming that view.
func TestVerifySoleWriterCAGGCoverage_RefusesShortLookback(t *testing.T) {
	r := &fakeCAGGWindows{windows: []timescale.CAGGRefreshWindow{
		policy("oracle_prices_1m", 5*time.Minute),
		policy("prices_1m", 15*time.Minute),
		policy("twap_1d", 7*24*time.Hour),
	}}
	err := VerifySoleWriterCAGGCoverage(context.Background(), r, SinkModeSkipProjected)
	if !errors.Is(err, ErrSoleWriterCAGGWindow) {
		t.Fatalf("err = %v, want ErrSoleWriterCAGGWindow", err)
	}
	if want := ErrSoleWriterCAGGWindow.Error() + ": oracle_prices_1m (start_offset 5m0s)"; err.Error() != want {
		t.Errorf("err = %q\nwant  %q", err, want)
	}
}

func TestVerifySoleWriterCAGGCoverage_Phase4Verdicts(t *testing.T) {
	cases := []struct {
		name    string
		windows []timescale.CAGGRefreshWindow
		err     error
		wantErr string
	}{
		{name: "every lookback covers the bound", windows: []timescale.CAGGRefreshWindow{
			policy("oracle_prices_1m", ProjectorStallBound), policy("prices_1m", 15*time.Minute),
		}},
		{name: "unbounded start_offset covers everything", windows: []timescale.CAGGRefreshWindow{
			{View: "supply_1d", HasPolicy: true, Unbounded: true},
		}},
		{name: "one second short", windows: []timescale.CAGGRefreshWindow{
			policy("prices_1m", ProjectorStallBound-time.Second),
		}, wantErr: "prices_1m (start_offset 14m59s)"},
		{name: "no refresh policy", windows: []timescale.CAGGRefreshWindow{
			{View: "source_volume_1h"},
		}, wantErr: "source_volume_1h (no refresh policy)"},
		{name: "no aggregates reported", wantErr: "coverage is unproven"},
		{name: "read error", err: errors.New("boom"), wantErr: "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySoleWriterCAGGCoverage(context.Background(), &fakeCAGGWindows{windows: tc.windows, err: tc.err}, SinkModeSkipProjected)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// soleWriterTables lists, per SoleWriter source in the registry, the
// tables its gap-detector targets name.
func soleWriterTables(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, spec := range Specs() {
		if spec.Projector == nil || !spec.Projector.SoleWriter {
			continue
		}
		for _, target := range timescale.DefaultGapDetectorTargets {
			if target.SourceNetKey() == spec.Name {
				out[spec.Name] = append(out[spec.Name], target.Table)
			}
		}
		if len(out[spec.Name]) == 0 {
			t.Fatalf("sole-writer source %s has no gap-detector target naming its tables", spec.Name)
		}
	}
	if len(out) == 0 {
		t.Fatal("the registry has no sole-writer source")
	}
	return out
}

// TestProjectedSourcesNameTheirTables keeps every projected source
// promotable: the Phase-3 gate refuses a SoleWriter source whose tables
// no gap-detector target names.
func TestProjectedSourcesNameTheirTables(t *testing.T) {
	named := map[string]bool{}
	for _, target := range timescale.DefaultGapDetectorTargets {
		named[target.SourceNetKey()] = true
	}
	for _, spec := range Specs() {
		if spec.Projector != nil && !named[spec.Name] {
			t.Errorf("projected source %s has no gap-detector target", spec.Name)
		}
	}
}

// TestVerifySoleWriterCAGGCoverage_SoleWriterTablesInPhase3 pins that Phase-3 mode gates aggregates over a sole-writer
// source's tables, since the projector alone writes them and a short lookback misses late rows. Dispatcher-fed
// aggregates stay ungated.
func TestVerifySoleWriterCAGGCoverage_SoleWriterTablesInPhase3(t *testing.T) {
	for source, tables := range soleWriterTables(t) {
		for _, table := range tables {
			r := &fakeCAGGWindows{windows: []timescale.CAGGRefreshWindow{
				{View: "oracle_prices_1m", Hypertable: "oracle_updates", HasPolicy: true, StartOffset: 5 * time.Minute},
				{View: table + "_1h", Hypertable: table, HasPolicy: true, StartOffset: 5 * time.Minute},
			}}
			err := VerifySoleWriterCAGGCoverage(context.Background(), r, SinkModeSkipSoleWriter)
			if !errors.Is(err, ErrSoleWriterCAGGWindow) {
				t.Fatalf("%s: aggregate over %s: err = %v, want ErrSoleWriterCAGGWindow", source, table, err)
			}
			if !strings.Contains(err.Error(), table+"_1h (start_offset 5m0s)") || strings.Contains(err.Error(), "oracle_prices_1m") {
				t.Errorf("%s: err = %q, want it to name only %s_1h", source, err, table)
			}
		}
	}
}

// TestVerifySoleWriterCAGGCoverage_Phase3SkipsDispatcherFedViews runs the gate on the shipped catalogue: no aggregate
// reads a sep41 table, and oracle_prices_1m's 5 minutes is fine while the dispatcher still writes oracle_updates live.
func TestVerifySoleWriterCAGGCoverage_Phase3SkipsDispatcherFedViews(t *testing.T) {
	r := &fakeCAGGWindows{windows: []timescale.CAGGRefreshWindow{
		{View: "oracle_prices_1m", Hypertable: "oracle_updates", HasPolicy: true, StartOffset: 5 * time.Minute},
		{View: "prices_1m", Hypertable: "trades", HasPolicy: true, StartOffset: 15 * time.Minute},
		{View: "supply_1d", Hypertable: "asset_supply_history", HasPolicy: true, StartOffset: 7 * 24 * time.Hour},
	}}
	if err := VerifySoleWriterCAGGCoverage(context.Background(), r, SinkModeSkipSoleWriter); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if err := VerifySoleWriterCAGGCoverage(context.Background(), &fakeCAGGWindows{}, SinkModeSkipSoleWriter); !errors.Is(err, ErrSoleWriterCAGGWindow) {
		t.Errorf("empty catalogue: err = %v, want ErrSoleWriterCAGGWindow", err)
	}
}

// TestVerifySoleWriterCAGGCoverage_NoProjector pins that the gate never
// reads the catalogue when the dispatcher writes everything.
func TestVerifySoleWriterCAGGCoverage_NoProjector(t *testing.T) {
	r := &fakeCAGGWindows{windows: []timescale.CAGGRefreshWindow{policy("oracle_prices_1m", 5*time.Minute)}}
	if err := VerifySoleWriterCAGGCoverage(context.Background(), r, SinkModeAll); err != nil || r.calls != 0 {
		t.Errorf("err = %v, calls = %d; want nil, 0", err, r.calls)
	}
}

// TestProjectorStallBound_CoversCatchUpPeriod keeps the bound in lockstep
// with the timer whose period it is sized from.
func TestProjectorStallBound_CoversCatchUpPeriod(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "ch-live-catchup.timer"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^OnUnitActiveSec=(\d+)min\s*$`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("ch-live-catchup.timer has no OnUnitActiveSec=<N>min line")
	}
	period, err := time.ParseDuration(string(m[1]) + "m")
	if err != nil {
		t.Fatal(err)
	}
	if ProjectorStallBound <= period {
		t.Errorf("ProjectorStallBound = %s, must exceed the %s ch-live-catchup period", ProjectorStallBound, period)
	}
}
