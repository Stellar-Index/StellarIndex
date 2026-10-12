package obstest

import (
	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"
)

// CounterValue returns the current value of a plain prometheus.Counter. Counters are process-global and never reset between tests, so assert on the DELTA of a before/after pair (absolute values fail under `-count=2` and parallel siblings).
func CounterValue(c prometheus.Counter) float64 {
	m := &io_prom_dto.Metric{}
	if err := c.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
