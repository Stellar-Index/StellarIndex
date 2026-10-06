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

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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

// TestVerifySoleWriterCAGGCoverage_RefusesShortLookback pins the pre-flip
// gate on the shipped policy shape: prices_1m was widened to 15 minutes
// (0165) but oracle_prices_1m is still at 0034's 5, so a projector stall
// on reflector/redstone would leave its buckets permanently short. The
// flip must be refused, naming exactly that view.
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

// TestVerifySoleWriterCAGGCoverage_SoleWriterTablesInPhase3 pins that
// Phase-3 mode gates the aggregates over a sole-writer source's tables:
// the projector alone writes them there too, so a short lookback misses
// its late rows. Aggregates the dispatcher still feeds live stay ungated.
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

// TestVerifySoleWriterCAGGCoverage_Phase3SkipsDispatcherFedViews runs the
// gate on the shipped catalogue shape: no aggregate reads a sep41 table,
// and oracle_prices_1m's 5 minutes is fine while the dispatcher still
// writes oracle_updates live.
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
