package chops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The idle-tick diagnostic line's min-present read (LakeMinLedger) is purely diagnostic.
// cap67IdleLogTick must swallow a failing read into the log line rather than propagate it as
// a catchUp error: an error is "neither work nor idleness" per followLoop's contract, so it
// would freeze the idle counter and retry the same failing read every tick instead of once
// per cap67IdleLogEvery.
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
