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
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// verifyUSDVolume checks the VALUE of usd_volume (alerts only check coverage). Tiers 1/2/2b are exact rescalings of a USD-pegged leg, so
// any mismatch fails the run; tiers 3/4 are inexact and only printed. Read-only against Postgres.
func verifyUSDVolume(args []string) error {
	fs := flag.NewFlagSet("verify-usd-volume", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml (Postgres DSN + the operator's USD peg list)")
	dayFlag := fs.String("day", "", "last UTC day to check, YYYY-MM-DD (default: yesterday — today is still being written)")
	days := fs.Int("days", 1, "how many consecutive UTC days to check, ending at -day")
	minRows := fs.Int64("min-rows", 1, "skip (source, base, quote) groups with fewer than this many priced rows; raises signal on dust pairs without hiding them from the totals")
	maxList := fs.Int("max-list", 20, "max per-group violation lines to print per day")
	textfile := fs.String("textfile", "", "node_exporter textfile path, e.g. /var/lib/node_exporter/textfile_collector/verify_usd_volume.prom; empty = none")
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

	// Use the insert path's own peg inputs; a separate peg set would verify a different waterfall.
	spec, err := timescale.NewUSDVolumeQuoteSpec(cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("verify-usd-volume: usd-volume quote spec: %w", err)
	}

	var violations int
	var lastRollups map[timescale.USDVolumeTier]*usdVolumeTierRollup
	for i := *days - 1; i >= 0; i-- {
		day := end.AddDate(0, 0, -i)
		n, rollups, derr := verifyUSDVolumeDay(ctx, store, spec, day, *minRows, *maxList)
		if derr != nil {
			return derr
		}
		violations += n
		lastRollups = rollups
	}

	return publishUSDVolumeRun(*textfile, end, lastRollups, violations, time.Now())
}

// publishUSDVolumeRun stamps the textfile before the violation exit so a failing run still
// publishes its tiers.
func publishUSDVolumeRun(textfile string, end time.Time, rollups map[timescale.USDVolumeTier]*usdVolumeTierRollup, violations int, now time.Time) error {
	if textfile != "" {
		if werr := writeAtomic(textfile, renderVerifyUSDVolumeProm(end, rollups, violations, now)); werr != nil {
			return fmt.Errorf("verify-usd-volume: write textfile: %w", werr)
		}
	}
	printUSDVolumeFooter(violations)
	if violations > 0 {
		return fmt.Errorf("verify-usd-volume: %d valuation violation(s) — see above", violations)
	}
	return nil
}

// xlmBaseBoundTolerance: stored Σusd_volume must be within [1-tol, 1+tol] of Σbase_amount/1e7 × the day's XLM/USD VWAP; a bound, not an
// identity. Trades price per minute, the bound at day VWAP, so honest days drift (worst 1.2206); below ~1.25 needs a notional floor.
// $0.00 both sides is exempt.
const xlmBaseBoundTolerance = 0.30

// xlmBaseBoundDustCeiling is half a cent: below it a value renders as
// $0.00, and a ratio between two such values is sub-cent quantisation.
var xlmBaseBoundDustCeiling = big.NewRat(1, 200)

// xlmBaseBoundIsDust reports whether both sides round to $0.00. It requires BOTH: a $160 group stored at $0.0002, or $0.001 stored at $5,
// is a valuation error at any size.
func xlmBaseBoundIsDust(stored, expected *big.Rat) bool {
	return new(big.Rat).Abs(stored).Cmp(xlmBaseBoundDustCeiling) < 0 &&
		new(big.Rat).Abs(expected).Cmp(xlmBaseBoundDustCeiling) < 0
}

// xlmBaseLegScale returns the denominator lifting a raw XLM base-amount sum to whole XLM; the scale is a connector property (DEX 1e7, CEX
// 1e8, FX 1e6), dispatched like the insert path's usdVolumeDecimals.
func xlmBaseLegScale(source string) *big.Rat {
	baseDecimals := int64(7) // on-chain stroop scale
	md := external.Lookup(source)
	switch md.Subclass {
	case external.SubclassCEX, external.SubclassFX:
		baseDecimals = int64(md.AmountScaleDecimals())
	default:
		// SubclassDEX and anything future: on-chain stroops.
	}
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(baseDecimals), nil))
}

// checkXLMBaseBound judges estimated-tier groups whose base leg is XLM (any alias) and returns
// the violation count. Sub-cent breaches are printed, not counted.
func checkXLMBaseBound(groups []timescale.TradeValuationGroup, spec *timescale.USDVolumeQuoteSpec, dayVWAP *big.Rat, minRows int64, maxList int) int {
	return checkXLMLegBound(groups, spec, dayVWAP, minRows, maxList, "XLM-BASE",
		func(g timescale.TradeValuationGroup) (asset, amount string) { return g.BaseAsset, g.SumBaseAmount })
}

// checkXLMQuoteBound mirrors checkXLMBaseBound for DEX trades with XLM in the quote leg, which
// the base check can never match.
func checkXLMQuoteBound(groups []timescale.TradeValuationGroup, spec *timescale.USDVolumeQuoteSpec, dayVWAP *big.Rat, minRows int64, maxList int) int {
	return checkXLMLegBound(groups, spec, dayVWAP, minRows, maxList, "XLM-QUOTE",
		func(g timescale.TradeValuationGroup) (asset, amount string) { return g.QuoteAsset, g.SumQuoteAmount })
}

// checkXLMLegBound is the shared body of the base and quote bounds; leg picks the judged side
// and label names the bound in output.
func checkXLMLegBound(groups []timescale.TradeValuationGroup, spec *timescale.USDVolumeQuoteSpec, dayVWAP *big.Rat, minRows int64, maxList int, label string, leg func(timescale.TradeValuationGroup) (asset, amount string)) int {
	xlmForms := map[string]bool{}
	for _, a := range canonical.AssetAliases(canonical.NativeAsset()) {
		xlmForms[a.String()] = true
	}
	tolLo := new(big.Rat).SetFloat64(1 - xlmBaseBoundTolerance)
	tolHi := new(big.Rat).SetFloat64(1 + xlmBaseBoundTolerance)

	var violations, listed, dust int
	for _, g := range groups {
		legAsset, legAmount := leg(g)
		if !xlmForms[legAsset] || g.PricedRows < minRows {
			continue
		}
		stored, expected, ok := xlmLegStoredAndExpected(g, spec, dayVWAP, legAmount)
		if !ok {
			continue
		}
		lo := new(big.Rat).Mul(expected, tolLo)
		hi := new(big.Rat).Mul(expected, tolHi)
		if stored.Cmp(lo) >= 0 && stored.Cmp(hi) <= 0 {
			continue
		}
		if xlmBaseBoundIsDust(stored, expected) {
			dust++
			continue
		}
		violations++
		if listed < maxList {
			listed++
			ratio := new(big.Rat).Quo(stored, expected)
			fmt.Printf("  %s BOUND VIOLATION %s %s/%s rows=%d  stored=%s  expected≈%s  ratio=%s\n",
				label, g.Source, g.BaseAsset, g.QuoteAsset, g.PricedRows,
				stored.FloatString(2), expected.FloatString(2), ratio.FloatString(4))
		} else if listed == maxList {
			listed++
			fmt.Printf("  … more %s bound violations suppressed (raise -max-list)\n", label)
		}
	}
	if dust > 0 {
		fmt.Printf("  %s BOUND: %d sub-cent breach(es) not counted — stored and expected both round to $0.00\n", label, dust)
	}
	return violations
}

// xlmLegStoredAndExpected returns a group's stored Σusd_volume and expected Σleg × dayVWAP; ok
// is false if the group is not estimated-tier or a side is unparseable or non-positive.
func xlmLegStoredAndExpected(g timescale.TradeValuationGroup, spec *timescale.USDVolumeQuoteSpec, dayVWAP *big.Rat, legAmount string) (stored, expected *big.Rat, ok bool) {
	tier, _, cerr := timescale.ClassifyUSDVolumeTier(g.Source, g.BaseAsset, g.QuoteAsset, spec)
	if cerr != nil || tier != timescale.TierEstimated {
		return nil, nil, false
	}
	stored, sok := new(big.Rat).SetString(g.SumUSDVolume)
	leg, lok := new(big.Rat).SetString(legAmount)
	if !sok || !lok || leg.Sign() <= 0 {
		return nil, nil, false
	}
	expected = new(big.Rat).Quo(leg, xlmBaseLegScale(g.Source))
	expected.Mul(expected, dayVWAP)
	if expected.Sign() <= 0 {
		return nil, nil, false
	}
	return stored, expected, true
}

// resolveUSDVolumeDay parses -day, defaulting to yesterday because today's rows are still being
// valued and would show phantom deltas.
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

// classifyExactTierGroups classifies one day's groups and checks each exact-tier identity, returning violation, judged and unparseable
// counts plus tier rollups. The caller must fold parseErrs into violations so a mis-spelled asset id cannot leave a clean exit.
//
//nolint:gocognit // one linear pass: classify → accumulate → judge exact tiers → print.
func classifyExactTierGroups(
	groups []timescale.TradeValuationGroup,
	spec *timescale.USDVolumeQuoteSpec,
	minRows int64,
	maxList int,
) (violations, judged, parseErrs int, rollups map[timescale.USDVolumeTier]*usdVolumeTierRollup) {
	rollups = map[timescale.USDVolumeTier]*usdVolumeTierRollup{}
	listed := 0

	for _, g := range groups {
		tier, decimals, cerr := timescale.ClassifyUSDVolumeTier(g.Source, g.BaseAsset, g.QuoteAsset, spec)
		if cerr != nil {
			// The error path demotes the group to TierEstimated, which would skip the judged
			// population, so the caller counts this as a violation.
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
		} else {
			parseErrs++
			fmt.Fprintf(os.Stderr, "  UNPARSEABLE USD SUM %s %s/%s (usd=%q)\n", g.Source, g.BaseAsset, g.QuoteAsset, g.SumUSDVolume)
		}

		if !tier.Exact() || g.PricedRows < minRows {
			continue
		}
		judged++
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
	return violations, judged, parseErrs, rollups
}

// usdVolumeTotalViolations folds parseErrs into the violation total: an unclassifiable group is
// a defect, and otherwise a day of only such groups would exit 0.
func usdVolumeTotalViolations(exactViolations, xlmBoundViolations, parseErrs int) int {
	return exactViolations + xlmBoundViolations + parseErrs
}

// verifyUSDVolumeDay checks one UTC day and returns the number of exact-tier
// violations found.
func verifyUSDVolumeDay(
	ctx context.Context,
	store *timescale.Store,
	spec *timescale.USDVolumeQuoteSpec,
	day time.Time,
	minRows int64,
	maxList int,
) (int, map[timescale.USDVolumeTier]*usdVolumeTierRollup, error) {
	groups, err := store.TradeValuationByDay(ctx, day)
	if err != nil {
		return 0, nil, err
	}

	fmt.Printf("\n=== verify-usd-volume: %s (UTC) — %d (source, base, quote) group(s) ===\n",
		day.Format(time.DateOnly), len(groups))

	violations, judged, parseErrs, rollups := classifyExactTierGroups(groups, spec, minRows, maxList)

	nb, err := verifyXLMBounds(ctx, store, groups, spec, day, minRows, maxList)
	if err != nil {
		return 0, nil, err
	}
	violations = usdVolumeTotalViolations(violations, nb, parseErrs)
	if len(groups) == 0 {
		// A day with no trade groups at all judged nothing; a quiet pass here would hide a broken read.
		fmt.Println("  EMPTY DAY: no trade groups at all — the check judged nothing")
		violations++
	}

	printUSDVolumeTierTable(rollups)
	fmt.Printf("  judged=%d of %d group(s) against an exact-tier identity\n", judged, len(groups))
	if parseErrs > 0 {
		fmt.Printf("  %d group(s) could not be classified or parsed — listed on stderr above\n", parseErrs)
	}
	fmt.Printf("%s: %d violation(s)\n", day.Format(time.DateOnly), violations)
	return violations, rollups, nil
}

// knownUSDVolumeTiers are always emitted, 0 when a day had none, so the
// estimated-tier alert has a series to read even on a quiet day.
var knownUSDVolumeTiers = []timescale.USDVolumeTier{
	timescale.TierQuotePegged, timescale.TierBasePegged, timescale.TierEstimated, timescale.TierUnvaluable,
}

// renderVerifyUSDVolumeProm renders the LAST checked day's tier rollups and
// the run's violation count as node_exporter textfile exposition.
func renderVerifyUSDVolumeProm(
	day time.Time,
	rollups map[timescale.USDVolumeTier]*usdVolumeTierRollup,
	violations int,
	now time.Time,
) string {
	tiers := append([]timescale.USDVolumeTier(nil), knownUSDVolumeTiers...)
	for t := range rollups {
		known := false
		for _, k := range knownUSDVolumeTiers {
			if k == t {
				known = true
				break
			}
		}
		if !known {
			tiers = append(tiers, t)
		}
	}
	sort.SliceStable(tiers[len(knownUSDVolumeTiers):], func(i, j int) bool {
		return tiers[len(knownUSDVolumeTiers)+i] < tiers[len(knownUSDVolumeTiers)+j]
	})

	var b strings.Builder
	b.WriteString("# HELP stellarindex_verify_usd_volume_tier_usd Sum of usd_volume on the last checked UTC day, per reconcile tier.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_tier_usd gauge\n")
	for _, t := range tiers {
		sum := "0"
		if r := rollups[t]; r != nil {
			sum = r.sumUSD.FloatString(2)
		}
		fmt.Fprintf(&b, "stellarindex_verify_usd_volume_tier_usd{tier=%q} %s\n", string(t), sum)
	}
	b.WriteString("# HELP stellarindex_verify_usd_volume_tier_groups (source, base, quote) groups on the last checked UTC day, per reconcile tier.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_tier_groups gauge\n")
	for _, t := range tiers {
		var groups int
		if r := rollups[t]; r != nil {
			groups = r.groups
		}
		fmt.Fprintf(&b, "stellarindex_verify_usd_volume_tier_groups{tier=%q} %d\n", string(t), groups)
	}
	b.WriteString("# HELP stellarindex_verify_usd_volume_tier_rows Trade rows on the last checked UTC day, per reconcile tier and whether usd_volume is set.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_tier_rows gauge\n")
	for _, t := range tiers {
		var priced, unpriced int64
		if r := rollups[t]; r != nil {
			priced, unpriced = r.pricedRows, r.unpricedRows
		}
		fmt.Fprintf(&b, "stellarindex_verify_usd_volume_tier_rows{tier=%q,priced=\"yes\"} %d\n", string(t), priced)
		fmt.Fprintf(&b, "stellarindex_verify_usd_volume_tier_rows{tier=%q,priced=\"no\"} %d\n", string(t), unpriced)
	}
	b.WriteString("# HELP stellarindex_verify_usd_volume_violations Exact-tier valuation violations found by the last run, summed over its days.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_violations gauge\n")
	fmt.Fprintf(&b, "stellarindex_verify_usd_volume_violations %d\n", violations)
	b.WriteString("# HELP stellarindex_verify_usd_volume_day_unix Start of the last checked UTC day.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_day_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_verify_usd_volume_day_unix %d\n", day.UTC().Truncate(24*time.Hour).Unix())
	b.WriteString("# HELP stellarindex_verify_usd_volume_last_run_unix When verify-usd-volume last completed its checks.\n")
	b.WriteString("# TYPE stellarindex_verify_usd_volume_last_run_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_verify_usd_volume_last_run_unix %d\n", now.Unix())
	return b.String()
}

// verifyXLMBounds runs the XLM-base and XLM-quote bounds for day against its
// CEX-fed XLM/USD VWAP and returns their combined violation count.
func verifyXLMBounds(
	ctx context.Context,
	store *timescale.Store,
	groups []timescale.TradeValuationGroup,
	spec *timescale.USDVolumeQuoteSpec,
	day time.Time,
	minRows int64,
	maxList int,
) (int, error) {
	violations := 0
	// Exact() covers only the pegged tiers, so the XLM bounds are the only judgement of the
	// estimated tiers, checked against the day's CEX-fed XLM/USD VWAP.
	rate, rateOK, rerr := store.DayCloseVWAPXLMUSD(ctx, day)
	if rerr != nil {
		return 0, rerr
	}
	if !rateOK {
		fmt.Printf("  XLM-BASE BOUND: skipped — no crypto:XLM/fiat:USD prices_1d bucket for this day\n")
	} else if rateRat, ok := new(big.Rat).SetString(rate); !ok || rateRat.Sign() <= 0 {
		fmt.Printf("  XLM-BASE BOUND: skipped — unusable day VWAP %q\n", rate)
	} else {
		n := checkXLMBaseBound(groups, spec, rateRat, minRows, maxList)
		if n > 0 {
			fmt.Printf("  XLM-BASE BOUND: %d violation(s) at ±%.0f%% vs day VWAP %s\n",
				n, xlmBaseBoundTolerance*100, rateRat.FloatString(6))
		}
		violations += n

		// Without the quote bound the xlm-quote population would be unjudged, and the
		// acceptance line would report 0 violations for a tier it never looked at.
		nq := checkXLMQuoteBound(groups, spec, rateRat, minRows, maxList)
		if nq > 0 {
			fmt.Printf("  XLM-QUOTE BOUND: %d violation(s) at ±%.0f%% vs day VWAP %s\n",
				nq, xlmBaseBoundTolerance*100, rateRat.FloatString(6))
		}
		violations += nq
	}
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

// printUSDVolumeTierTable renders the per-tier distribution in a fixed order so runs diff cleanly.
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

// printUSDVolumeFooter states in the report what the run did and did not prove, so a clean exit
// on a money surface is not mistaken for "usd_volume is verified correct".
func printUSDVolumeFooter(violations int) {
	fmt.Print(usdVolumeFooterText(violations))
}

// xlmBaseBoundFiresOver and xlmBaseBoundFiresUnder derive the bound's firing thresholds from
// xlmBaseBoundTolerance so the footer text cannot drift from the check.
func xlmBaseBoundFiresOver() *big.Rat {
	return new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).SetFloat64(xlmBaseBoundTolerance))
}

func xlmBaseBoundFiresUnder() *big.Rat {
	return new(big.Rat).Inv(new(big.Rat).Sub(big.NewRat(1, 1), new(big.Rat).SetFloat64(xlmBaseBoundTolerance)))
}

// usdVolumeFooterText is [printUSDVolumeFooter]'s body, returned rather
// than printed so the claims it makes are assertable in a test.
func usdVolumeFooterText(violations int) string {
	return fmt.Sprintf(`
--- what this run proved ---
CHECKED  quote_pegged + base_pegged: usd_volume == pegged_leg / 10^decimals,
         an exact identity with no tolerance.
CHECKED  XLM-base AND XLM-quote estimated groups (-tier xlm-base,
         -tier xlm-quote — whichever leg is an XLM form): Σusd_volume
         within ±%s%% of Σ(that leg)/1e7 × the day's CEX XLM/USD VWAP. A
         breach where stored and expected both round to $0.00 is printed, not counted.
         Read the bound honestly (#372 F1):
           - it FIRES at %sx overstatement / %sx understatement.
             10x-1,000,000x was the SIZE of the 2026-08-04 tier-3b
             poisoning, not this bound's threshold. A ratio just past the
             edge is a violation, not "within tolerance but interesting".
           - it is NOT structurally immune to intraday movement. Trades
             were anchored per-minute; this divides by a DAY VWAP off a
             different series, so an honest day can score up to
             max(intraday_hi/day_vwap, day_vwap/intraday_lo). Measured
             on r1 over 120 days (2026-01-01..2026-09-02): worst 1.2206,
             mean 1.0370, none reaching 1.30. It holds by 0.08 of
             measured headroom, not by construction.
         Total: %d violation(s) across both checked classes.
MEASURED remaining estimated rows (-tier cex-fx pairs where NEITHER leg
         is XLM, e.g. BTC/EUR, ETH/GBP): sums and row counts printed
         only — no XLM anchor to bound against, no calibrated threshold
         yet. This command's "acceptance:" line is NOT a pass/fail
         verdict for these rows.

A clean run does NOT certify that tier-3/4 usd_volume values are correct,
and does not certify coverage (that is the standing
stellarindex_{cex,onchain}_usd_volume_coverage_low alerts).

OPERATOR FOLLOW-UP: run this on r1 across a representative span
(e.g. -days 30), read the tier-3/4 spread out of the per-tier table, and
only then calibrate an alert threshold for the estimated tiers. Landing a
guessed threshold on a money surface is the C6-118 mistake — an
uncalibrated bound either false-fires forever or never fires at all.
`,
		new(big.Rat).Mul(new(big.Rat).SetFloat64(xlmBaseBoundTolerance), big.NewRat(100, 1)).FloatString(0),
		xlmBaseBoundFiresOver().FloatString(2),
		xlmBaseBoundFiresUnder().FloatString(2),
		violations,
	)
}
