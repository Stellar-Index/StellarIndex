package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func fillDurationCount(t *testing.T, name, result string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	o, err := obs.APICacheFillDuration.GetMetricWithLabelValues(name, result)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.(prometheus.Metric).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.Histogram.GetSampleCount()
}

func TestRunDetachedFill_ObservesDurationOncePerFill(t *testing.T) {
	cases := []struct {
		result string
		fn     func(context.Context) (int, error)
	}{
		{"ok", func(context.Context) (int, error) { return 1, nil }},
		{"error", func(context.Context) (int, error) { return 0, errors.New("db down") }},
		{"timeout", func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, fmt.Errorf("fill: %w", ctx.Err())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.result, func(t *testing.T) {
			name := "test-fill-duration-" + tc.result
			done := make(chan struct{})
			runDetachedFill(slog.Default(), name, 20*time.Millisecond, done, tc.fn, func(int, error) {})
			<-done
			for _, r := range []string{"ok", "error", "timeout"} {
				want := uint64(0)
				if r == tc.result {
					want = 1
				}
				if got := fillDurationCount(t, name, r); got != want {
					t.Errorf("result=%s count = %d, want %d", r, got, want)
				}
			}
		})
	}
}

// Every fill name is a literal at its call site, so the label set is the fixed set of call sites.
func TestRunDetachedFill_LabelIsTheFillNameOnly(t *testing.T) {
	name := "test-fill-duration-labels"
	done := make(chan struct{})
	runDetachedFill(slog.Default(), name, time.Second, done,
		func(context.Context) (int, error) { return 1, nil }, func(int, error) {})
	<-done
	if got := fillDurationCount(t, name, "ok"); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
	if _, err := obs.APICacheFillDuration.GetMetricWithLabelValues(name); err == nil {
		t.Error("histogram must carry exactly the cache and result labels")
	}
}
