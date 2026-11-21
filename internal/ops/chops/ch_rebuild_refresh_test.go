package chops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const wantRefreshFollowUp = "stellarindex-ops trades-cagg-refresh -config /etc/si.toml -from 100 -to 200 -oracle -write"

func chRebuildRefresh(write, refresh bool, written map[string]int) rebuildRefresh {
	return rebuildRefresh{verb: "ch-rebuild", cfgPath: "/etc/si.toml", write: write, refresh: refresh, feeds: chRebuildFeedsAggregates(written), lo: 100, hi: 200}
}

// A -write that landed trade or oracle rows owes the aggregates a refresh over its range: it runs by
// default, is named as owed when switched off, and a failure fails the run.
func TestRefreshAfterCHRebuild(t *testing.T) {
	ran := 0
	run := func(context.Context) error { ran++; return nil }
	var out bytes.Buffer

	for name, tc := range map[string]struct {
		write, refresh bool
		written        map[string]int
		wantRuns       int
		wantOut        string
	}{
		"dry run":                  {false, true, map[string]int{"sdex": 5}, 0, "-write would then refresh the trade and oracle aggregates over [100,200]: prices_1m"},
		"dry run, refresh off":     {false, false, map[string]int{"sdex": 5}, 0, ""},
		"trades written":           {true, true, map[string]int{"sdex": 5}, 1, "refreshing the aggregates over [100,200]"},
		"oracle rows written":      {true, true, map[string]int{"band": 2}, 1, "refreshing"},
		"only sep41 rows":          {true, true, map[string]int{"sep41_supply": 9, "sep41_transfers": 9}, 0, ""},
		"only lending rows":        {true, true, map[string]int{"blend": 9}, 0, ""},
		"a source that wrote zero": {true, true, map[string]int{"sdex": 0}, 0, ""},
		"an unregistered source":   {true, true, map[string]int{"not_in_registry": 1}, 1, "refreshing"},
		"refresh switched off":     {true, false, map[string]int{"sdex": 5}, 0, "FOLLOW-UP OWED"},
	} {
		ran = 0
		out.Reset()
		if err := refreshAfterRebuild(context.Background(), &out, chRebuildRefresh(tc.write, tc.refresh, tc.written), run); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if ran != tc.wantRuns || !strings.Contains(out.String(), tc.wantOut) || (tc.wantOut == "" && out.Len() != 0) {
			t.Errorf("%s: refresh ran %d time(s), output %q; want %d run(s) and %q", name, ran, out.String(), tc.wantRuns, tc.wantOut)
		}
		if name == "refresh switched off" && !strings.Contains(out.String(), wantRefreshFollowUp) {
			t.Errorf("owed follow-up %q does not name the exact command %q", out.String(), wantRefreshFollowUp)
		}
	}

	// The rows are written, so a cancelled run still refreshes, and a failed refresh is the run's error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := refreshAfterRebuild(ctx, &out, chRebuildRefresh(true, true, map[string]int{"sdex": 1}), func(rctx context.Context) error {
		if rctx.Err() != nil {
			t.Error("the refresh inherited the run's cancellation")
		}
		return errors.New("lock timeout")
	})
	if err == nil || !strings.Contains(err.Error(), "are not refreshed") || !strings.Contains(err.Error(), wantRefreshFollowUp) {
		t.Errorf("failed refresh: err = %v, want the stale-aggregates error naming %q", err, wantRefreshFollowUp)
	}
}

// A pass that fails after some rows landed owes the same refresh as a finished run, since a re-run writes nothing and would
// skip it; the run's own error still fails the command.
func TestRefreshAfterFailedCHRebuild(t *testing.T) {
	passErr := errors.New("sdex reconcile stream [150,160]: clickhouse reset")
	var out bytes.Buffer
	ran := 0
	run := func(context.Context) error { ran++; return nil }

	err := refreshAfterFailedRebuild(context.Background(), &out, passErr, chRebuildRefresh(true, false, map[string]int{"sdex": 3}), run)
	if !errors.Is(err, passErr) {
		t.Errorf("err = %v, want the pass error kept", err)
	}
	if !strings.Contains(out.String(), "FOLLOW-UP OWED") || !strings.Contains(out.String(), wantRefreshFollowUp) {
		t.Errorf("refresh off: output %q, want the owed follow-up %q", out.String(), wantRefreshFollowUp)
	}

	out.Reset()
	err = refreshAfterFailedRebuild(context.Background(), &out, passErr, chRebuildRefresh(true, true, map[string]int{"sdex": 3}), run)
	if !errors.Is(err, passErr) || ran != 1 {
		t.Errorf("refresh on: err = %v, refresh ran %d time(s); want the pass error and one refresh", err, ran)
	}

	err = refreshAfterFailedRebuild(context.Background(), &out, passErr, chRebuildRefresh(true, true, map[string]int{"sdex": 3}),
		func(context.Context) error { return errors.New("lock timeout") })
	if !errors.Is(err, passErr) || !strings.Contains(err.Error(), wantRefreshFollowUp) {
		t.Errorf("refresh failed too: err = %v, want both errors and the follow-up", err)
	}

	ran = 0
	out.Reset()
	for name, r := range map[string]rebuildRefresh{
		"nothing written": chRebuildRefresh(true, true, map[string]int{}),
		"dry run":         chRebuildRefresh(false, true, map[string]int{"sdex": 3}),
	} {
		if err := refreshAfterFailedRebuild(context.Background(), &out, passErr, r, run); !errors.Is(err, passErr) {
			t.Errorf("%s: err = %v, want the pass error", name, err)
		}
	}
	if ran != 0 || out.Len() != 0 {
		t.Errorf("no rows written: refresh ran %d time(s), output %q; want neither", ran, out.String())
	}
	if err := refreshAfterFailedRebuild(context.Background(), &out, nil, chRebuildRefresh(true, true, map[string]int{"sdex": 3}), run); err != nil || ran != 0 {
		t.Errorf("a finished run is reported by the success path: err = %v, ran %d", err, ran)
	}
}

// A cancelled pass must not start the uncancellable refresh the operator just tried to stop: it names the owed follow-up
// instead and still fails with the cancellation.
func TestRefreshAfterCancelledCHRebuild(t *testing.T) {
	passErr := fmt.Errorf("sdex reconcile stream [150,160]: %w", context.Canceled)
	var out bytes.Buffer
	ran := 0
	run := func(context.Context) error { ran++; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := refreshAfterFailedRebuild(ctx, &out, passErr, chRebuildRefresh(true, true, map[string]int{"sdex": 3}), run)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation kept", err)
	}
	if ran != 0 {
		t.Errorf("refresh ran %d time(s) after a cancel, want none", ran)
	}
	if !strings.Contains(out.String(), "FOLLOW-UP OWED") || !strings.Contains(out.String(), wantRefreshFollowUp) {
		t.Errorf("output %q, want the owed follow-up %q", out.String(), wantRefreshFollowUp)
	}
}
