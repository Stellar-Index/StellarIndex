package projector

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// watermarkHarness is a CH feed-switch projector (projector cursor 100) whose
// ledgerstream tip and lake contiguous watermark are fixed per test.
func watermarkHarness(t *testing.T, name string, ledgerstreamTip, watermark uint32) *wedgeHarness {
	t.Helper()
	h := newWedgeHarness(t, name, nil, ledgerstreamTip, func(consumer.Event) error { return nil })
	h.proj.chAddr = "clickhouse.invalid:9000"
	fake := &fakeLake{wm: watermark}
	h.lake = &sourceLake{open: func(context.Context) (lakeReader, error) { return fake, nil }}
	return h
}

// A lake hole at ledger 101 stalls the watermark at 100 while ledgerstream
// holds through 200. The source is held, not caught up: it must be counted as
// watermark_held and report the real 100-ledger lag, or a stalled watermark
// is indistinguishable from a healthy idle source and no lag alert can fire.
func TestCycle_WatermarkClampedIdleReportsHeldWithRealLag(t *testing.T) {
	const src = "wm-held"
	h := watermarkHarness(t, src, 200, 100)
	idleBefore := runsCount(t, src, "idle")
	heldBefore := runsCount(t, src, "watermark_held")

	h.cycle()

	if got := testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(src)); got != 100 {
		t.Errorf("lag = %v, want 100 (ledgerstream tip 200 - cursor 100)", got)
	}
	if d := runsCount(t, src, "watermark_held") - heldBefore; d != 1 {
		t.Errorf("runs_total{watermark_held} delta = %v, want 1", d)
	}
	if d := runsCount(t, src, "idle") - idleBefore; d != 0 {
		t.Errorf("runs_total{idle} delta = %v, want 0 for a watermark-held cycle", d)
	}
	if h.store.projectorCursor != 100 {
		t.Errorf("cursor = %d, want it held at 100", h.store.projectorCursor)
	}
}

// When ledgerstream itself is at the cursor, the empty scan range is a real
// catch-up: idle with lag 0.
func TestCycle_WatermarkAtDurableTipIsIdle(t *testing.T) {
	const src = "wm-caught-up"
	h := watermarkHarness(t, src, 100, 100)
	idleBefore := runsCount(t, src, "idle")
	heldBefore := runsCount(t, src, "watermark_held")

	h.cycle()

	if got := testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(src)); got != 0 {
		t.Errorf("lag = %v, want 0", got)
	}
	if d := runsCount(t, src, "idle") - idleBefore; d != 1 {
		t.Errorf("runs_total{idle} delta = %v, want 1", d)
	}
	if d := runsCount(t, src, "watermark_held") - heldBefore; d != 0 {
		t.Errorf("runs_total{watermark_held} delta = %v, want 0", d)
	}
}
