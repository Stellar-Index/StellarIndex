// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The XLM-QUOTE usd_volume re-derive, the mirror of xlm-base
// (usd_volume_restamp_xlmbase.go) for rows the pool stored the other way round:
//
// 	usd_volume = quote_amount / 1e7 x XLM/USD at ts
//
// Which leg of an `XLM / <token>` market lands in `base_asset` is the pool's own
// token ordering. Rows inserted before the waterfall anchored the XLM quote leg
// went to [tradeUSDVolumeViaFX], whose two-leg cross-check can leave NULL, a value
// from before the resolver could bridge, or a value pulled DOWN to the token leg's
// figure. The anchor's answer does not depend on a rate the pair's counterparties
// can author.
//
// # The two XLM tiers are DISJOINT, by construction
//
// [xlmQuoteTierFor] refuses any row whose BASE leg is also an XLM form, so a
// `native / XLM-SAC` trade belongs to the xlm-base tier alone. Two tiers claiming
// one row would each stamp it at their own generation, and the
// `derive_generation <= $gen` guard would make run order decide the value. It also
// refuses a USD-pegged base leg, which is tier 2b (EXACT,
// `usd-volume-restamp -tier exact`).

// PlanXLMQuoteUSDVolumeRestamp scans one bounded window and returns the
// rows the XLM-quote re-derive would rewrite, plus the disposition of
// every row it would not. READ-ONLY; the plan is the whole of the dry run
// and [Store.ApplyUSDVolumeRestampPlan] consumes it verbatim.
//
// The value comes from [tradeUSDVolumeViaXLMQuoteAnchorFor] — the
// base-side anchor the insert path calls, handed the mirrored row, so
// there is exactly one spelling of `leg/1e7 x XLM/USD` in the codebase
// and a change to it moves both tiers at once.
func (s *Store) PlanXLMQuoteUSDVolumeRestamp(ctx context.Context, p RestampScanParams) (*RestampPlan, error) {
	return s.planRestampTier(ctx, p, restampTierScan{
		Tier:    "xlm-quote",
		Sources: xlmBaseRestampSources(p.Sources),
		Leg:     restampLegQuote,
		Assets:  xlmAssetForms(),
		Gate:    xlmQuoteTierFor,
		Value: func(t canonical.Trade) (*string, error) {
			return tradeUSDVolumeViaXLMQuoteAnchorFor(ctx, t, s.usdVolumeFXResolver)
		},
	})
}
