// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── The published availability figure and the alert budget agree ───
//
// The API's availability commitment is stated in three kinds of place:
// the public page at /sla, the burn-rate alerts that page an operator
// when the error budget is being spent, and the operator documents
// that explain those alerts. For four months they disagreed by an
// order of magnitude — the page promised ≥ 99.9 % while ADR-0008 and
// the alerts operationalised 99.99 %, under a recording-rule label
// named for the tighter figure (#487). Nothing could fail on the
// disagreement because each site is prose or config that no build
// step reads.
//
// The public page is the authority: it is the number a customer holds
// the service to, and the one the sla-probe binary, the load-test
// thresholds and the launch announcement already state. Everything
// else is derived from it here, so a change to the page is the only
// edit that legitimately moves the figure, and that edit fails the
// build until every dependent site follows:
//
//   - both Prometheus rule trees budget against exactly (100 − figure) %
//     of requests and carry a label that names the figure;
//   - the sla-probe binary's default availability target is the figure;
//   - the operator documents that restate the figure or its burn
//     arithmetic state the same number.
//
// Assertions run over whitespace-collapsed content so a re-wrapped
// paragraph cannot hollow the check out, and the prose sites assert
// BOTH that the stale figure is gone and that the corrected one is
// present — a deletion is not a fix.

const (
	slaPage       = "web/explorer/src/app/sla/page.tsx"
	slaProbeMain  = "cmd/stellarindex-sla-probe/main.go"
	sloRulesMulti = "deploy/monitoring/rules/slo.yml"
	sloRulesR1    = "configs/prometheus/rules.r1/slo.yml"
)

// publishedAvailability reads the objective from the public page's
// targets table: the cell that follows the "Availability" row label.
func publishedAvailability(t *testing.T) string {
	t.Helper()
	page := readRepoFile(t, slaPage)
	// Whitespace-tolerant on BOTH sides of the label and around the figure.
	// The first version required `Availability</td>` to be adjacent, so when
	// the tree was reformatted and prettier broke that cell across three
	// lines, this gate stopped matching — a formatter silently disabled a
	// consistency check while the published figure was untouched. The value
	// is what this test is about; its layout is not.
	re := regexp.MustCompile(`Availability\s*</td>\s*<td[^>]*>\s*&ge;\s*([0-9]+\.[0-9]+)\s*%`)
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s no longer states an availability objective as '&ge; NN.N %%' in the targets table", slaPage)
	}
	return m[1]
}

// availabilityBudget derives the error budget the alerts must use from
// the published percentage, in exact decimal arithmetic: 99.9 → 0.001,
// 99.99 → 0.0001. Returned both as the canonical literal and as a
// float for tolerance comparison against whatever literal the rules
// carry.
func availabilityBudget(t *testing.T, pct string) (string, float64) {
	t.Helper()
	whole, frac, ok := strings.Cut(pct, ".")
	if !ok || frac == "" {
		t.Fatalf("availability figure %q is not of the form NN.N", pct)
	}
	scale := int64(math.Pow10(len(frac)))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		t.Fatalf("availability figure %q: %v", pct, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		t.Fatalf("availability figure %q: %v", pct, err)
	}
	budgetScaled := 100*scale - (w*scale + f)
	literal := fmt.Sprintf("0.%0*d", len(frac)+2, budgetScaled)
	return literal, float64(budgetScaled) / float64(100*scale)
}

func TestPublishedAvailabilityFigureGovernsTheAlertBudget(t *testing.T) {
	pct := publishedAvailability(t)
	pctF, err := strconv.ParseFloat(pct, 64)
	if err != nil {
		t.Fatalf("availability figure %q: %v", pct, err)
	}
	budgetLiteral, budget := availabilityBudget(t, pct)
	// 99.9 carries three nines, 99.99 four; the label names the figure so
	// a query against it reads the objective it enforces.
	wantLabel := fmt.Sprintf("api_availability_%d_nines", strings.Count(pct, "9"))

	t.Run("public page states the budget it implies", func(t *testing.T) {
		page := flattenProse(readRepoFile(t, slaPage))
		for _, want := range []string{
			fmt.Sprintf("What %s %% actually permits", pct),
			fmt.Sprintf("A %s %% monthly availability objective", pct),
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s targets table says %s %% but the error-budget section no longer says %q", slaPage, pct, want)
			}
		}
	})

	t.Run("sla-probe default target is the published figure", func(t *testing.T) {
		src := readRepoFile(t, slaProbeMain)
		m := regexp.MustCompile(`defaultAvailabilityT\s*=\s*([0-9.]+)`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s no longer declares defaultAvailabilityT", slaProbeMain)
		}
		got, err := strconv.ParseFloat(m[1], 64)
		if err != nil || math.Abs(got-pctF) > 1e-9 {
			t.Errorf("%s defaultAvailabilityT = %s, want the published %s", slaProbeMain, m[1], pct)
		}
	})

	burnExpr := regexp.MustCompile(`api_error_ratio:[0-9a-z]+\{slo="([a-z0-9_]+)"\} > \((?:14\.4|6|1) \* ([0-9.]+)\)`)
	sloLabel := regexp.MustCompile(`slo: (api_availability_[a-z0-9_]+)`)
	// A rule file's prose is YAML comments; drop the leading marker the
	// way flattenProse drops a Go comment's so a wrapped sentence reads
	// as one line.
	yamlLineComment := regexp.MustCompile(`(?m)^[\t ]*#[\t ]?`)
	for _, path := range []string{sloRulesMulti, sloRulesR1} {
		t.Run(path, func(t *testing.T) {
			raw := readRepoFile(t, path)

			exprs := burnExpr.FindAllStringSubmatch(raw, -1)
			if len(exprs) != 6 {
				t.Fatalf("%s: found %d availability burn comparisons, want 6 (three alerts × two windows)", path, len(exprs))
			}
			for _, m := range exprs {
				if m[1] != wantLabel {
					t.Errorf("%s: burn expression selects slo=%q, want %q (the label must name the published %s %%)", path, m[1], wantLabel, pct)
				}
				got, err := strconv.ParseFloat(m[2], 64)
				if err != nil || math.Abs(got-budget) > 1e-12 {
					t.Errorf("%s: burn expression budgets against %s, want %s — the published page says %s %%", path, m[2], budgetLiteral, pct)
				}
			}

			labels := sloLabel.FindAllStringSubmatch(raw, -1)
			if len(labels) != 8 {
				t.Fatalf("%s: found %d availability slo labels, want 8 (five recording rules + three alerts)", path, len(labels))
			}
			for _, m := range labels {
				if m[1] != wantLabel {
					t.Errorf("%s: rule carries slo: %s, want %s", path, m[1], wantLabel)
				}
			}

			prose := flattenProse(yamlLineComment.ReplaceAllString(raw, ""))
			for _, want := range []string{
				fmt.Sprintf("Availability: %s%% of API requests return non-5xx over 30 days", pct),
				fmt.Sprintf("Budget = %s (1 - ", budgetLiteral),
				fmt.Sprintf("Availability SLO (%s%% non-5xx over 30d) is burning fast.", pct),
			} {
				if !strings.Contains(prose, want) {
					t.Errorf("%s no longer says %q", path, want)
				}
			}
		})
	}
}

// TestOperatorDocsStateThePublishedAvailabilityFigure pins the documents
// a responder or a customer reads that restate the figure or the burn
// arithmetic derived from it. The literals here are the 99.9 % set; a
// legitimate change to the public page fails this test at every site
// that has to follow, which is the point.
func TestOperatorDocsStateThePublishedAvailabilityFigure(t *testing.T) {
	cases := []struct {
		path      string
		forbidden []string
		required  []string
		// quotesOriginalFigure marks a record that legitimately carries
		// the 99.99 % wording: an ADR's accepted text is immutable and is
		// corrected by a dated amendment, and the coverage matrix quotes
		// the proposal's claim verbatim before grading it. Both are pinned
		// on their correction instead of swept.
		quotesOriginalFigure bool
	}{
		{
			path:                 "docs/adr/0008-ha-topology.md",
			required:             []string{"Amendment — 2026-09-04", "the published availability commitment is **≥ 99.9 %**"},
			quotesOriginalFigure: true,
		},
		{
			path:     "docs/architecture/ha-plan.md",
			required: []string{"**≥ 99.9 % availability** — the figure published to customers"},
		},
		{
			path:                 "docs/architecture/coverage-matrix.md",
			forbidden:            []string{"| S9.1 | ≥ 99.99 % uptime |"},
			required:             []string{"| S9.1 | ≥ 99.9 % availability", "the proposal's 99.99 % is not the published commitment"},
			quotesOriginalFigure: true,
		},
		{
			path:     "docs/operations/sla-probe.md",
			required: []string{"| Availability | ≥ 99.9 % | service SLA |"},
		},
		{
			path:      "docs/operations/alerts-catalog.md",
			forbidden: []string{"99.99% non-5xx"},
			required:  []string{"`stellarindex_slo_availability_burn_fast` | 99.9% non-5xx"},
		},
		{
			path:      "docs/operations/runbooks/slo-availability-burn-fast.md",
			forbidden: []string{"(99.99 % non-5xx over 30 d", "slo `api_availability_3_nines_9`", "14.4 × 0.0001"},
			required:  []string{"(99.9 % non-5xx over 30 d", "slo `api_availability_3_nines`)", "14.4 × 0.001 = **1.44 %**"},
		},
		{
			path:      "docs/operations/runbooks/slo-availability-burn-medium.md",
			forbidden: []string{"(99.99 % non-5xx over 30 d", "slo `api_availability_3_nines_9`", "6 × 0.0001"},
			required:  []string{"(99.9 % non-5xx over 30 d", "slo `api_availability_3_nines`)", "6 × 0.001 = 0.6 %"},
		},
		{
			path:      "docs/operations/runbooks/slo-availability-burn-slow.md",
			forbidden: []string{"(99.99 % non-5xx over 30 d", "slo `api_availability_3_nines_9`", "1 × 0.0001"},
			required:  []string{"(99.9 % non-5xx over 30 d", "slo `api_availability_3_nines`)", "1 × 0.001 = 0.1 %"},
		},
		{
			path:      "docs/operations/runbooks/api-5xx.md",
			forbidden: []string{"(99.99 % non-5xx over 30 d)", "14.4 × 0.0001"},
			required:  []string{"(99.9 % non-5xx over 30 d)", "14.4 × 0.001 = 1.44 %"},
		},
		{
			path:     "configs/healthchecks/sla-probe.sh",
			required: []string{"availability ≥ 99.9 %"},
		},
		{
			path:     "deploy/comms/launch-announcement.md",
			required: []string{"≥ 99.9 % availability"},
		},
		{
			path:     "docs/operations/post-launch-queries.md",
			required: []string{"is ≥ 99.9% availability"},
		},
		{
			path:     "test/load/scenarios/lib/thresholds.js",
			required: []string{"ADR-0009 multi-window SLO: 99.9 % availability", "'http_req_failed': ['rate<0.001']"},
		},
	}

	// Beyond the pinned sentences, none of these living documents may
	// describe a 99.99 % objective as the one in force. Dated history
	// lines that mention the figure in passing are not matched.
	staleObjective := regexp.MustCompile(`99\.99 ?% (?:non-5xx|uptime|of API requests|availab)`)

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			text := flattenProse(readRepoFile(t, tc.path))
			for _, claim := range tc.forbidden {
				if strings.Contains(text, claim) {
					t.Errorf("%s still states %q, which the published SLA contradicts", tc.path, claim)
				}
			}
			for _, claim := range tc.required {
				if !strings.Contains(text, claim) {
					t.Errorf("%s no longer states %q", tc.path, claim)
				}
			}
			if !tc.quotesOriginalFigure {
				if m := staleObjective.FindString(text); m != "" {
					t.Errorf("%s describes a 99.99 %% objective (%q); the published commitment is 99.9 %%", tc.path, m)
				}
			}
		})
	}
}

// ─── Every published target is an alert threshold, in both trees ───
//
// The probe measures every target the /sla page publishes and its
// verdict fails on each, so a published bound that no rule compares
// against is a breach nobody is told about: availability and p99 were
// exported and selected by nothing, and the /v1/price freshness alert
// sat above the published structural bound (#741).

const (
	slaProbeTextfile   = "cmd/stellarindex-sla-probe/textfile.go"
	slaProofScript     = "scripts/ops/sla-proof-from-probe.sh"
	slaProbeRulesMulti = "deploy/monitoring/rules/sla-probe.yml"
	slaProbeRulesR1    = "configs/prometheus/rules.r1/sla-probe.yml"
)

// slaProbeUnalertedFamilies are the exported families that state no
// target, each with the reason it needs no rule. Every other family
// the probe exports must be selected by an alert in both trees.
var slaProbeUnalertedFamilies = map[string]string{
	"stellarindex_sla_probe_samples":              "per-run sample count, the denominator of the other families",
	"stellarindex_sla_probe_run_duration_seconds": "wall-clock of the run itself, a diagnostic",
}

type publishedSLATargets struct {
	p95MS, p99MS, availPct, freshSec, closedFreshSec string
}

func publishedSLAPageTargets(t *testing.T) publishedSLATargets {
	t.Helper()
	page := readRepoFile(t, slaPage)
	cell := func(label, value string) string {
		t.Helper()
		re := regexp.MustCompile(regexp.QuoteMeta(label) + `\s*</td>\s*<td[^>]*>\s*` + value)
		m := re.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("%s no longer states the %q objective in the targets table", slaPage, label)
		}
		return m[1]
	}
	closed := regexp.MustCompile(`held to that structural ([0-9]+)-second bound`).FindStringSubmatch(flattenProse(page))
	if closed == nil {
		t.Fatalf("%s no longer states /v1/price's structural freshness bound", slaPage)
	}
	return publishedSLATargets{
		p95MS:          cell("p95 latency", `&le;\s*([0-9]+)\s*ms`),
		p99MS:          cell("p99 latency", `&le;\s*([0-9]+)\s*ms`),
		availPct:       publishedAvailability(t),
		freshSec:       cell("Price freshness", `&le;\s*([0-9]+)\s*s\b`),
		closedFreshSec: closed[1],
	}
}

// alertExprs returns every alert expression in a rule file,
// whitespace-collapsed and newline-joined.
func alertExprs(t *testing.T, path string) string {
	t.Helper()
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				out = append(out, strings.Join(strings.Fields(r.Expr), " "))
			}
		}
	}
	return strings.Join(out, "\n")
}

func TestSLAProbeAlertsEnforceEveryPublishedTarget(t *testing.T) {
	pub := publishedSLAPageTargets(t)
	comparisons := []string{
		fmt.Sprintf(`stellarindex_sla_probe_latency_ms{quantile="0.95"} > %s`, pub.p95MS),
		fmt.Sprintf(`stellarindex_sla_probe_latency_ms{quantile="0.99"} > %s`, pub.p99MS),
		fmt.Sprintf(`stellarindex_sla_probe_availability_pct < %s`, pub.availPct),
		fmt.Sprintf(`stellarindex_sla_probe_freshness_sec{endpoint="price"} > %s`, pub.closedFreshSec),
		fmt.Sprintf(`stellarindex_sla_probe_freshness_sec{endpoint!="price"} > %s`, pub.freshSec),
	}
	families := regexp.MustCompile(`# TYPE (stellarindex_sla_probe_[a-z0-9_]+) gauge`).
		FindAllStringSubmatch(readRepoFile(t, slaProbeTextfile), -1)
	if len(families) == 0 {
		t.Fatalf("%s declares no stellarindex_sla_probe_* families", slaProbeTextfile)
	}
	timerRef := regexp.MustCompile(`[A-Za-z0-9_./-]+\.timer\b`)
	for _, path := range []string{slaProbeRulesMulti, slaProbeRulesR1} {
		t.Run(path, func(t *testing.T) {
			exprs := alertExprs(t, path)
			for _, want := range comparisons {
				if !strings.Contains(exprs, want) {
					t.Errorf("%s: no alert compares %q — the /sla page publishes that bound", path, want)
				}
			}
			for _, f := range families {
				if _, exempt := slaProbeUnalertedFamilies[f[1]]; !exempt && !strings.Contains(exprs, f[1]) {
					t.Errorf("%s: probe family %s is selected by no alert", path, f[1])
				}
			}
			for _, ref := range timerRef.FindAllString(readRepoFile(t, path), -1) {
				if _, err := os.Stat(filepath.Join(repoRoot(t), ref)); err != nil {
					t.Errorf("%s points at %s, which does not exist", path, ref)
				}
			}
		})
	}
}

// TestSLAProbeVerdictUsesThePublishedTargets pins the probe's own
// verdict defaults and the proof script's gate to the same page.
func TestSLAProbeVerdictUsesThePublishedTargets(t *testing.T) {
	pub := publishedSLAPageTargets(t)
	probe := strings.Join(strings.Fields(readRepoFile(t, slaProbeMain)), " ")
	for _, want := range []string{
		fmt.Sprintf("defaultP95Target = %s * time.Millisecond", pub.p95MS),
		fmt.Sprintf("defaultP99Target = %s * time.Millisecond", pub.p99MS),
		fmt.Sprintf("defaultFreshTarget = %s * time.Second", pub.freshSec),
		fmt.Sprintf("defaultClosedBucketFreshTarget = %s * time.Second", pub.closedFreshSec),
	} {
		if !strings.Contains(probe, want) {
			t.Errorf("%s no longer declares %q", slaProbeMain, want)
		}
	}
	script := readRepoFile(t, slaProofScript)
	for _, want := range []string{
		fmt.Sprintf("P95_TARGET_MS = %s.0", pub.p95MS),
		fmt.Sprintf("P99_TARGET_MS = %s.0", pub.p99MS),
		fmt.Sprintf("AVAILABILITY_TARGET_PCT = %s", pub.availPct),
		fmt.Sprintf(`FRESHNESS_TARGET_SEC = {"price": %s.0, "price-tip": %s.0}`, pub.closedFreshSec, pub.freshSec),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s no longer declares %q", slaProofScript, want)
		}
	}
}
