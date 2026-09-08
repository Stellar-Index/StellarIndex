package obstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"
)

// CounterValue returns the current value of a plain (unlabelled)
// prometheus.Counter.
//
// These counters are PROCESS-GLOBAL: every test in the binary shares one
// instance, and nothing resets it between tests. So an assertion on an
// absolute value passes on the first run and fails under `go test -count=2`,
// and its result depends on which other tests ran first. Always take a
// before/after pair and assert on the DELTA:
//
//	before := obstest.CounterValue(obs.MyCounter)
//	// ... exercise the code path ...
//	if obstest.CounterValue(obs.MyCounter)-before == 0 {
//		t.Fatal("counter did not advance")
//	}
//
// That pattern is what makes such a test survive repeat runs and parallel
// siblings; it is the same reason HistogramSampleCount above is documented
// with a before/after pair rather than a fixed expectation.
func CounterValue(c prometheus.Counter) float64 {
	m := &io_prom_dto.Metric{}
	if err := c.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// CounterValueT is CounterValue with a *testing.T, for callers that want a
// write failure to fail the test rather than read as zero.
func CounterValueT(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &io_prom_dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
