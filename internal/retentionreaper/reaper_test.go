package retentionreaper_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/retentionreaper"
)

// The DELETE predicates are proven against real Postgres in
// test/integration/platform_retention_reaper_test.go; these pin the
// worker: the cutoff it hands the store, and what it reports.

var fixedNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func newReaper(name string, retention time.Duration, sweep retentionreaper.SweepFunc) *retentionreaper.Reaper {
	return retentionreaper.New(retentionreaper.Options{
		Name:      name,
		Sweep:     sweep,
		Retention: retention,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return fixedNow },
	})
}

func TestSweepPassesRetentionCutoffAndCountsDeletions(t *testing.T) {
	var gotCutoff time.Time
	r := newReaper(obs.AuthReaperSession, retentionreaper.SessionRetention,
		func(_ context.Context, olderThan time.Time) (int64, error) {
			gotCutoff = olderThan
			return 7, nil
		})
	deletedBefore := testutil.ToFloat64(obs.RetentionReaperRowsDeletedTotal.WithLabelValues(obs.AuthReaperSession))

	r.Sweep(context.Background())

	if want := fixedNow.Add(-90 * 24 * time.Hour); !gotCutoff.Equal(want) {
		t.Errorf("cutoff = %v, want %v (now - 90d)", gotCutoff, want)
	}
	if d := testutil.ToFloat64(obs.RetentionReaperRowsDeletedTotal.WithLabelValues(obs.AuthReaperSession)) - deletedBefore; d != 7 {
		t.Errorf("rows_deleted delta = %v, want 7", d)
	}
	if got := testutil.ToFloat64(obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperSession)); got != float64(fixedNow.Unix()) {
		t.Errorf("last_sweep_unix = %v, want %v", got, fixedNow.Unix())
	}
}

func TestSweepFailureIsCountedAndStillMarksLiveness(t *testing.T) {
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperWebhookDelivery).Set(0)
	r := newReaper(obs.AuthReaperWebhookDelivery, retentionreaper.WebhookDeliveryRetention,
		func(context.Context, time.Time) (int64, error) { return 0, errors.New("boom") })
	errsBefore := testutil.ToFloat64(obs.RetentionReaperErrorsTotal.WithLabelValues(obs.AuthReaperWebhookDelivery))

	r.Sweep(context.Background())

	if d := testutil.ToFloat64(obs.RetentionReaperErrorsTotal.WithLabelValues(obs.AuthReaperWebhookDelivery)) - errsBefore; d != 1 {
		t.Errorf("errors delta = %v, want 1", d)
	}
	if got := testutil.ToFloat64(obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperWebhookDelivery)); got != float64(fixedNow.Unix()) {
		t.Errorf("last_sweep_unix = %v, want %v — a failing reaper is alive", got, fixedNow.Unix())
	}
}

func TestSweepCancelledIsNotAFailure(t *testing.T) {
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperWebhookDelivery).Set(0)
	r := newReaper(obs.AuthReaperWebhookDelivery, retentionreaper.WebhookDeliveryRetention,
		func(context.Context, time.Time) (int64, error) { return 0, context.Canceled })
	errsBefore := testutil.ToFloat64(obs.RetentionReaperErrorsTotal.WithLabelValues(obs.AuthReaperWebhookDelivery))

	r.Sweep(context.Background())

	if d := testutil.ToFloat64(obs.RetentionReaperErrorsTotal.WithLabelValues(obs.AuthReaperWebhookDelivery)) - errsBefore; d != 0 {
		t.Errorf("errors delta = %v, want 0 on cancellation", d)
	}
	if got := testutil.ToFloat64(obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperWebhookDelivery)); got != 0 {
		t.Errorf("last_sweep_unix = %v, want untouched on cancellation", got)
	}
}

func TestNewRejectsMissingRetention(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with zero Retention did not panic")
		}
	}()
	retentionreaper.New(retentionreaper.Options{
		Name:  obs.AuthReaperSession,
		Sweep: func(context.Context, time.Time) (int64, error) { return 0, nil },
	})
}
