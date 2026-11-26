package clickhouse

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// obs cannot import this package, so its zero-seeded reason list is a copy; an unseeded reason makes increase() miss its first skip.
func TestSupplySkipReasons_SeededInObs(t *testing.T) {
	ch := make(chan prometheus.Metric, 64)
	obs.SupplyFlowSkippedTotal.Collect(ch)
	close(ch)
	var seeded []string
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatal(err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "reason" {
				seeded = append(seeded, l.GetValue())
			}
		}
	}
	for _, r := range SupplySkipReasons {
		if !slices.Contains(seeded, r) {
			t.Errorf("reason %q is not zero-seeded on stellarindex_supply_flow_skipped_total (seeded: %v)", r, seeded)
		}
	}
}
