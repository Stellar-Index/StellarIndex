package chops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// RLT-189 — the periodic idle-tick diagnostic line's min-present read
// (LakeMinLedger) is purely diagnostic: nothing in the follow loop derives
// from it. cap67IdleLogTick must therefore swallow a failing read into the
// log line rather than let it propagate as a catchUp error — a propagated
// error would freeze followLoop's idle counter on the SAME value forever
// (an error is "neither work nor idleness" per followLoop's contract),
// so the exact same failing diagnostic read would be retried every tick
// instead of once per cap67IdleLogEvery, and would never recover.
func TestCap67IdleLogTick_MinLedgerFailureDoesNotPropagate(t *testing.T) {
	res := cap67CatchUp{start: 100, last: 99} // idle: last < start
	failingMinLedger := func(context.Context, string) (uint32, error) {
		return 0, errors.New("clickhouse: lake min ledger: connection refused")
	}

	out := captureStderr(t, func() {
		cap67IdleLogTick(context.Background(), "example.com:9000", res, 30, time.Second, failingMinLedger)
	})

	if !strings.Contains(out, "idle for 30 ticks") {
		t.Fatalf("diagnostic line missing even though the failure must still be logged; got: %q", out)
	}
	if !strings.Contains(out, "unavailable") || !strings.Contains(out, "connection refused") {
		t.Errorf("diagnostic line must name the min-present read failure; got: %q", out)
	}
}
