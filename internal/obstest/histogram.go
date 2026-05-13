// Package obstest contains helpers for asserting against
// Prometheus metrics from regression tests. It exists because
// `prometheus.HistogramVec.WithLabelValues(...)` returns a
// `prometheus.Observer` (not a `prometheus.Collector`), so the
// official `testutil.CollectAndCount` cannot act on a per-label
// child directly.
//
// Centralised here on wave 100 (2026-05-13) after four
// identical 20-line copies of the same helper accumulated across
// the wave-92/93/94/95 regression-test series. Cross-package
// test helpers carry an import-cycle risk for some shapes; this
// package deliberately depends only on the upstream Prometheus
// client libraries, so it's import-safe from every test package.
package obstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"
)

// HistogramSampleCount returns the cumulative sample count of
// the histogram series identified by the given label key/value.
// Sums across every series whose label-set contains
// `(labelKey, labelValue)` — equivalent to what the wire-format
// `_count` suffix exposes per-series.
//
// Use this when a regression test needs to assert a histogram
// observation was recorded for a specific label combination,
// independent of the bucket values (which depend on
// test-machine performance). The typical pattern is:
//
//	before := obstest.HistogramSampleCount(t,
//		obs.MyHistogram, "outcome", "ok")
//	// ... exercise the code path ...
//	after := obstest.HistogramSampleCount(t,
//		obs.MyHistogram, "outcome", "ok")
//	if after <= before {
//		t.Errorf("histogram did not advance")
//	}
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
