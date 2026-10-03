package assetcharacterrollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
)

type fakeRefresher struct {
	err   error
	calls int
}

func (f *fakeRefresher) RefreshAssetVolumeCharacter(context.Context) error {
	f.calls++
	return f.err
}

// TestNew_NilRefresher — missing refresher yields a nil worker so main.go
// can gate with a plain nil check (mirrors assetvolrollup.New).
func TestNew_NilRefresher(t *testing.T) {
	if New(nil, Options{}) != nil {
		t.Fatal("New(nil, …) must return nil")
	}
}

// TestRefresh_Metrics proves the paired counter + histogram advance on both
// the ok and refresh_error paths.
func TestRefresh_Metrics(t *testing.T) {
	ctx := context.Background()

	okBefore := obstest.HistogramSampleCount(t,
		obs.AssetCharacterRollupSweepDurationSeconds, "outcome", "ok")
	w := New(&fakeRefresher{}, Options{})
	w.refresh(ctx)
	if got := obstest.HistogramSampleCount(t,
		obs.AssetCharacterRollupSweepDurationSeconds, "outcome", "ok"); got != okBefore+1 {
		t.Errorf("ok histogram count = %d, want %d", got, okBefore+1)
	}

	errBefore := obstest.HistogramSampleCount(t,
		obs.AssetCharacterRollupSweepDurationSeconds, "outcome", "refresh_error")
	wErr := New(&fakeRefresher{err: errors.New("pg down")}, Options{})
	wErr.refresh(ctx)
	if got := obstest.HistogramSampleCount(t,
		obs.AssetCharacterRollupSweepDurationSeconds, "outcome", "refresh_error"); got != errBefore+1 {
		t.Errorf("refresh_error histogram count = %d, want %d", got, errBefore+1)
	}
}

// TestRun_RefreshesImmediately proves Run does one pass before the first
// tick, then returns on context cancellation.
func TestRun_RefreshesImmediately(t *testing.T) {
	f := &fakeRefresher{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run does the immediate refresh, then the select returns.
	_ = New(f, Options{Interval: time.Hour}).Run(ctx)
	if f.calls != 1 {
		t.Errorf("refresher called %d times, want 1 (the immediate pass)", f.calls)
	}
}

// TestRun_StartupDelayHoldsFirstRoll proves no roll happens at t=0 when a
// startup delay is configured and ctx is cancelled inside it.
func TestRun_StartupDelayHoldsFirstRoll(t *testing.T) {
	f := &fakeRefresher{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := New(f, Options{Interval: time.Hour, StartupDelay: time.Hour}).Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want deadline exceeded", err)
	}
	if f.calls != 0 {
		t.Errorf("refresher called %d times during the startup delay, want 0", f.calls)
	}
}

// TestNew_NegativeDelayDefaults keeps the production default when unset.
func TestNew_NegativeDelayDefaults(t *testing.T) {
	if got := New(&fakeRefresher{}, Options{StartupDelay: -1}).delay; got != DefaultStartupDelay {
		t.Errorf("delay = %s, want %s", got, DefaultStartupDelay)
	}
}

// TestDefaultInterval_Sane keeps the heavy roll on a slow cadence.
func TestDefaultInterval_Sane(t *testing.T) {
	if DefaultInterval < 5*time.Minute {
		t.Errorf("DefaultInterval = %s, want a slow cadence (>= 5m) — the roll is heavy", DefaultInterval)
	}
}
