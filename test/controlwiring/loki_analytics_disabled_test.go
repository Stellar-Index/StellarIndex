package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── T479: Loki ships with Grafana Labs usage analytics armed ──
//
// Loki's upstream default phones home to https://stats.grafana.org/ unless
// `analytics.reporting_enabled: false` is set. loki.r1.yml — the config r1
// actually runs today (see configs/loki/README.md; it's `cp`'d onto the host
// as /etc/loki/config.yml by the single-host stop-gap, not templated by
// ansible) — carried the disable stanza commented out as a suggestion
// rather than applied.
//
// configs/ansible/roles/loki/templates/loki-config.yaml.j2 belongs to a
// SEPARATE, not-yet-applied multi-host HA role gated on R2 (see
// configs/loki/README.md#migration-to-the-ha-role) — it does not render to
// r1 today. It's checked here too only so the same upstream default isn't
// re-armed the day that role is first applied.

// TestLokiR1Config_AnalyticsReportingDisabled parses the config r1 runs
// today and asserts the disable stanza is live, not commented out.
func TestLokiR1Config_AnalyticsReportingDisabled(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "loki", "loki.r1.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cfg struct {
		Analytics *struct {
			ReportingEnabled *bool `yaml:"reporting_enabled"`
		} `yaml:"analytics"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if cfg.Analytics == nil || cfg.Analytics.ReportingEnabled == nil {
		t.Fatalf("%s: analytics.reporting_enabled is absent (still commented out) — Loki phones home to Grafana Labs by upstream default", path)
	}
	if *cfg.Analytics.ReportingEnabled {
		t.Fatalf("%s: analytics.reporting_enabled = true, want false", path)
	}
}

// lokiAnalyticsDisabledPattern matches an uncommented, live
// `analytics:\n  reporting_enabled: false` stanza. The ansible template
// mixes Jinja2 with YAML (`{{ var }}`), which breaks a real YAML parse, so
// this checks the rendered text directly.
var lokiAnalyticsDisabledPattern = regexp.MustCompile(`(?m)^analytics:\n\s+reporting_enabled:\s*false\s*$`)

// TestLokiAnsibleTemplate_AnalyticsReportingDisabled asserts the not-yet-
// applied HA role's template also disables analytics reporting, so it
// doesn't ship with the upstream phone-home default armed whenever the R2
// migration first applies it.
func TestLokiAnsibleTemplate_AnalyticsReportingDisabled(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "loki", "templates", "loki-config.yaml.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !lokiAnalyticsDisabledPattern.Match(raw) {
		t.Fatalf("%s: no uncommented `analytics:\\n  reporting_enabled: false` stanza", path)
	}
}
