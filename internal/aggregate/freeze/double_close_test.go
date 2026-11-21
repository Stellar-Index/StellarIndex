package freeze_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// onceRecoverer closes the open row once, as the SQL UPDATE ... WHERE recovered_at IS NULL does: a later close finds no
// open row and reports it the way timescale.FreezeEventSink wraps it. err, when set, is a real failure.
type onceRecoverer struct {
	recordingSink
	mu     sync.Mutex
	closed bool
	calls  int
	err    error
}

func (r *onceRecoverer) MarkRecovered(context.Context, canonical.Asset, canonical.Asset, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return r.err
	}
	if r.closed {
		return fmt.Errorf("timescale: MarkRecovered: %w", freeze.ErrNoOpenRow)
	}
	r.closed = true
	return nil
}

// releaseFiveMinute freezes and then releases the pair's only window through a writer logging to buf.
func releaseFiveMinute(t *testing.T, sink *onceRecoverer, store *windowedFakeLadderStore, buf *bytes.Buffer) {
	t.Helper()
	_, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0), freeze.WithEventSink(sink),
		freeze.WithLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), freshState(time.Now().UTC()), 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	if kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow); err != nil || kept {
		t.Fatalf("ReleaseWindow = (kept=%v, err=%v), want (false, nil)", kept, err)
	}
}

// sweepOnce runs the recovery worker over the pair's (already listed) open row and returns its WARN output and the
// sweep's "partial" delta.
func sweepOnce(t *testing.T, closer freeze.Recoverer, store *windowedFakeLadderStore) (string, float64) {
	t.Helper()
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)
	var buf bytes.Buffer
	partial := obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues("partial")
	before := testutil.ToFloat64(partial)
	r := freeze.NewRecovery(rdb, &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}, closer,
		freeze.RecoveryOptions{Interval: 10 * time.Millisecond, Ladder: store, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
	return buf.String(), testutil.ToFloat64(partial) - before
}

// TestDoubleClose_LoserTreatsNoOpenRowAsSuccess: the releasing Writer and the recovery sweep may both close one row;
// whichever runs second finds it closed, which is the end state both wanted, so neither may warn or count an error.
func TestDoubleClose_LoserTreatsNoOpenRowAsSuccess(t *testing.T) {
	t.Run("writer_first_sweep_loses", func(t *testing.T) {
		sink, store := &onceRecoverer{}, newWindowedFakeLadderStore()
		var wbuf bytes.Buffer
		releaseFiveMinute(t, sink, store, &wbuf)
		if !sink.closed {
			t.Fatal("setup: the releasing writer did not close the row")
		}
		warns, partial := sweepOnce(t, sink, store)
		if sink.calls < 2 {
			t.Fatalf("setup: the sweep never tried to close the row (%d calls)", sink.calls)
		}
		if strings.Contains(warns, "level=WARN") || partial != 0 {
			t.Errorf("sweep after the writer closed the row: warnings %q, partial sweeps +%v; want none", warns, partial)
		}
	})
	t.Run("sweep_first_writer_loses", func(t *testing.T) {
		sink, store := &onceRecoverer{closed: true}, newWindowedFakeLadderStore()
		var wbuf bytes.Buffer
		releaseFiveMinute(t, sink, store, &wbuf)
		if sink.calls != 1 {
			t.Fatalf("setup: the writer made %d close calls, want 1", sink.calls)
		}
		if strings.Contains(wbuf.String(), "level=WARN") {
			t.Errorf("writer after the sweep closed the row warned: %q", wbuf.String())
		}
	})
	t.Run("real_failures_still_reported", func(t *testing.T) {
		boom := errors.New("connection reset")
		sink, store := &onceRecoverer{err: boom}, newWindowedFakeLadderStore()
		var wbuf bytes.Buffer
		releaseFiveMinute(t, sink, store, &wbuf)
		if !strings.Contains(wbuf.String(), "level=WARN") {
			t.Errorf("a real close failure in the writer was not warned: %q", wbuf.String())
		}
		if warns, partial := sweepOnce(t, sink, store); !strings.Contains(warns, "level=WARN") || partial == 0 {
			t.Errorf("a real close failure in the sweep: warnings %q, partial +%v; want a warning and a partial sweep", warns, partial)
		}
	})
}
