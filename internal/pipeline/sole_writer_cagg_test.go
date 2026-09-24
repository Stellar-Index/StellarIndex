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

// TestVerifySoleWriterCAGGCoverage_OnlyPhase4 pins that the gate never
// blocks a mode in which the dispatcher still feeds the aggregates.
func TestVerifySoleWriterCAGGCoverage_OnlyPhase4(t *testing.T) {
	for _, mode := range []SinkMode{SinkModeAll, SinkModeSkipSoleWriter} {
		r := &fakeCAGGWindows{windows: []timescale.CAGGRefreshWindow{policy("oracle_prices_1m", 5*time.Minute)}}
		if err := VerifySoleWriterCAGGCoverage(context.Background(), r, mode); err != nil {
			t.Errorf("mode %v: err = %v, want nil", mode, err)
		}
		if r.calls != 0 {
			t.Errorf("mode %v: read the policies %d times, want 0", mode, r.calls)
		}
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
