// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── the XLM-QUOTE usd_volume RE-DERIVE — the mirror of xlm-base ───────
//
// usd_volume_restamp_xlmbase.go re-derives the on-chain DEX trades whose
// XLM leg is the BASE one. This file is the same tier for the rows where
// the pool stored the trade the other way round:
//
//	usd_volume = quote_amount / 1e7 x XLM/USD at ts
//
// Measured population on r1: ~18.0M rows.
//
// # Why the orientation alone makes a different tier
//
// Which leg of an `XLM / <token>` market ends up in `base_asset` is the
// pool's own token ordering, not a property of the trade — the same
// economic swap lands either way round depending on the venue. The
// waterfall reads the two sides differently, though:
// [tradeUSDVolumeViaXLMBaseAnchor] fires only when the BASE leg is
// anchorable, so an XLM-QUOTED trade goes to [tradeUSDVolumeViaFX]
// instead and is valued off whatever rate the resolver can find for the
// quote leg — which for XLM is a direct XLM/USD market, i.e. the same
// number, reached by a different route.
//
// The rows this tier repairs are the ones where that route produced
// something else, or nothing: a NULL usd_volume (no rate for the minute
// at insert time), or a value from before the resolver could bridge, or
// a value the two-leg cross-check pulled DOWN to the token leg's own
// figure. The anchor's answer is the one that does not depend on a rate
// the pair's own counterparties can author.
//
// # The two XLM tiers are DISJOINT, by construction
//
// [xlmQuoteTierFor] refuses any row whose BASE leg is also an XLM form,
// so a `native / XLM-SAC` trade belongs to the xlm-base tier alone. That
// matters beyond tidiness: two tiers claiming one row would each stamp it
// at their own generation, and the INV-3 guard would then make the run
// order decide the value. It also refuses a USD-pegged base leg, which is
// tier 2b — `usd-volume-restamp -tier exact`'s, and EXACT.

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
			return tradeUSDVolumeViaXLMQuoteAnchorFor(ctx, t, s.usdVolumeFXResolver), nil
		},
	})
}
