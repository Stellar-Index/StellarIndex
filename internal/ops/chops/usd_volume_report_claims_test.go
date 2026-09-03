// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
	"time"
)

// ─── #372 F1: the verify footer must describe the bound it actually runs ──

// TestUSDVolumeFooter_QuotesTheBoundsRealFiringThresholds is the lockstep
// guard between [xlmBaseBoundTolerance] and the prose the operator reads.
//
// The pre-fix footer told operators the XLM-base bound "catches 10x+
// errors" and "cannot false-alarm on intraday movement". Both are false at
// ±30%: it fires at 1.30× overstatement / 1.43× understatement, and its
// immunity to intraday movement is a measured 0.08 of headroom (worst
// honest day 1.2206 over 120 days of r1 data), not a structural property.
// That wrong footer is what generated issue #372's own hypothesis — that a
// 1.3–1.7 ratio must be dispersion rather than error.
//
// The thresholds are DERIVED from the constant here, so changing the
// tolerance without updating the text fails this test rather than shipping
// a footer that lies by a different amount.
func TestUSDVolumeFooter_QuotesTheBoundsRealFiringThresholds(t *testing.T) {
	t.Parallel()
	footer := usdVolumeFooterText(0)

	over := xlmBaseBoundFiresOver().FloatString(2)
	under := xlmBaseBoundFiresUnder().FloatString(2)
	if over != "1.30" || under != "1.43" {
		t.Fatalf("derived thresholds = (%s, %s), want (1.30, 1.43) at the shipped ±30%% tolerance", over, under)
	}
	for _, want := range []string{
		over + "x overstatement",
		under + "x understatement",
		"NOT structurally immune to intraday movement",
		"1.2206", // the measured worst honest day
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer does not state %q:\n%s", want, footer)
		}
	}
	// The two claims that were wrong must be gone, not merely qualified.
	for _, banned := range []string{"cannot false-alarm", "catches 10x+ errors"} {
		if strings.Contains(footer, banned) {
			t.Errorf("footer still claims %q, which is false at ±30%%", banned)
		}
	}
}

// ─── #372 F3/F5: the restamp report must name its own follow-up ──────────

// TestXLMBaseRestampFollowUp_OrdersPricesFirstAndTwapsLast is the #372-F3
// regression. The restamp's printed `acceptance:` step runs
// verify-usd-volume, which reads `trades` DIRECTLY — while every served
// volume surface reads a continuous aggregate that will not auto-refresh
// a months-old window (measured start_offset on r1: prices_1m 5 min,
// nothing further back than 7 days). So the acceptance check can go green
// while /v1/markets, asset volume, venue rankings and every chart serve
// pre-restamp numbers indefinitely. The report must therefore print the
// refresh sequence itself.
//
// The ORDER is the load-bearing part and the reason this is a test rather
// than a doc: twap_1h and twap_1d are the only hierarchical aggregates in
// the set (parent_mat_hypertable_id → prices_1m's materialisation), so
// refreshing them before prices_1m re-materialises them from stale input.
func TestXLMBaseRestampFollowUp_OrdersPricesFirstAndTwapsLast(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	out := xlmBaseRestampFollowUp(from, to)

	// Every trades-rooted continuous aggregate on r1 must be named. A
	// missing one is a surface that silently keeps serving stale volume.
	want := []string{
		"prices_1m", "prices_15m", "prices_1h", "prices_4h", "prices_1d",
		"prices_1w", "prices_1mo", "dex_volume_by_pair_1d",
		"source_volume_1h", "pools_per_source_1h", "twap_1h", "twap_1d",
	}
	idx := map[string]int{}
	for _, name := range want {
		call := "refresh_continuous_aggregate('" + name + "'"
		i := strings.Index(out, call)
		if i < 0 {
			t.Fatalf("follow-up never refreshes %s:\n%s", name, out)
		}
		idx[name] = i
	}
	for _, dependent := range []string{"twap_1h", "twap_1d"} {
		if idx[dependent] < idx["prices_1m"] {
			t.Errorf("%s is refreshed BEFORE prices_1m — it is materialised from prices_1m, "+
				"so it would rebuild from stale input", dependent)
		}
	}

	// Windows must clear Timescale's 2-bucket minimum per aggregate, or
	// the call is REJECTED (SQLSTATE 22023) and the operator skips that
	// aggregate — shipping a partially-stale surface. The 200-day window
	// above satisfies every minimum on its own, so exercise the narrow
	// case: a ONE-day restamp must still pad prices_1mo out to 93 days.
	narrow := xlmBaseRestampFollowUp(from, from)
	lo, hi := parseRefreshWindow(t, narrow, "prices_1mo")
	if got := hi.Sub(lo); got < 93*24*time.Hour {
		t.Errorf("a one-day restamp padded prices_1mo to %v, want >= 93 days", got)
	}
	if lo.After(from) || hi.Before(from.AddDate(0, 0, 1)) {
		t.Errorf("padded prices_1mo window [%s, %s] does not cover the restamped day", lo, hi)
	}
}

// parseRefreshWindow pulls the two RFC3339 bounds out of one printed
// `CALL refresh_continuous_aggregate('<view>', '<lo>', '<hi>');` line.
func parseRefreshWindow(t *testing.T, report, view string) (time.Time, time.Time) {
	t.Helper()
	prefix := "refresh_continuous_aggregate('" + view + "', '"
	i := strings.Index(report, prefix)
	if i < 0 {
		t.Fatalf("no refresh call for %s in:\n%s", view, report)
	}
	rest := report[i+len(prefix):]
	args := strings.SplitN(rest, "'", 4)
	if len(args) < 4 {
		t.Fatalf("malformed refresh call for %s: %q", view, rest)
	}
	lo, err := time.Parse(time.RFC3339, args[0])
	if err != nil {
		t.Fatalf("%s lower bound %q: %v", view, args[0], err)
	}
	hi, err := time.Parse(time.RFC3339, args[2])
	if err != nil {
		t.Fatalf("%s upper bound %q: %v", view, args[2], err)
	}
	return lo, hi
}

// TestXLMBaseRestampFollowUp_RecommendsMinRelDelta is #372 F5. The
// re-derive reads the FINALISED prices_1m bucket while the original
// insert read the partially-materialised real-time bucket for the same
// minute, so ~8% of the write set moves by under 0.1% — repairing nothing,
// on a compressed hypertable where the write is the expensive part.
// -min-rel-delta 0.001 drops exactly that, and never a NULL fill (pinned
// behaviourally by
// timescale.TestXLMBaseRestampDecide_MinRelDeltaSuppressesSmallMovesOnly).
func TestXLMBaseRestampFollowUp_RecommendsMinRelDelta(t *testing.T) {
	t.Parallel()
	out := xlmBaseRestampFollowUp(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"-min-rel-delta 0.001", "2,311,219", "never suppresses a NULL fill"} {
		if !strings.Contains(out, want) {
			t.Errorf("follow-up does not state %q:\n%s", want, out)
		}
	}
}
