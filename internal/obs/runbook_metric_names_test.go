package obs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestOracleManipulationDefenseDoc_NoFabricatedMetricNames guards
// docs/architecture/oracle-manipulation-defense.md's "Engineering
// observability" section against citing Prometheus series that
// internal/obs/metrics.go never registers. T509: the doc claimed
// `stellarindex_anomaly_z_score` and `stellarindex_anomaly_confidence`
// exist as a histogram/gauge; neither is registered anywhere.
func TestOracleManipulationDefenseDoc_NoFabricatedMetricNames(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "architecture", "oracle-manipulation-defense.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	doc := string(raw)

	fabricated := []string{
		"stellarindex_anomaly_z_score",
		"stellarindex_anomaly_confidence",
	}

	// The metric names must never appear as registered/real Prometheus
	// series in metrics.go — otherwise the "do not exist" claim below
	// would itself be false.
	metricFamilies, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	registered := make(map[string]bool, len(metricFamilies))
	for _, mf := range metricFamilies {
		registered[mf.GetName()] = true
	}

	for _, name := range fabricated {
		if registered[name] {
			t.Fatalf("expected %s to be unregistered (doc claims it does not exist), but it is registered", name)
		}

		// The doc must not present the name as a live Prometheus series
		// (e.g. "`name{...}` histogram: spikes" / "gauge: drops")
		// without the correcting annotation.
		if idx := strings.Index(doc, name); idx == -1 {
			t.Fatalf("doc no longer mentions %s at all; expected an explicit non-existence note", name)
		}

		// The fabricated name must be accompanied by an explicit
		// non-existence correction within the same bullet (markdown
		// wraps long bullets across lines, so compare the whole
		// paragraph rather than a single physical line).
		para := normalizeWhitespace(paragraphContaining(doc, name))
		if !strings.Contains(para, "do not exist in `internal/obs/metrics.go`") {
			t.Fatalf("bullet referencing %s lacks the non-existence correction: %q", name, para)
		}
	}
}

// paragraphContaining returns the blank-line-delimited block of doc that
// contains needle, so a check can span a wrapped markdown bullet.
func paragraphContaining(doc, needle string) string {
	for _, para := range strings.Split(doc, "\n\n") {
		if strings.Contains(para, needle) {
			return para
		}
	}
	return ""
}

// normalizeWhitespace collapses markdown line-wrapping so a phrase split
// across physical lines still matches as one string.
func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
