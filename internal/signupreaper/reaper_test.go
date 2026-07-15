package signupreaper_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
)

type reapCall struct {
	prefix    string
	olderThan time.Time
}

type fakeOrphanStore struct {
	mu      sync.Mutex
	calls   []reapCall
	deleted int64
	err     error
}

func (f *fakeOrphanStore) ReapSuspendedOrphans(_ context.Context, prefix string, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, reapCall{prefix: prefix, olderThan: olderThan})
	if f.err != nil {
		return 0, f.err
	}
	return f.deleted, nil
}

func (f *fakeOrphanStore) lastCall(t *testing.T) reapCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("store was never called")
	}
	return f.calls[len(f.calls)-1]
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSweep_DeletesAndRecordsOkMetric — a successful sweep that deletes
// N rows advances the ok run counter + histogram and adds N to the
// rows-deleted counter.
func TestSweep_DeletesAndRecordsOkMetric(t *testing.T) {
	store := &fakeOrphanStore{deleted: 3}
	r := signupreaper.New(store, signupreaper.Options{
		Interval: time.Hour, MinAge: 24 * time.Hour, Logger: silent(),
	})

	beforeRuns := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("ok"))
	beforeRows := testutil.ToFloat64(obs.SignupReaperRowsDeletedTotal)
	beforeHist := obstest.HistogramSampleCount(t, obs.SignupReaperRunDurationSeconds, "outcome", "ok")

	r.Sweep(context.Background())

	if got := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("ok")) - beforeRuns; got != 1 {
		t.Errorf("runs_total{ok} delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.SignupReaperRowsDeletedTotal) - beforeRows; got != 3 {
		t.Errorf("rows_deleted_total delta = %v, want 3", got)
	}
	if after := obstest.HistogramSampleCount(t, obs.SignupReaperRunDurationSeconds, "outcome", "ok"); after <= beforeHist {
		t.Errorf("ok duration histogram did not advance (%d -> %d)", beforeHist, after)
	}
}

// TestSweep_ErrorRecordsErrorMetric — a store failure records the error
// outcome (counter + histogram) and does NOT touch the rows-deleted
// counter.
func TestSweep_ErrorRecordsErrorMetric(t *testing.T) {
	store := &fakeOrphanStore{err: errors.New("postgres unreachable")}
	r := signupreaper.New(store, signupreaper.Options{Logger: silent()})

	beforeRuns := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("error"))
	beforeRows := testutil.ToFloat64(obs.SignupReaperRowsDeletedTotal)
	beforeHist := obstest.HistogramSampleCount(t, obs.SignupReaperRunDurationSeconds, "outcome", "error")

	r.Sweep(context.Background())

	if got := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("error")) - beforeRuns; got != 1 {
		t.Errorf("runs_total{error} delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.SignupReaperRowsDeletedTotal) - beforeRows; got != 0 {
		t.Errorf("rows_deleted_total advanced on error: delta = %v, want 0", got)
	}
	if after := obstest.HistogramSampleCount(t, obs.SignupReaperRunDurationSeconds, "outcome", "error"); after <= beforeHist {
		t.Errorf("error duration histogram did not advance (%d -> %d)", beforeHist, after)
	}
}

// TestSweep_ContextCanceledIsOk — a cancellation surfaces as the "ok"
// outcome (clean shutdown), not "error", matching the pricealerts
// convention so shutdown doesn't smear the error-rate alert.
func TestSweep_ContextCanceledIsOk(t *testing.T) {
	store := &fakeOrphanStore{err: context.Canceled}
	r := signupreaper.New(store, signupreaper.Options{Logger: silent()})

	beforeOK := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("ok"))
	beforeErr := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("error"))

	r.Sweep(context.Background())

	if got := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("ok")) - beforeOK; got != 1 {
		t.Errorf("runs_total{ok} delta = %v, want 1 on context cancellation", got)
	}
	if got := testutil.ToFloat64(obs.SignupReaperRunsTotal.WithLabelValues("error")) - beforeErr; got != 0 {
		t.Errorf("runs_total{error} advanced on cancellation: delta = %v, want 0", got)
	}
}

// TestSweep_PassesReasonPrefixAndAgeWindow — the reaper asks the store
// for exactly the signup-race prefix and an olderThan of now - MinAge.
func TestSweep_PassesReasonPrefixAndAgeWindow(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	store := &fakeOrphanStore{}
	r := signupreaper.New(store, signupreaper.Options{
		MinAge: 24 * time.Hour,
		Clock:  func() time.Time { return now },
		Logger: silent(),
	})

	r.Sweep(context.Background())

	c := store.lastCall(t)
	if c.prefix != signupreaper.SignupRaceReasonPrefix {
		t.Errorf("reason prefix = %q, want %q", c.prefix, signupreaper.SignupRaceReasonPrefix)
	}
	if want := now.Add(-24 * time.Hour); !c.olderThan.Equal(want) {
		t.Errorf("olderThan = %v, want %v (now - MinAge)", c.olderThan, want)
	}
}

// TestNew_DefaultsApplied — a zero Options gets the library default
// MinAge (visible via the age window the store is asked for).
func TestNew_DefaultsApplied(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeOrphanStore{}
	r := signupreaper.New(store, signupreaper.Options{
		Clock:  func() time.Time { return now },
		Logger: silent(),
	})
	r.Sweep(context.Background())
	if want := now.Add(-signupreaper.DefaultMinAge); !store.lastCall(t).olderThan.Equal(want) {
		t.Errorf("default MinAge not applied: olderThan = %v, want %v", store.lastCall(t).olderThan, want)
	}
}

// TestNew_PanicsOnNilStore — construction with a nil store is a wiring
// bug and must panic loudly.
func TestNew_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil store")
		}
	}()
	signupreaper.New(nil, signupreaper.Options{})
}

// TestRun_ReturnsOnContextCancel — Run sweeps at least once and returns
// the context error promptly on cancellation.
func TestRun_ReturnsOnContextCancel(t *testing.T) {
	store := &fakeOrphanStore{}
	r := signupreaper.New(store, signupreaper.Options{Interval: time.Hour, Logger: silent()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run so it sweeps once then returns.

	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if len(store.calls) == 0 {
		t.Error("Run did not sweep before returning")
	}
}
