// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// verifyUSDVolume is the stellarindex-ops `verify-usd-volume` subcommand —
// the VALUE half of the usd_volume checks (C4-055 / C4-066,
// audit-2026-07-23).
//
// The standing usd-volume alerts (configs/prometheus/rules.r1/
// usd-volume-coverage.yml) are a COVERAGE check: the ratio of trades
// inserted with a non-NULL `usd_volume`. That catches "we stopped pricing
// this venue" and nothing else. A trade priced with the WRONG number is
// 100% covered and completely wrong, and every volume surface this system
// publishes — DEX volume, asset volume, venue rankings, market share,
// transfer volume — is a sum of that column.
//
// This tool is deliberately split in two, because the column's five tiers
// (see timescale.tradeUSDVolume) are not all the same kind of number:
//
//	CHECKED — the exact tiers. Tiers 1/2 (quote leg USD-pegged) and 2b
//	  (base leg USD-pegged) are pure decimal rescalings of an amount
//	  already on the row: usd_volume = pegged_leg / 10^decimals. No price
//	  lookup, no time alignment, therefore NO TOLERANCE TO ARGUE ABOUT. A
//	  mismatch is a defect — wrong scale, wrong leg, stale peg list, or a
//	  superseded backfill vintage — and it FAILS this command.
//
//	MEASURED — the estimated tiers. Tiers 3/4 value the trade from an FX
//	  rate or the XLM anchor at trade time. Legitimately inexact, and this
//	  repo has already measured two valuation routes diverging by up to
//	  134.92% on the same trades. Their sums and row counts are PRINTED so
//	  an operator can read the real distribution off production, but they
//	  are not judged: the threshold that would judge them is exactly the
//	  number this run exists to produce.
//
// Report-only by design in every other respect: read-only against Postgres,
// no alert rule, no calibrated threshold. The operator follow-up is printed
// in the report footer.
//
// Usage: verify-usd-volume [-config PATH] [-day YYYY-MM-DD] [-days N]
// [-min-rows N] [-max-list N].
//
// Exit: non-zero iff an EXACT-tier identity is violated. A clean run says
// nothing about tier-3/4 accuracy and the report says so out loud.
func verifyUSDVolume(args []string) error {
	fs := flag.NewFlagSet("verify-usd-volume", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml (Postgres DSN + the operator's USD peg list)")
	dayFlag := fs.String("day", "", "last UTC day to check, YYYY-MM-DD (default: yesterday — today is still being written)")
	days := fs.Int("days", 1, "how many consecutive UTC days to check, ending at -day")
	minRows := fs.Int64("min-rows", 1, "skip (source, base, quote) groups with fewer than this many priced rows; raises signal on dust pairs without hiding them from the totals")
	maxList := fs.Int("max-list", 20, "max per-group violation lines to print per day")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *days < 1 {
		return fmt.Errorf("verify-usd-volume: -days must be >= 1, got %d", *days)
	}

	end, err := resolveUSDVolumeDay(*dayFlag)
	if err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	// The SAME peg inputs the insert path is configured with. A tool that
	// built its own peg set would "verify" a different waterfall than the
	// one that wrote the rows — the reimplementation trap.
	spec, err := timescale.NewUSDVolumeQuoteSpec(cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("verify-usd-volume: usd-volume quote spec: %w", err)
	}

	var violations int
	for i := *days - 1; i >= 0; i-- {
		day := end.AddDate(0, 0, -i)
		n, derr := verifyUSDVolumeDay(ctx, store, spec, day, *minRows, *maxList)
		if derr != nil {
			return derr
		}
		violations += n
	}

	printUSDVolumeFooter(violations)
	if violations > 0 {
		return fmt.Errorf("verify-usd-volume: %d exact-tier valuation violation(s) — see above", violations)
	}
	return nil
}

// resolveUSDVolumeDay parses -day, defaulting to YESTERDAY: today's chunk is
// still being written, so a today-scoped run races the ingest path and would
// report phantom deltas on rows the aggregator has not finished valuing.
func resolveUSDVolumeDay(flagVal string) (time.Time, error) {
	if flagVal == "" {
		return time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour), nil
	}
	day, err := time.Parse(time.DateOnly, flagVal)
	if err != nil {
		return time.Time{}, fmt.Errorf("verify-usd-volume: -day %q: want YYYY-MM-DD: %w", flagVal, err)
	}
	return day.UTC().Truncate(24 * time.Hour), nil
}

// usdVolumeTierRollup accumulates one tier's population for a day.
type usdVolumeTierRollup struct {
	groups       int
	pricedRows   int64
	unpricedRows int64
	sumUSD       *big.Rat
	// worstDelta / worstLabel track the largest |delta| seen on an exact
	// tier, so the report leads with the biggest offender rather than the
	// first one encountered.
	worstDelta *big.Rat
	worstLabel string
}

func newTierRollup() *usdVolumeTierRollup {
	return &usdVolumeTierRollup{sumUSD: new(big.Rat), worstDelta: new(big.Rat)}
}

// verifyUSDVolumeDay checks one UTC day and returns the number of exact-tier
// violations found.
//
//nolint:gocognit // one linear pass: classify → accumulate → judge exact tiers → print.
func verifyUSDVolumeDay(
	ctx context.Context,
	store *timescale.Store,
	spec *timescale.USDVolumeQuoteSpec,
	day time.Time,
	minRows int64,
	maxList int,
) (int, error) {
	groups, err := store.TradeValuationByDay(ctx, day)
	if err != nil {
		return 0, err
	}

	fmt.Printf("\n=== verify-usd-volume: %s (UTC) — %d (source, base, quote) group(s) ===\n",
		day.Format(time.DateOnly), len(groups))

	rollups := map[timescale.USDVolumeTier]*usdVolumeTierRollup{}
	var (
		violations int
		listed     int
		parseErrs  int
	)

	for _, g := range groups {
		tier, decimals, cerr := timescale.ClassifyUSDVolumeTier(g.Source, g.BaseAsset, g.QuoteAsset, spec)
		if cerr != nil {
			// An unparseable asset id on a LANDED trade is its own finding;
			// report it rather than dropping the group silently.
			parseErrs++
			fmt.Fprintf(os.Stderr, "  UNCLASSIFIABLE %s %s/%s: %v\n", g.Source, g.BaseAsset, g.QuoteAsset, cerr)
		}
		r, ok := rollups[tier]
		if !ok {
			r = newTierRollup()
			rollups[tier] = r
		}
		r.groups++
		r.pricedRows += g.PricedRows
		r.unpricedRows += g.UnpricedRows
		if v, vok := new(big.Rat).SetString(g.SumUSDVolume); vok {
			r.sumUSD.Add(r.sumUSD, v)
		}

		if !tier.Exact() || g.PricedRows < minRows {
			continue
		}
		delta, dok := timescale.ExactTierDelta(g, tier, decimals)
		if !dok {
			parseErrs++
			fmt.Fprintf(os.Stderr, "  UNPARSEABLE SUMS %s %s/%s (usd=%q base=%q quote=%q)\n",
				g.Source, g.BaseAsset, g.QuoteAsset, g.SumUSDVolume, g.SumBaseAmount, g.SumQuoteAmount)
			continue
		}
		abs := new(big.Rat).Abs(delta)
		if abs.Cmp(r.worstDelta) > 0 {
			r.worstDelta.Set(abs)
			r.worstLabel = fmt.Sprintf("%s %s/%s", g.Source, g.BaseAsset, g.QuoteAsset)
		}
		slack := timescale.USDVolumeRoundingSlack(decimals, g.PricedRows)
		if abs.Cmp(slack) <= 0 {
			continue
		}
		violations++
		if listed < maxList {
			listed++
			fmt.Printf("  VIOLATION [%s] %s %s/%s rows=%d  stored=%s  expected=%s  delta=%s USD (slack=%s)\n",
				tier, g.Source, g.BaseAsset, g.QuoteAsset, g.PricedRows,
				g.SumUSDVolume,
				new(big.Rat).Sub(mustRat(g.SumUSDVolume), delta).FloatString(8),
				delta.FloatString(8), slack.FloatString(8))
		} else if listed == maxList {
			listed++
			fmt.Printf("  … more violations suppressed (raise -max-list)\n")
		}
	}

	printUSDVolumeTierTable(rollups)
	if parseErrs > 0 {
		fmt.Printf("  %d group(s) could not be classified or parsed — listed on stderr above\n", parseErrs)
	}
	fmt.Printf("%s: %d exact-tier violation(s)\n", day.Format(time.DateOnly), violations)
	return violations, nil
}

// mustRat parses a NUMERIC render that ExactTierDelta already proved
// parseable. Returns zero rather than panicking — this is a report path.
func mustRat(s string) *big.Rat {
	if v, ok := new(big.Rat).SetString(s); ok {
		return v
	}
	return new(big.Rat)
}

// printUSDVolumeTierTable renders the per-tier distribution — the whole
// point of the MEASURED half. Tier order is fixed (not map order) so runs
// diff cleanly against each other.
func printUSDVolumeTierTable(rollups map[timescale.USDVolumeTier]*usdVolumeTierRollup) {
	order := []timescale.USDVolumeTier{
		timescale.TierQuotePegged,
		timescale.TierBasePegged,
		timescale.TierEstimated,
		timescale.TierUnvaluable,
	}
	// Any tier string the classifier grows later still shows up.
	for tier := range rollups {
		known := false
		for _, k := range order {
			if k == tier {
				known = true
				break
			}
		}
		if !known {
			order = append(order, tier)
		}
	}
	sort.SliceStable(order[4:], func(i, j int) bool { return order[4+i] < order[4+j] })

	fmt.Printf("  %-14s %7s %10s %10s %20s %s\n", "TIER", "GROUPS", "PRICED", "UNPRICED", "SUM(usd_volume)", "WORST |Δ| (exact tiers)")
	for _, tier := range order {
		r, ok := rollups[tier]
		if !ok {
			continue
		}
		worst := "-"
		if tier.Exact() && r.worstLabel != "" {
			worst = fmt.Sprintf("%s  (%s)", r.worstDelta.FloatString(8), r.worstLabel)
		}
		fmt.Printf("  %-14s %7d %10d %10d %20s %s\n",
			tier, r.groups, r.pricedRows, r.unpricedRows, r.sumUSD.FloatString(2), worst)
	}
}

// printUSDVolumeFooter states, in the report itself, exactly what this run
// did and did NOT prove, plus the operator follow-up that turns the measured
// half into an alert. Printing it here (rather than only in a runbook) is
// deliberate: the person reading this output is the person who has to make
// that call, and a clean exit code on a money surface must never be mistaken
// for "usd_volume is verified correct".
func printUSDVolumeFooter(violations int) {
	fmt.Printf(`
--- what this run proved ---
CHECKED  quote_pegged + base_pegged: usd_volume == pegged_leg / 10^decimals,
         an exact identity with no tolerance. %d violation(s).
MEASURED estimated (FX / XLM-anchor tiers): sums and row counts printed only.
         NOT judged — these are genuinely inexact and no threshold for them
         has been measured yet.

A clean run does NOT certify that tier-3/4 usd_volume values are correct,
and does not certify coverage (that is the standing
stellarindex_{cex,onchain}_usd_volume_coverage_low alerts).

OPERATOR FOLLOW-UP: run this on r1 across a representative span
(e.g. -days 30), read the tier-3/4 spread out of the per-tier table, and
only then calibrate an alert threshold for the estimated tiers. Landing a
guessed threshold on a money surface is the C6-118 mistake — an
uncalibrated bound either false-fires forever or never fires at all.
`, violations)
}
