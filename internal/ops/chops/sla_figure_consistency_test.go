// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
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
	re := regexp.MustCompile(`Availability</td>\s*<td[^>]*>&ge; ([0-9]+\.[0-9]+) %`)
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
