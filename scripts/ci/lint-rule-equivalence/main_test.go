// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCompareFile_DetectsAnnotationDrift is the GH-686 regression: two
// rule files that are byte-identical on expr/for/labels but differ in
// a load-bearing annotation (here, the operator-facing description)
// must be reported as a divergence. Before this fix, annotation prose
// was excluded from comparison entirely and this scenario passed
// silently.
func TestCompareFile_DetectsAnnotationDrift(t *testing.T) {
	dir := t.TempDir()
	multiDir := filepath.Join(dir, "multi")
	r1Dir := filepath.Join(dir, "r1")
	if err := os.MkdirAll(multiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r1Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const multiYAML = `groups:
- name: storage
  rules:
  - alert: stellarindex_postgres_ping_failing
    expr: rate(stellarindex_postgres_ping_failures_total[5m]) > 0
    for: 5m
    labels:
      severity: page
    annotations:
      summary: "Indexer postgres pool ping failing"
      description: "Check the primary postgres unit and the indexer journal."
`
	const r1YAML = `groups:
- name: storage
  rules:
  - alert: stellarindex_postgres_ping_failing
    expr: rate(stellarindex_postgres_ping_failures_total[5m]) > 0
    for: 5m
    labels:
      severity: page
    annotations:
      summary: "Indexer postgres pool ping failing"
      description: "Check postgresql@15-main.service (NOT the umbrella) and the indexer journal."
`
	if err := os.WriteFile(filepath.Join(multiDir, "storage.yml"), []byte(multiYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r1Dir, "storage.yml"), []byte(r1YAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, format)
		_ = args
	}
	allowed := func(string) bool { return false }

	compareFile(multiDir, filepath.Join(r1Dir, "storage.yml"), allowed, fail)

	if len(failures) == 0 {
		t.Fatal("compareFile did not flag the annotation drift between the two trees; annotations are not being compared")
	}
}

// TestCheckExprWaiversAreTested is the GH-1174 regression: a baseline
// `:expr` waiver naming a rule with no promtool test anywhere under
// the rule-tests dir must fail, and adding that test must clear it.
func TestCheckExprWaiversAreTested(t *testing.T) {
	baseline := map[string]bool{
		"meta.yml:stellarindex_alertmanager_down:expr": true,
	}

	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, format)
	}

	checkExprWaiversAreTested(baseline, map[string]bool{}, "deploy/monitoring/rule-tests", fail)
	if len(failures) == 0 {
		t.Fatal("checkExprWaiversAreTested did not flag a waived rule with zero promtool coverage")
	}

	failures = nil
	tested := map[string]bool{"stellarindex_alertmanager_down": true}
	checkExprWaiversAreTested(baseline, tested, "deploy/monitoring/rule-tests", fail)
	if len(failures) != 0 {
		t.Fatalf("checkExprWaiversAreTested flagged a waived rule that IS covered by a promtool test: %v", failures)
	}
}

// TestRuleTestedAlerts confirms the alertname scanner picks up an
// alert_rule_test block's alertname field from a real test file shape.
func TestRuleTestedAlerts(t *testing.T) {
	dir := t.TempDir()
	const yaml = `rule_files:
  - ../rules/meta.yml
tests:
  - alert_rule_test:
      - eval_time: 5m
        alertname: stellarindex_alertmanager_down
`
	if err := os.WriteFile(filepath.Join(dir, "meta_test.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	tested, err := ruleTestedAlerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !tested["stellarindex_alertmanager_down"] {
		t.Fatalf("ruleTestedAlerts(%q) = %v, want stellarindex_alertmanager_down present", dir, tested)
	}
}

func TestAnnotationsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"identical", map[string]string{"summary": "x"}, map[string]string{"summary": "x"}, true},
		{"whitespace-only", map[string]string{"summary": "x  y"}, map[string]string{"summary": "x y"}, true},
		{"job-label-convention", map[string]string{"d": "job=stellarindex-api"}, map[string]string{"d": "job=stellarindex_api"}, true},
		{"content-drift", map[string]string{"d": "umbrella unit"}, map[string]string{"d": "postgresql@15-main.service"}, false},
		{"missing-key", map[string]string{"summary": "x"}, map[string]string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := annotationsEqual(c.a, c.b); got != c.want {
				t.Errorf("annotationsEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
