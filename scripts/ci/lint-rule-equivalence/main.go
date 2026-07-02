// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// lint-rule-equivalence — semantic differ for the two Prometheus
// rule trees.
//
// deploy/monitoring/rules/ (multi-host, underscored job labels) and
// configs/prometheus/rules.r1/ (single-host overlay, hyphenated job
// labels) are hand-maintained near-copies. lint-docs.sh checks FILE
// pairing; nothing checked that the paired files stay SEMANTICALLY
// equivalent — an operator fixing a threshold in one tree silently
// diverges the only live deployment from the documented one (the
// api.yml header has warned about exactly this since F-1222).
//
// This linter parses both trees and compares, per paired file, the
// set of alert/record rules and each rule's expr (job labels
// normalized: `stellarindex-x` ≡ `stellarindex_x`), `for`, and
// `labels`. Comments and annotation prose are deliberately NOT
// compared — wording may differ; firing behavior may not.
//
// Intentional divergences live in
// scripts/ci/rule-equivalence.baseline, one per line:
//
//	<file>:<rule>[:<field>]  — allow this specific divergence
//
// The .baseline suffix puts the file under lint-baseline-growth.sh
// (CS-098): growth requires a declared Baseline-Growth: trailer.
// Stale entries (divergence no longer present) fail, so the
// baseline shrinks monotonically.
//
// Usage:
//
//	go run ./scripts/ci/lint-rule-equivalence \
//	    deploy/monitoring/rules configs/prometheus/rules.r1 \
//	    scripts/ci/rule-equivalence.baseline
//
// Files intentionally absent from the r1 overlay (per
// configs/prometheus/rules.r1/README.md) are simply not compared —
// file pairing is lint-docs.sh's job.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type rule struct {
	Alert  string            `yaml:"alert"`
	Record string            `yaml:"record"`
	Expr   string            `yaml:"expr"`
	For    string            `yaml:"for"`
	Labels map[string]string `yaml:"labels"`
}

func (r rule) name() string {
	if r.Alert != "" {
		return r.Alert
	}
	return "record:" + r.Record
}

type group struct {
	Rules []rule `yaml:"rules"`
}

type ruleFile struct {
	Groups []group `yaml:"groups"`
}

var (
	jobNorm = regexp.MustCompile(`stellarindex-([a-z0-9-]+)`)
	wsNorm  = regexp.MustCompile(`\s+`)
)

// normalize canonicalizes an expression so the two trees' job-label
// conventions compare equal: hyphenated stellarindex-* tokens become
// underscored, and whitespace collapses.
func normalize(expr string) string {
	out := jobNorm.ReplaceAllStringFunc(expr, func(m string) string {
		return strings.ReplaceAll(m, "-", "_")
	})
	return strings.TrimSpace(wsNorm.ReplaceAllString(out, " "))
}

func loadRules(path string) (map[string]rule, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // CI tool — rule-tree paths are CLI args by design
	if err != nil {
		return nil, err
	}
	var rf ruleFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]rule{}
	for _, g := range rf.Groups {
		for _, r := range g.Rules {
			out[r.name()] = r
		}
	}
	return out, nil
}

func loadBaseline(path string) (map[string]bool, error) {
	out := map[string]bool{}
	raw, err := os.ReadFile(path) //nolint:gosec // CI tool — baseline path is a CLI arg by design
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Entries may carry a trailing "  # reason".
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		out[line] = true
	}
	return out, nil
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: lint-rule-equivalence <multi-host-dir> <r1-dir> <baseline>")
		os.Exit(2)
	}
	multiDir, r1Dir, baselinePath := os.Args[1], os.Args[2], os.Args[3]

	baseline, err := loadBaseline(baselinePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	usedBaseline := map[string]bool{}

	allowed := func(key string) bool {
		// A field-level entry is covered by its rule-level parent.
		for _, k := range []string{key, key[:strings.LastIndex(key, ":")]} {
			if baseline[k] {
				usedBaseline[k] = true
				return true
			}
		}
		return false
	}

	r1Files, err := filepath.Glob(filepath.Join(r1Dir, "*.yml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sort.Strings(r1Files)

	failures := 0
	fail := func(format string, args ...any) {
		fmt.Printf("DIVERGED: "+format+"\n", args...)
		failures++
	}

	for _, r1Path := range r1Files {
		compareFile(multiDir, r1Path, allowed, fail)
	}

	// Stale baseline entries fail — shrink-only.
	stale := make([]string, 0)
	for k := range baseline {
		if !usedBaseline[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		fmt.Printf("STALE-BASELINE: %s no longer diverges — remove it from %s\n", k, baselinePath)
		failures++
	}

	if failures > 0 {
		fmt.Printf("\nlint-rule-equivalence: %d divergence(s). Fix the tree that is wrong, or — if the divergence is intentional for the r1 host shape — add '<file>:<rule>:<field>  # reason' to %s (growth requires a Baseline-Growth: commit trailer).\n", failures, baselinePath)
		os.Exit(1)
	}
	fmt.Println("lint-rule-equivalence: the two rule trees are semantically equivalent.")
}

// compareFile diffs one paired rule file's alert/record rules across
// the two trees, reporting divergences through fail (respecting the
// baseline via allowed). A missing multi-host sibling is skipped —
// file PAIRING is lint-docs.sh's job.
//
//nolint:gocognit // presence/expr/for/labels checks are one linear ladder per rule; splitting them hides the diff semantics
func compareFile(multiDir, r1Path string, allowed func(string) bool, fail func(string, ...any)) {
	base := filepath.Base(r1Path)
	multiPath := filepath.Join(multiDir, base)
	if _, err := os.Stat(multiPath); os.IsNotExist(err) { //nolint:gosec // CI tool — dir roots are CLI args by design
		return
	}
	r1Rules, err := loadRules(r1Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	multiRules, err := loadRules(multiPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	names := map[string]bool{}
	for n := range r1Rules {
		names[n] = true
	}
	for n := range multiRules {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, n := range sorted {
		key := base + ":" + n
		r1r, inR1 := r1Rules[n]
		mr, inMulti := multiRules[n]
		switch {
		case !inR1:
			if !allowed(key + ":presence") {
				fail("%s: rule %q exists in multi-host but not in the r1 overlay", base, n)
			}
			continue
		case !inMulti:
			if !allowed(key + ":presence") {
				fail("%s: rule %q exists in the r1 overlay but not in multi-host", base, n)
			}
			continue
		}
		if normalize(r1r.Expr) != normalize(mr.Expr) && !allowed(key+":expr") {
			fail("%s: %q expr differs\n  multi: %s\n  r1:    %s", base, n, normalize(mr.Expr), normalize(r1r.Expr))
		}
		if r1r.For != mr.For && !allowed(key+":for") {
			fail("%s: %q `for` differs (multi=%q r1=%q)", base, n, mr.For, r1r.For)
		}
		if !labelsEqual(r1r.Labels, mr.Labels) && !allowed(key+":labels") {
			fail("%s: %q labels differ (multi=%v r1=%v)", base, n, mr.Labels, r1r.Labels)
		}
	}
}
