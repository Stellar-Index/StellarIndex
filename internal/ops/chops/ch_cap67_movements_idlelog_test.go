package chops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The idle-tick diagnostic read (LakeMinLedger) is purely diagnostic: cap67IdleLogTick must swallow its failure into the
// log line. Propagating it as a catchUp error would count as neither work nor idleness, freezing the idle counter and
// retrying the read every tick.
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
