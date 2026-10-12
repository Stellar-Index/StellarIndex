// Package obstest contains helpers for asserting against Prometheus metrics in regression tests.
// HistogramVec.WithLabelValues returns an Observer, not a Collector, so testutil.CollectAndCount
// cannot act on a per-label child directly.
//
// It depends only on the upstream Prometheus client libraries so it cannot create an import
// cycle from any test package.
package obstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"
)

// HistogramSampleCount returns the cumulative sample count summed across every series whose label set contains (labelKey, labelValue), like the wire-format `_count`; use a before/after pair to assert an observation was recorded independent of machine-speed-dependent bucket values.
func HistogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, labelKey, labelValue string) uint64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var total uint64
	for m := range ch {
		var dto io_prom_dto.Metric
		if err := m.Write(&dto); err != nil {
			t.Fatalf("obstest.HistogramSampleCount: Write: %v", err)
		}
		for _, l := range dto.GetLabel() {
			if l.GetName() == labelKey && l.GetValue() == labelValue {
				total += dto.GetHistogram().GetSampleCount()
			}
		}
	}
	return total
}
