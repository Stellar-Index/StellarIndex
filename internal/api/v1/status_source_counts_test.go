package v1

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The two queries behind /v1/status `freshness.active_sources` and
// `freshness.total_sources` are evaluated by Prometheus, not by this
// package, so no Go test can run them. What CAN be pinned here is that
// the promtool fixture which does run them —
// deploy/monitoring/rule-tests/status-source-counts_test.yml — runs
// THESE expressions, and models the case it exists for: a source hosted
// by the API binary (`massive`, the fiat-FX worker) publishing its own
// stellarindex_source_enabled series. Until the worker published that
// gauge the feed was invisible to both counts, so the status page
// counted a switched-on, emitting source on neither side of
// "Active sources N / M". The fixture proves that both counts rise by
// one once the series exists; this test keeps the fixture honest about
// which queries it proves that against, and that it still demonstrates
// the delta.

const statusSourceCountsFixture = "deploy/monitoring/rule-tests/status-source-counts_test.yml"

// statusSourceCountsDoc is the slice of the promtool unit-test schema
// this guard reads. Field names follow promtool's.
type statusSourceCountsDoc struct {
	Tests []struct {
		Name        string `yaml:"name"`
		InputSeries []struct {
			Series string `yaml:"series"`
		} `yaml:"input_series"`
		PromQLExprTest []struct {
			Expr       string `yaml:"expr"`
			ExpSamples []struct {
				Value float64 `yaml:"value"`
			} `yaml:"exp_samples"`
		} `yaml:"promql_expr_test"`
	} `yaml:"tests"`
}

// normalizePromQL collapses whitespace so the YAML block scalar and the
// Go raw string compare on their tokens, which is all Prometheus sees.
func normalizePromQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// massiveEnabledSeriesRE matches a fixture series line for the gauge
// the API binary now publishes for massive, in either label order.
var massiveEnabledSeriesRE = regexp.MustCompile(
	`^stellarindex_source_enabled\{(?:[^}]*,)?job="stellarindex_api"(?:,[^}]*)?\}$`)

func TestStatusSourceCountQueries_MatchPromtoolFixture(t *testing.T) {
	path := filepath.Join(repoRootForClaims(t), statusSourceCountsFixture)
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", statusSourceCountsFixture, err)
	}
	var doc statusSourceCountsDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", statusSourceCountsFixture, err)
	}
	if len(doc.Tests) < 2 {
		t.Fatalf("%s has %d cases, want at least the before/after pair", statusSourceCountsFixture, len(doc.Tests))
	}

	wantActive := normalizePromQL(activeSourcesQuery)
	wantTotal := normalizePromQL(totalSourcesQuery)

	// Per case: the expected count for each query, and whether the
	// fixture publishes massive's enabled series from the API job.
	type outcome struct {
		total, active float64
		massiveGauge  bool
	}
	var before, after *outcome
	for _, tc := range doc.Tests {
		o := outcome{total: -1, active: -1}
		for _, s := range tc.InputSeries {
			if massiveEnabledSeriesRE.MatchString(s.Series) && strings.Contains(s.Series, `source="massive"`) {
				o.massiveGauge = true
			}
		}
		for _, e := range tc.PromQLExprTest {
			if len(e.ExpSamples) != 1 {
				t.Fatalf("case %q: expression has %d expected samples, want exactly one scalar count", tc.Name, len(e.ExpSamples))
			}
			switch got := normalizePromQL(e.Expr); got {
			case wantTotal:
				o.total = e.ExpSamples[0].Value
			case wantActive:
				o.active = e.ExpSamples[0].Value
			default:
				t.Errorf("case %q evaluates an expression that is neither totalSourcesQuery nor "+
					"activeSourcesQuery — the fixture has drifted from status.go and no longer "+
					"proves anything about the served counts:\n%s", tc.Name, e.Expr)
			}
		}
		if o.total < 0 || o.active < 0 {
			t.Fatalf("case %q must evaluate BOTH status queries (total=%v active=%v)", tc.Name, o.total, o.active)
		}
		if o.active > o.total {
			t.Errorf("case %q expects active %v > total %v — the numerator is a strict subset of "+
				"the denominator by construction", tc.Name, o.active, o.total)
		}
		if o.massiveGauge {
			after = &o
		} else {
			before = &o
		}
	}

	// The point of the fixture: one case is the host WITHOUT the
	// API-published gauge, one WITH, and the gauge is worth exactly one
	// on each count — the feed is switched on and emitting, so it must
	// land in the numerator as well as the denominator.
	if before == nil {
		t.Fatalf("%s has no case without stellarindex_source_enabled{job=\"stellarindex_api\",source=\"massive\"} — "+
			"the pre-fix shape is what the delta is measured against", statusSourceCountsFixture)
	}
	if after == nil {
		t.Fatalf("%s has no case publishing stellarindex_source_enabled{job=\"stellarindex_api\",source=\"massive\"} — "+
			"the fixture no longer models the API-hosted source it exists for", statusSourceCountsFixture)
	}
	if d := after.total - before.total; d != 1 {
		t.Errorf("total_sources moves by %v when the API publishes massive's enabled series, want exactly 1 "+
			"(before=%v after=%v)", d, before.total, after.total)
	}
	if d := after.active - before.active; d != 1 {
		t.Errorf("active_sources moves by %v when the API publishes massive's enabled series, want exactly 1 "+
			"(before=%v after=%v) — an emitting, switched-on source must count on both sides", d, before.active, after.active)
	}
}
