package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// An explicit -from above the resume point would derive [from, to] and stamp the watermark over [resume, from-1], which the
// movements API then serves from the (empty) archive arm. The first run is the case the storage advance cannot catch (wm 0).
func TestResolveDeriveRange_RefusesFromAboveTheResumePoint(t *testing.T) {
	orig := deriveLakeMinLedger
	deriveLakeMinLedger = func(context.Context, string) (uint32, error) { return 2, nil }
	t.Cleanup(func() { deriveLakeMinLedger = orig })
	const floor = uint32(58_762_517)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		wm, from uint32
		refuse   bool
	}{
		{"first run above the floor", 0, 60_000_000, true},
		{"resume skipping a prefix", 59_000_000, 60_000_000, true},
		{"first run at the floor", 0, floor, false},
		{"re-derive below the watermark", 59_000_000, 58_900_000, false},
		{"resume at watermark+1", 59_000_000, 59_000_001, false},
	} {
		readWM := func(context.Context, string) (uint32, error) { return tc.wm, nil }
		// An unreachable lake: a run past the refusal fails on the contiguity read instead.
		_, _, err := resolveDeriveRange(ctx, "127.0.0.1:1", "ch-cap67-movements", tc.from, 0, floor, readWM, false)
		if got := err != nil && strings.Contains(err.Error(), "omit -from"); got != tc.refuse {
			t.Errorf("%s (wm %d, -from %d): refused=%v (err %v), want refused=%v", tc.name, tc.wm, tc.from, got, err, tc.refuse)
		}
	}
}

// A dry run stamps no watermark, so it cannot open the gap the write refusal guards. The entry_index runbook measures an
// arbitrary -from/-to window before the watermark table exists, so a dry run must reach the lake read either way.
func TestResolveDeriveRange_DryRunMeasuresAnyWindow(t *testing.T) {
	orig := deriveLakeMinLedger
	deriveLakeMinLedger = func(context.Context, string) (uint32, error) { return 2, nil }
	t.Cleanup(func() { deriveLakeMinLedger = orig })
	const floor = uint32(58_762_517)
	ctx := context.Background()
	tableMissing := &clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table stellar.entry_history_watermark does not exist"}
	for _, tc := range []struct {
		name    string
		readWM  func(context.Context, string) (uint32, error)
		wantLog string
	}{
		{
			"above the resume point", func(context.Context, string) (uint32, error) { return 59_000_000, nil },
			"a -write over this window would be refused, it skips [59000001,59999999]",
		},
		{
			"watermark table missing", func(context.Context, string) (uint32, error) { return 0, tableMissing },
			"cannot tell whether a -write over this window would be refused",
		},
	} {
		var err error
		log := captureStderr(t, func() {
			_, _, err = resolveDeriveRange(ctx, "127.0.0.1:1", "ch-entry-index", 60_000_000, 60_010_000, floor, tc.readWM, true)
		})
		// Past the resume check the unreachable lake fails the contiguity read: that is the proof the run was let through.
		if err == nil || !strings.Contains(err.Error(), "resolve contiguous lake tip") {
			t.Errorf("%s: dry run err = %v, want it to reach the contiguity read", tc.name, err)
		}
		if !strings.Contains(log, tc.wantLog) {
			t.Errorf("%s: stderr %q, want it to say %q", tc.name, log, tc.wantLog)
		}
	}

	// The same window with -write is still refused, and a dry run with no -from still needs its watermark.
	above := func(context.Context, string) (uint32, error) { return 59_000_000, nil }
	if _, _, err := resolveDeriveRange(ctx, "127.0.0.1:1", "ch-entry-index", 60_000_000, 60_010_000, floor, above, false); err == nil || !strings.Contains(err.Error(), "omit -from") {
		t.Errorf("write above the resume point: err = %v, want the -from refusal", err)
	}
	missing := func(context.Context, string) (uint32, error) { return 0, tableMissing }
	if _, _, err := resolveDeriveRange(ctx, "127.0.0.1:1", "ch-entry-index", 0, 0, floor, missing, true); err == nil || !strings.Contains(err.Error(), "read watermark") {
		t.Errorf("dry run resuming from the watermark: err = %v, want the watermark read error", err)
	}
}
