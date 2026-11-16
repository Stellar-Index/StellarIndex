package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func testGateAdmissions(ctx context.Context) *gateAdmissions {
	g := newGateAdmissions(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.retryEvery = 2 * time.Millisecond
	return g
}

// A factory child is admitted in memory whether or not its row is written, and after a restart only the row
// re-admits it. A write that fails (Postgres failing over at the creation ledger) must be retried until it lands,
// counted, and visible as unpersisted in the meantime.
func TestGateAdmissions_AFailedWriteIsRetriedUntilItLands(t *testing.T) {
	const source = "gate-admission-retry-test"
	g := testGateAdmissions(t.Context())
	unpersisted := func() float64 { return testutil.ToFloat64(obs.GateAdmissionsUnpersisted.WithLabelValues(source)) }
	failures := func() float64 {
		return testutil.ToFloat64(obs.GateAdmissionPersistFailuresTotal.WithLabelValues(source))
	}

	var down atomic.Bool
	var writes atomic.Int64
	down.Store(true)
	g.persist(source, "CCHILD", func(context.Context) error {
		writes.Add(1)
		if down.Load() {
			return errors.New("postgres: failing over")
		}
		return nil
	})
	if got := unpersisted(); got != 1 {
		t.Fatalf("unpersisted after the failed write = %v, want 1", got)
	}
	if got := failures(); got < 1 {
		t.Fatalf("persist failures = %v, want the failed write counted", got)
	}
	waitFor(t, func() bool { return writes.Load() >= 3 })
	if got := unpersisted(); got != 1 {
		t.Fatalf("unpersisted while the store is still down = %v, want 1", got)
	}

	down.Store(false)
	waitFor(t, func() bool { return unpersisted() == 0 })
	landed := writes.Load()
	time.Sleep(20 * time.Millisecond)
	if writes.Load() != landed {
		t.Errorf("the write was repeated after it landed (%d -> %d)", landed, writes.Load())
	}

	// The loop stopped once drained; a later failure must start it again.
	down.Store(true)
	g.persist(source, "CCHILD2", func(context.Context) error {
		if down.Load() {
			return errors.New("postgres: failing over")
		}
		return nil
	})
	down.Store(false)
	waitFor(t, func() bool { return unpersisted() == 0 })
}

func TestGateAdmissions_AWriteThatLandsIsNotKept(t *testing.T) {
	const source = "gate-admission-ok-test"
	g := testGateAdmissions(t.Context())
	var writes atomic.Int64
	g.persist(source, "CCHILD", func(context.Context) error { writes.Add(1); return nil })
	time.Sleep(20 * time.Millisecond)
	if writes.Load() != 1 || testutil.ToFloat64(obs.GateAdmissionsUnpersisted.WithLabelValues(source)) != 0 {
		t.Errorf("writes = %d, want exactly one and nothing kept", writes.Load())
	}
}

// Shutdown cancels the hook context. The kept write gets one more attempt on a live context, so a store that came
// back in time still receives it.
func TestGateAdmissions_ShutdownMakesOneLastAttemptOnALiveContext(t *testing.T) {
	const source = "gate-admission-shutdown-test"
	ctx, cancel := context.WithCancel(context.Background())
	g := testGateAdmissions(ctx)
	g.retryEvery = time.Hour

	var shuttingDown atomic.Bool
	lastAttempt := make(chan error, 1)
	g.persist(source, "CCHILD", func(wctx context.Context) error {
		if !shuttingDown.Load() {
			return errors.New("postgres: failing over")
		}
		lastAttempt <- wctx.Err()
		return nil
	})
	shuttingDown.Store(true)
	cancel()
	select {
	case err := <-lastAttempt:
		if err != nil {
			t.Errorf("the last attempt ran on a cancelled context (%v): it could never succeed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no last attempt was made at shutdown")
	}
	waitFor(t, func() bool {
		return testutil.ToFloat64(obs.GateAdmissionsUnpersisted.WithLabelValues(source)) == 0
	})
}
