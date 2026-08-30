package obs

import (
	"os"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// childLabelValues collects the value of `label` across every child
// series currently registered on a CounterVec.
func childLabelValues(t *testing.T, vec *prometheus.CounterVec, label string) map[string]bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	out := map[string]bool{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		for _, lp := range pb.GetLabel() {
			if lp.GetName() == label {
				out[lp.GetValue()] = true
			}
		}
	}
	return out
}

// TestDivergenceRefreshOutcomesAreSeeded is the wave-D ALERT-06
// regression.
//
// Both divergence guards — stellarindex_divergence_no_reference and
// stellarindex_divergence_refresh_error_dominant — compare a FAILURE
// outcome's rate against the `ok` outcome's rate. A counter child does
// not exist until its first .Inc(), so a process that has never had a
// successful refresh has no `ok` series, the comparison is the empty
// vector, and BOTH alerts are silent in exactly the total-outage case
// they exist for: every reference unreachable, the aggregator restarted
// mid-outage (as deploys routinely do), flags.divergence_warning served
// frozen, and a live depeg unflagged.
//
// The subject set is derived from the EMITTER, not restated here, so a
// new outcome added to divergence_refresh.go without a matching seed
// fails on the day it is written rather than at the next audit.
func TestDivergenceRefreshOutcomesAreSeeded(t *testing.T) {
	src, err := os.ReadFile("../aggregate/orchestrator/divergence_refresh.go")
	if err != nil {
		t.Fatalf("read emitter source: %v", err)
	}

	// Both spellings the emitter uses: a literal at the WithLabelValues
	// call, and the `outcome = "…"` / `outcome := "…"` assignments feeding
	// the dynamic call site.
	lits := regexp.MustCompile(`DivergenceRefreshTotal\.WithLabelValues\("([a-z_]+)"\)`)
	assigns := regexp.MustCompile(`outcome\s:?=\s"([a-z_]+)"`)

	want := map[string]bool{}
	for _, m := range lits.FindAllStringSubmatch(string(src), -1) {
		want[m[1]] = true
	}
	for _, m := range assigns.FindAllStringSubmatch(string(src), -1) {
		want[m[1]] = true
	}
	if len(want) == 0 {
		t.Fatal("found no outcome literals in the emitter — the scan is broken, and a " +
			"guard with an empty subject set passes forever")
	}
	// The alerts compare against `ok`, so it must be in the set whatever
	// the scan finds.
	want["ok"] = true

	got := childLabelValues(t, DivergenceRefreshTotal, "outcome")
	for outcome := range want {
		if !got[outcome] {
			t.Errorf("outcome %q is emitted by divergence_refresh.go but not pre-seeded in "+
				"seedBoundedLabelSeries. Until its first occurrence the series does not "+
				"exist, and a rule comparing rates across outcomes evaluates to the EMPTY "+
				"vector rather than zero — silencing the alert in the total-failure case "+
				"it exists to catch.", outcome)
		}
	}
}
