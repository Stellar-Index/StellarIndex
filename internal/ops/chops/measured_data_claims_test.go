// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"regexp"
	"strings"
	"testing"
)

// ─── Contributor guidance must state the MEASURED data floors ────────
//
// The docs a contributor (or an agent) reads cold to learn "what does
// this system actually hold" carried four data facts that a read-only
// sweep of r1 contradicted. Each one is load-bearing in the same way:
// it is the number someone sizes a backfill, a restore or a reconcile
// scope against, so a wrong one is not a typo — it plans work against
// history that was never materialised, or hides history that was.
//
//   - `prices_1d` does NOT span back to 2015. It starts 2018-07-01 with
//     a single pair (crypto:XLM/fiat:USD, 946 bars) and holds nothing
//     between 2021-02-01 and 2024-03-10.
//   - pre-P23 classic movement IS reconstructed. The unified-event
//     decoder does not parse it, but `classic-movements-backfill`
//     derives it from operations + operation_results, and
//     stellar.account_movements holds 6,702,108,079 `classic_derived`
//     rows from ledger 3.
//   - trades floors are PER SOURCE, not one ~61.5M number: sdex begins
//     at 61,609,957, soroswap ten million ledgers earlier at 50,746,445.
//   - `fx_quotes` is DAILY, not hourly. The worker polls hourly and then
//     buckets every write to 00:00Z.
//
// This guard lives here because [targetScope]'s own doc comment is one
// of the four sites, and because nothing else in the build can fail on
// a sentence: the drift is a documentation drift, and the only way to
// catch it is to pin the corrected text. Assertions run over
// whitespace-collapsed content so re-flowing a paragraph (or re-wrapping
// a Go comment) cannot hollow the check out, and each site asserts BOTH
// that the false sentence is gone and that its correction is present —
// a deletion is not a fix.
func TestContributorGuidanceStatesTheMeasuredDataFloors(t *testing.T) {
	cases := []struct {
		path string
		// forbidden are the sentences r1 contradicts.
		forbidden []string
		// required are the corrections, so a revert is caught too.
		required []string
	}{
		{
			path:      "docs/architecture/ha-plan.md",
			forbidden: []string{"daily OHLC spans back to 2015"},
			required: []string{
				"`prices_1d` starts **2018-07-01**",
				"between 2021-02-01 and 2024-03-10",
			},
		},
		{
			path: "docs/architecture/domain-traps.md",
			forbidden: []string{
				"writes hourly fiat rates to `fx_quotes`",
				"there is no operations+effects fallback",
			},
			required: []string{
				"writes **daily** fiat rates to `fx_quotes`",
				"one row per ticker per UTC day",
				"`provenance='classic_derived'` spanning ledgers 3 → 58,762,516",
			},
		},
		{
			path: "internal/ops/chops/compute_completeness.go",
			forbidden: []string{
				"soroswap/sdex trades begin ~61.5M",
				"holds no trades below ~61.5M",
			},
			required: []string{
				"sdex trades begin at ledger 61,609,957",
				"soroswap trades begin at 50,746,445",
			},
		},
		{
			// The registry comment is where docs/architecture/domain-traps.md
			// sends a reader for the full detail, so the corrected FX cadence
			// must hold there too.
			path:      "internal/sources/external/registry.go",
			forbidden: []string{"writes hourly fiat rates"},
			required: []string{
				"Truncate(24 * time.Hour)",
				"one row per ticker per UTC day",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			text := flattenProse(readRepoFile(t, tc.path))
			for _, claim := range tc.forbidden {
				if strings.Contains(text, claim) {
					t.Errorf("%s still states %q, which a read-only measurement of r1 contradicts", tc.path, claim)
				}
			}
			for _, claim := range tc.required {
				if !strings.Contains(text, claim) {
					t.Errorf("%s no longer states the measured fact %q", tc.path, claim)
				}
			}
		})
	}
}

// goLineComment strips a leading `//` marker so a Go doc comment reads as
// one paragraph. Anchored to the start of a line so a `https://` inside
// the prose survives.
var goLineComment = regexp.MustCompile(`(?m)^[\t ]*//[\t ]?`)

// flattenProse renders markdown or a Go comment block as a single
// whitespace-normalised line, so the matchers above are insensitive to
// where the text happens to wrap.
func flattenProse(s string) string {
	return strings.Join(strings.Fields(goLineComment.ReplaceAllString(s, "")), " ")
}
