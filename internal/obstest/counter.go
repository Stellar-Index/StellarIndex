package obstest

import (
	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"
)

// CounterValue returns the current value of a plain (unlabelled)
// prometheus.Counter.
//
// These counters are PROCESS-GLOBAL and never reset between tests, so an absolute assertion
// fails under `go test -count=2` or depends on test order. Assert on the DELTA of a
// before/after pair, which also survives parallel siblings:
//
//	before := obstest.CounterValue(obs.MyCounter)
//	// ... exercise the code path ...
//	if obstest.CounterValue(obs.MyCounter)-before == 0 {
//		t.Fatal("counter did not advance")
//	}
func CounterValue(c prometheus.Counter) float64 {
	m := &io_prom_dto.Metric{}
	if err := c.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
