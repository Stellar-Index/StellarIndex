package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ─── C4-055 / C4-066: the usd_volume VALUE reconcile ─────────────
//
// The existing usd-volume alerts (configs/prometheus/rules.r1/
// usd-volume-coverage.yml) are a COVERAGE check: they read the ratio of
// trades inserted with a non-NULL `usd_volume`. That catches "we stopped
// pricing this venue" and nothing else. A trade priced with the WRONG
// number is 100% covered and completely wrong, and every volume surface we
// publish — DEX volume, asset volume, venue rankings, market share — is a
// sum of that column.
//
// This file is the value half's read side: the day-scoped SQL aggregation
// and the exact rational arithmetic that judges it. The TIER CLASSIFIER
// deliberately lives in trades.go instead, immediately beside the waterfall
// it mirrors — see [ClassifyUSDVolumeTier]. Two reasons, and both matter:
// the two must move together or the check silently verifies a waterfall the
// writer no longer runs, and this file must not import
// internal/sources/external (the storage-below-compute layering rule; that
// upward edge is grandfathered for trades.go, and the baseline only
// shrinks).
//
// It It deliberately does NOT try to
// re-price trades from an independent price series: `usd_volume` is a
// five-tier waterfall (see tradeUSDVolume) whose upper tiers are VWAP/FX
// ESTIMATES, and this repo has already measured the two routes diverging by
// up to 134.92% on the same trades, so any cross-series comparison needs a
// calibrated tolerance nobody has measured yet.
//
// What it does instead is exploit the fact that the BOTTOM tiers are not
// estimates at all. Tiers 1/2 (the QUOTE leg is USD or a declared USD peg)
// and tier 2b (the BASE leg is) are pure decimal rescalings of an amount
// already stored on the row:
//
//	usd_volume = pegged_leg_amount / 10^decimals
//
// That is an EXACT arithmetic identity with no price lookup, no time
// alignment and therefore no tolerance to argue about — and it covers the
// largest and most load-bearing slice of the volume surface. If it does not
// hold, `usd_volume` was computed with the wrong scale, the wrong leg, a
// stale peg list, or by a superseded backfill vintage, and every aggregate
// built on it is wrong by exactly that factor.
//
// The estimated tiers are still MEASURED here (their sums and row counts are
// returned) so an operator can read the real divergence distribution off a
// production run and calibrate a threshold for them afterwards. They are not
// judged, because judging them would require the number that run produces.

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

// TradeValuationGroup is one (source, base, quote) group's raw sums over a
// single UTC day. Amounts are decimal strings straight off NUMERIC — never
// float64 — so the caller's comparison stays exact (ADR-0003).
//
// The sums cover PRICED rows only (`usd_volume IS NOT NULL`). Mixing in
// unpriced rows would make even the exact identity fail by construction,
// since SUM(usd_volume) skips them while SUM(quote_amount) would not;
// UnpricedRows carries that population separately, and it is the coverage
// signal the existing alerts already own.
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
// Grouped by pair rather than aggregated because the TIER is a property of
// the pair, and it cannot be decided in SQL: the peg set lives in Go (the
// operator's `usd_pegged_classic_assets` + `supply.sac_wrappers`, plus
// aggregate.FiatProxy for off-chain tickers). The caller classifies each
// group with [ClassifyUSDVolumeTier] — the SAME functions the insert path
// uses, so the check is a re-derivation of what the writer should have done
// rather than a parallel reimplementation that could disagree for the wrong
// reason (the discipline internal/completeness applies to its decoder
// re-derive).
//
// Cardinality is bounded by the traded pair universe for one day — a few
// thousand rows at most — and the scan is one day of the `trades`
// hypertable's time dimension, i.e. a single chunk.
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

// USDVolumeRoundingSlack is the largest |Σusd_volume − Σleg/10^d| that
// per-row rendering can legitimately produce for an exact tier, in USD.
//
// [tradeUSDVolume] renders each row as `big.Rat(leg, 10^d).FloatString(8)`.
// When d ≤ 8 the quotient of an integer by 10^d has at most 8 decimal
// places, so the rendering is lossless and the slack is exactly ZERO — the
// identity is checkable with no tolerance at all. Only if a future
// [USDVolumeQuoteSpec] ever declares a >8-decimal peg does rounding enter,
// and then it is bounded by half an ulp per row.
//
// This is DERIVED arithmetic, not a calibrated threshold: it is the exact
// worst case of a known rounding rule, so it cannot mask a real defect
// beyond a fraction of a cent per row.
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

// ExactTierDelta returns Σusd_volume − Σpegged_leg/10^decimals for an exact
// tier: the signed error, in USD, of the stored column against the identity
// the insert path claims to satisfy. ok=false when the group's sums do not
// parse as decimals (a corrupt NUMERIC render, which is itself reportable).
//
// Exact rational arithmetic throughout — never float64. A relative error
// smaller than a float64 ulp on a nine-figure daily volume is precisely the
// class of drift this check exists to see.
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

// DayCloseVWAPXLMUSD returns the daily VWAP for crypto:XLM/fiat:USD
// (the CEX-fed series — the one XLM/USD series whose inputs are not
// authorable on-chain) from the prices_1d CAGG for the given UTC day.
// ok=false when the day has no bucket (a fresh deployment, or a day
// before CEX ingest existed) — callers skip the XLM-base sanity bound
// for that day rather than judging against a missing rate.
//
// This feeds verify-usd-volume's XLM-BASE BOUND: tier-4 rows anchor
// usd_volume to base_amount/1e7 × XLM/USD at trade time, so a
// day-group's Σusd_volume must land within an intraday-range tolerance
// of Σbase/1e7 × day-VWAP. The 2026-08-04 poisoning class was 10×–10⁶×
// off; a coarse bound catches it while a day-granular rate cannot
// false-alarm on normal intraday movement.
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
