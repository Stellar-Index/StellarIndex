package projector

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// A caught-up source publishes lag 0. Every failure exit below must then
// replace that 0 with the real distance to the durable tip: a gauge frozen at
// its last healthy value keeps stellarindex_projector_lag_high silent while
// the source falls behind.

func lagOf(source string) float64 {
	return testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(source))
}

func TestCycle_WatermarkErrorPublishesLag(t *testing.T) {
	const src = "lag-on-watermark-error"
	h := watermarkHarness(t, src, 100, 100) // cursor 100, tip 100: caught up
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 400
	fake := &fakeLake{wm: 400, wmErr: errors.New("clickhouse: connection reset")}
	h.lake = &sourceLake{open: func(context.Context) (lakeReader, error) { return fake, nil }}
	errBefore := runsCount(t, src, "error")
	h.cycle()

	if d := runsCount(t, src, "error") - errBefore; d != 1 {
		t.Fatalf("runs_total{error} delta = %v, want 1", d)
	}
	if got := lagOf(src); got != 300 {
		t.Errorf("lag after a failed watermark read = %v, want 300 (tip 400 - cursor 100)", got)
	}
}

func TestCycle_StreamErrorPublishesLag(t *testing.T) {
	const src = "lag-on-stream-error"
	h := newWedgeHarness(t, src, nil, 100, func(consumer.Event) error { return nil })
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 250
	h.store.streamErr = errors.New("pq: connection reset")
	h.cycle()

	if h.store.cursor() != 100 {
		t.Fatalf("cursor = %d, want it held at 100 on a stream error", h.store.cursor())
	}
	if got := lagOf(src); got != 150 {
		t.Errorf("lag after a failed stream = %v, want 150 (tip 250 - cursor 100)", got)
	}
}

func TestCycle_CursorReadErrorPublishesLagFromLastRead(t *testing.T) {
	const src = "lag-on-cursor-error"
	h := newWedgeHarness(t, src, nil, 100, func(consumer.Event) error { return nil })
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 180
	h.store.cursorErr = errors.New("pq: projector_cursors: lock timeout")
	h.cycle()

	if got := lagOf(src); got != 80 {
		t.Errorf("lag after a failed cursor read = %v, want 80 (tip 180 - last read cursor 100)", got)
	}
}
