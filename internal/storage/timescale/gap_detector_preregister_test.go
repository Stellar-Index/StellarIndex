package timescale

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// gatherGapDetectorRunsTotal collects the runs_total counter through a
// private registry and returns value-by-label-set. It deliberately does
// NOT call WithLabelValues on the vec — that would create the very series
// the test is asserting on — so an absent series is reported as absent.
func gatherGapDetectorRunsTotal(t *testing.T) map[[3]string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(obs.IngestGapDetectorRunsTotal)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := make(map[[3]string]float64)
	for _, mf := range families {
		if mf.GetName() != "stellarindex_ingest_gap_detector_runs_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var key [3]string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "source":
					key[0] = lp.GetValue()
				case "table":
					key[1] = lp.GetValue()
				case "outcome":
					key[2] = lp.GetValue()
				}
			}
			got[key] = m.GetCounter().GetValue()
		}
	}
	return got
}

// TestGapDetectorPreregistersRunsTotalSeries pins the 2026-08-29 09:55Z
// r1 false-fire of stellarindex_ingest_gap_detector_silent: after the
// v0.49.0 deploy the seeded schedule correctly deferred every scan, so
// no Inc() ever ran and `sum by (outcome)(runs_total)` returned NO
// series for 26 min — absent_over_time(runs_total[15m]) read "no scan
// due yet" as "detector dead". A detector that has run zero scans must
// still expose {outcome="ok"} and {outcome="error"} at exactly 0 for
// every configured target.
func TestGapDetectorPreregistersRunsTotalSeries(t *testing.T) {
	t.Parallel()
	targets := []GapDetectorTarget{
		{Source: "prereg-a", Table: "prereg_a_events", LedgerColumn: "ledger"},
		{Source: "prereg-b", Table: "prereg_b_trades", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour},
	}

	before := gatherGapDetectorRunsTotal(t)
	for _, tg := range targets {
		for _, outcome := range []string{"ok", "error"} {
			if _, present := before[[3]string{tg.Source, tg.Table, outcome}]; present {
				t.Fatalf("test fixture %s/%s outcome=%s already present before pre-registration; pick unique names", tg.Source, tg.Table, outcome)
			}
		}
	}

	preregisterGapDetectorSeries(targets)

	after := gatherGapDetectorRunsTotal(t)
	for _, tg := range targets {
		for _, outcome := range []string{"ok", "error"} {
			key := [3]string{tg.Source, tg.Table, outcome}
			v, present := after[key]
			if !present {
				t.Errorf("runs_total{source=%q,table=%q,outcome=%q} absent after pre-registration; a restarted detector with no scan due reads as dead", tg.Source, tg.Table, outcome)
				continue
			}
			if v != 0 {
				t.Errorf("runs_total{source=%q,table=%q,outcome=%q} = %v; want exactly 0 (no scan has run)", tg.Source, tg.Table, outcome, v)
			}
		}
	}
}

// TestGapDetectorPreregisterCoversDefaultTargets guards the production
// wiring: every DefaultGapDetectorTargets entry gets both outcomes, so a
// newly added target cannot regress to emit-on-first-scan.
func TestGapDetectorPreregisterCoversDefaultTargets(t *testing.T) {
	t.Parallel()
	preregisterGapDetectorSeries(DefaultGapDetectorTargets)
	got := gatherGapDetectorRunsTotal(t)
	for _, tg := range DefaultGapDetectorTargets {
		for _, outcome := range []string{"ok", "error"} {
			if _, present := got[[3]string{tg.Source, tg.Table, outcome}]; !present {
				t.Errorf("default target %s/%s outcome=%s not pre-registered", tg.Source, tg.Table, outcome)
			}
		}
	}
}
