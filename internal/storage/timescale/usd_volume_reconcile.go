package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// The usd_volume VALUE reconcile. The usd-volume alerts check COVERAGE only; a trade priced with
// the WRONG number is 100% covered.
//
// This file is the read side: a day-scoped SQL aggregation and the exact rational arithmetic that
// judges it. The tier classifier lives in trades.go beside the waterfall it mirrors
// ([ClassifyUSDVolumeTier]); they must move together, and this file must not import
// internal/sources/external (storage-below-compute layering).
//
// Upper tiers are VWAP/FX estimates with no calibrated tolerance, so they are measured, not
// judged. Tiers 1/2 (QUOTE leg is USD or a declared peg) and 2b (BASE leg is) are pure decimal
// rescalings of an amount on the row:
//
// 	usd_volume = pegged_leg_amount / 10^decimals
//
// An exact identity with no price lookup or tolerance; if it fails, `usd_volume` used the wrong
// scale, leg, peg list or backfill vintage.

// USDVolumeTier names how a trade's `usd_volume` was derived, mirroring the
// waterfall in [tradeUSDVolume]. Stable strings: they appear in the
// operator report.
type USDVolumeTier string

const (
	// TierQuotePegged is tiers 1/2: the QUOTE asset is fiat:USD or a
	// declared USD peg, so usd_volume = quote_amount / 10^decimals. EXACT.
	TierQuotePegged USDVolumeTier = "quote_pegged"

	// TierBasePegged is tier 2b: the quote leg declined but the BASE asset
	// is USD-pegged, so usd_volume = base_amount / 10^decimals. EXACT, and
	// the same "trust the declared peg" assumption as the quote side.
	TierBasePegged USDVolumeTier = "base_pegged"

	// TierEstimated is tiers 3/4: neither leg is pegged, so the value came
	// from an FX rate or the XLM anchor at trade time. NOT reproducible
	// from the row alone, and legitimately inexact.
	TierEstimated USDVolumeTier = "estimated"

	// TierUnvaluable is a source whose external class is not
	// ClassExchange — [tradeUSDVolume] returns nil for these by
	// construction, so any non-NULL usd_volume on such a row was written
	// by something other than the current insert path.
	TierUnvaluable USDVolumeTier = "unvaluable"
)

// Exact reports whether the tier's identity is a pure rescaling of a stored
// leg — i.e. whether a mismatch is a defect rather than an estimate's error.
func (t USDVolumeTier) Exact() bool {
	return t == TierQuotePegged || t == TierBasePegged
}

// TradeValuationGroup is one (source, base, quote) group's raw sums over a single UTC day.
// Amounts are decimal strings straight off NUMERIC, never float64, so comparison stays exact.
//
// The sums cover PRICED rows only (`usd_volume IS NOT NULL`): SUM(usd_volume) skips unpriced rows
// while SUM(quote_amount) would not, so mixing them in would fail the identity by construction.
// UnpricedRows carries that population separately.
type TradeValuationGroup struct {
	Source     string
	BaseAsset  string
	QuoteAsset string

	PricedRows   int64
	UnpricedRows int64

	SumUSDVolume   string
	SumBaseAmount  string
	SumQuoteAmount string
}

// TradeValuationByDay returns the per-(source, base, quote) valuation sums
// for the UTC day containing `day`.
//
// Grouped by pair because the TIER is a property of the pair and cannot be decided in SQL: the
// peg set lives in Go (`usd_pegged_classic_assets`, `supply.sac_wrappers`, aggregate.FiatProxy).
// The caller classifies each group with [ClassifyUSDVolumeTier], the SAME function the insert
// path uses, so the check re-derives what the writer should have done instead of a parallel
// reimplementation that could disagree for the wrong reason.
//
// Cardinality is the day's traded pair universe (a few thousand rows at most), and the scan is
// one day of the `trades` time dimension, i.e. a single chunk.
func (s *Store) TradeValuationByDay(ctx context.Context, day time.Time) ([]TradeValuationGroup, error) {
	start := day.UTC().Truncate(24 * time.Hour)
	const q = `
		SELECT source, base_asset, quote_asset,
		       count(*) FILTER (WHERE usd_volume IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE usd_volume IS NULL)::bigint,
		       COALESCE(sum(usd_volume)   FILTER (WHERE usd_volume IS NOT NULL), 0)::text,
		       COALESCE(sum(base_amount)  FILTER (WHERE usd_volume IS NOT NULL), 0)::text,
		       COALESCE(sum(quote_amount) FILTER (WHERE usd_volume IS NOT NULL), 0)::text
		  FROM trades
		 WHERE ts >= $1 AND ts < $1::timestamptz + interval '1 day'
		 GROUP BY source, base_asset, quote_asset
		 ORDER BY source, base_asset, quote_asset
	`
	rows, err := s.db.QueryContext(ctx, q, start)
	if err != nil {
		return nil, fmt.Errorf("timescale: TradeValuationByDay %s: %w", start.Format(time.DateOnly), err)
	}
	defer func() { _ = rows.Close() }()

	var out []TradeValuationGroup
	for rows.Next() {
		var g TradeValuationGroup
		if err := rows.Scan(&g.Source, &g.BaseAsset, &g.QuoteAsset,
			&g.PricedRows, &g.UnpricedRows,
			&g.SumUSDVolume, &g.SumBaseAmount, &g.SumQuoteAmount); err != nil {
			return nil, fmt.Errorf("timescale: TradeValuationByDay scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradeValuationByDay rows: %w", err)
	}
	return out, nil
}

// PeggedLegSum picks the leg whose rescaling produced `usd_volume` for an
// exact tier. Empty string for a non-exact tier.
func (g TradeValuationGroup) PeggedLegSum(tier USDVolumeTier) string {
	switch tier {
	case TierQuotePegged:
		return g.SumQuoteAmount
	case TierBasePegged:
		return g.SumBaseAmount
	case TierEstimated, TierUnvaluable:
		return ""
	default:
		return ""
	}
}

// USDVolumeRoundingSlack is the largest |Σusd_volume − Σleg/10^d| that per-row rendering can
// legitimately produce for an exact tier, in USD.
//
// [tradeUSDVolume] renders each row as `big.Rat(leg, 10^d).FloatString(8)`. For d ≤ 8 that is
// lossless, so the slack is exactly ZERO and the identity needs no tolerance. Only a peg declared
// by [USDVolumeQuoteSpec] with more than 8 decimals introduces rounding, bounded by half an ulp
// per row. This is the derived worst case of a known rounding rule, not a calibrated threshold.
func USDVolumeRoundingSlack(decimals int, rows int64) *big.Rat {
	if decimals <= usdVolumeRenderDecimals || rows <= 0 {
		return new(big.Rat)
	}
	// rows × 0.5 × 10^-8
	halfUlp := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Mul(
		big.NewInt(2), scaleDenominator(usdVolumeRenderDecimals)))
	return new(big.Rat).Mul(halfUlp, new(big.Rat).SetInt64(rows))
}

// usdVolumeRenderDecimals is the fixed-precision scale tradeUSDVolume
// renders every computed usd_volume at (`FloatString(8)`). Kept next to the
// slack it determines so the two cannot drift apart.
const usdVolumeRenderDecimals = 8

// ExactTierDelta returns Σusd_volume − Σpegged_leg/10^decimals for an exact tier: the signed
// USD error of the stored column against the identity the insert path claims. ok=false when the
// sums do not parse as decimals (a corrupt NUMERIC render, itself reportable).
//
// Exact rational arithmetic, never float64: a drift below a float64 ulp on a nine-figure daily
// volume is exactly what this check exists to see.
func ExactTierDelta(g TradeValuationGroup, tier USDVolumeTier, decimals int) (delta *big.Rat, ok bool) {
	leg := g.PeggedLegSum(tier)
	if leg == "" {
		return nil, false
	}
	stored, ok := new(big.Rat).SetString(g.SumUSDVolume)
	if !ok {
		return nil, false
	}
	legSum, ok := new(big.Rat).SetString(leg)
	if !ok {
		return nil, false
	}
	expected := new(big.Rat).Quo(legSum, new(big.Rat).SetInt(scaleDenominator(decimals)))
	return new(big.Rat).Sub(stored, expected), true
}

// DayCloseVWAPXLMUSD returns the daily VWAP for crypto:XLM/fiat:USD (the CEX-fed series, whose
// inputs are not authorable on-chain) from the prices_1d CAGG for the given UTC day. ok=false
// when the day has no bucket, and callers then skip the XLM-base bound rather than judge
// against a missing rate.
//
// It feeds verify-usd-volume's XLM-BASE BOUND: tier-4 rows anchor usd_volume to
// base_amount/1e7 × XLM/USD at trade time, so a day-group's Σusd_volume must land within an
// intraday-range tolerance of Σbase/1e7 × day-VWAP.
//
// A day-granular rate CAN false-alarm: the anchor reads a per-MINUTE prices_1m bucket while this
// reads a DAY bucket of a different (CEX) series, so an honest day scores up to
// max(intraday_hi/day_vwap, day_vwap/intraday_lo). See [chops.xlmBaseBoundTolerance].
func (s *Store) DayCloseVWAPXLMUSD(ctx context.Context, day time.Time) (string, bool, error) {
	start := day.UTC().Truncate(24 * time.Hour)
	const q = `
		SELECT vwap::text
		  FROM prices_1d
		 WHERE base_asset = 'crypto:XLM' AND quote_asset = 'fiat:USD'
		   AND bucket = $1
	`
	var vwap string
	err := s.db.QueryRowContext(ctx, q, start).Scan(&vwap)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("timescale: DayCloseVWAPXLMUSD %s: %w", start.Format(time.DateOnly), err)
	}
	return vwap, true, nil
}
