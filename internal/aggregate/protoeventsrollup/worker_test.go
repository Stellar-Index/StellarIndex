package protoeventsrollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/obstest"
)

type fakeRefresher struct {
	err   error
	calls int
}

func (f *fakeRefresher) RefreshProtocolEventCounts(context.Context) error {
	f.calls++
	return f.err
}

// TestNew_NilRefresher — missing refresher yields a nil worker so
// main.go can gate with a plain nil check (mirrors usage.NewRollup).
func TestNew_NilRefresher(t *testing.T) {
	if New(nil, Options{}) != nil {
		t.Fatal("New(nil, …) must return nil")
	}
}

// TestRefresh_Metrics proves the paired counter + histogram advance on
// both the ok and refresh_error paths (wave-100 obstest convention).
func TestRefresh_Metrics(t *testing.T) {
	ctx := context.Background()

	okBefore := obstest.HistogramSampleCount(t,
		obs.ProtocolEventsRollupSweepDurationSeconds, "outcome", "ok")
	w := New(&fakeRefresher{}, Options{})
	w.refresh(ctx)
	if got := obstest.HistogramSampleCount(t,
		obs.ProtocolEventsRollupSweepDurationSeconds, "outcome", "ok"); got != okBefore+1 {
		t.Errorf("ok histogram count = %d, want %d", got, okBefore+1)
	}

	errBefore := obstest.HistogramSampleCount(t,
		obs.ProtocolEventsRollupSweepDurationSeconds, "outcome", "refresh_error")
	wErr := New(&fakeRefresher{err: errors.New("pg down")}, Options{})
	wErr.refresh(ctx)
	if got := obstest.HistogramSampleCount(t,
		obs.ProtocolEventsRollupSweepDurationSeconds, "outcome", "refresh_error"); got != errBefore+1 {
		t.Errorf("refresh_error histogram count = %d, want %d", got, errBefore+1)
	}
}

// TestRun_RefreshesImmediately proves Run does one pass before the
// first tick, then returns on context cancellation.
func TestRun_RefreshesImmediately(t *testing.T) {
	f := &fakeRefresher{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run does the immediate refresh, then the select returns.
	_ = New(f, Options{Interval: time.Hour}).Run(ctx)
	if f.calls != 1 {
		t.Errorf("refresher called %d times, want 1 (the immediate pass)", f.calls)
	}
}
