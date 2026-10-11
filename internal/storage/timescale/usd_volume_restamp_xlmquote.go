// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The XLM-QUOTE usd_volume re-derive, the mirror of xlm-base (usd_volume_restamp_xlmbase.go)
// for pools that store an `XLM / <token>` market the other way round:
//
// 	usd_volume = quote_amount / 1e7 x XLM/USD at ts
//
// Which leg lands in `base_asset` is the pool's own token ordering. Rows inserted before the
// waterfall anchored the XLM quote leg went to [tradeUSDVolumeViaFX], whose two-leg
// cross-check can leave NULL or a value pulled down to the token leg's figure. The anchor does
// not depend on a rate the pair's counterparties can author.
//
// # The two XLM tiers are DISJOINT, by construction
//
// [xlmQuoteTierFor] refuses any row whose BASE leg is also an XLM form, so a `native / XLM-SAC`
// trade belongs to the xlm-base tier alone. Two tiers claiming one row would each stamp it at
// their own generation and the `derive_generation <= $gen` guard would let run order decide the
// value. It also refuses a USD-pegged base leg, which is the exact tier (2b).

// PlanXLMQuoteUSDVolumeRestamp scans one bounded window and returns the rows the XLM-quote
// re-derive would rewrite, plus the disposition of every row it would not. READ-ONLY; the plan
// is the whole dry run and [Store.ApplyUSDVolumeRestampPlan] consumes it verbatim.
//
// The value comes from [tradeUSDVolumeViaXLMQuoteAnchorFor], the base-side anchor the insert
// path calls on the mirrored row, so `leg/1e7 x XLM/USD` has one spelling and a change moves
// both tiers at once.
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
