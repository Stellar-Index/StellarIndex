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
// this system actually hold" carried five data facts that a read-only
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
//   - NO price aggregate carries a retention policy. Migration 0002
//     placed 30-day policies on `prices_1m` / `prices_15m`; migration
//     0031 removed them, and migration 0116 records the tree as holding
//     none. The backfill refresh set still excludes the minute rungs,
//     and that exclusion now rests on cost — a sentence justifying it by
//     a retention that no longer exists would send a reader to size a
//     restore or a reconcile against a 30-day floor the data does not
//     have.
//
// Two more sites were added 2026-09-04 for the same class with a
// different contradicting witness: the tree rather than r1. The
// backfill's continuous-aggregate refresh set skipped prices_1m and
// prices_15m, justified in two places by a 30-day retention migration
// 0031 removed on 2026-05-14 and then by "nothing reads at that
// resolution" — which /v1/ohlc, /v1/chart and /v1/history all
// contradict, each serving both grains over a caller-chosen window.
// The behaviour is fixed and the set is pinned structurally in
// internal/storage/timescale/cagg_refresh_set_test.go; these two
// entries stop the retired justification returning as prose.
//
// This guard lives here because [targetScope]'s own doc comment is one
// of the five sites, and because nothing else in the build can fail on
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
			// The refresh set's own doc comment. Its first justification
			// for skipping the minute grains was a retention migration
			// 0031 removed; the correction has to name both migrations,
			// or a later reader re-derives the same wrong conclusion
			// from 0002 alone.
			//
			// An earlier entry for this same file pinned the exclusion
			// itself, requiring the comment to say the minute rungs are
			// left out on cost rather than on retention. There is no
			// exclusion left to justify — they are in the set — so that
			// entry is gone and this one carries the ground it covered:
			// both migrations named, and every served rung present.
			path: "internal/storage/timescale/diagnostics.go",
			forbidden: []string{
				"have a 30-day retention by design",
				"so refreshing historical buckets there is wasted work",
				// CAGGCoverage's own comment, ~100 lines below, carried
				// the same expired premise from the other direction.
				"raw trades have a 90-day retention but the hourly+ CAGGs are retained forever",
				// And RefreshContinuousAggregate's own doc, ~20 lines
				// ABOVE the corrected comment, stated it a third time
				// and in the present tense — inside the very function
				// the fix is about, in the file it claims to have
				// de-drifted.
				"the 90-day retention on raw trades drops chunks before the policy's natural cadence picks them up",
			},
			required: []string{
				"migration 0002 gave prices_1m and prices_15m a 30-day retention and migration 0031 removed it on 2026-05-14",
				"Every SERVED rung has to be here",
				"ORDER IS DEFENSIVE, not load-bearing today, and prices_1m leads",
				// The retired retention claim reached a second site
				// 100 lines below the one this pass corrected. Pin the
				// correction so the two cannot diverge again.
				"migration 0031 removed the 90-day retention on raw `trades` and the 30-day retention on prices_1m / prices_15m",
				// And a third, in RefreshContinuousAggregate's doc.
				// The roll-forward policy is the only live reason a
				// refresh is needed; a reader who is given a second
				// one re-derives the wrong repair (re-decode, archive
				// read) for a range whose trades were never dropped.
				"The roll-forward policy is the WHOLE reason",
				"which migration 0031 retired on 2026-05-14 when it removed the 90-day retention",
			},
		},
		{
			// The call site carried the SECOND justification — a cost
			// claim about what the fine grains are read over, which the
			// three surfaces above disprove.
			path: "internal/ops/ingest/backfill.go",
			forbidden: []string{
				"a window nothing reads at that resolution",
				"the 90-day raw-trades retention will drop the just-",
			},
			required: []string{
				"All seven price CAGGs are refreshed (migration 0002)",
				"the CAGG policies only roll forward",
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
					t.Errorf("%s still states %q, which r1 or the tree itself contradicts", tc.path, claim)
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
