// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// `-tier xlm-quote` (DEX trades quoted in XLM, priced via the xlm-base anchor) and `-tier cex-fx`
// (CEX trades quoted in a non-USD fiat, priced from `fx_quotes`) share the xlm-base run, walk and
// guards and supply only a planner (see [timescale.Store.PlanXLMQuoteUSDVolumeRestamp] and
// [timescale.Store.PlanCEXFiatUSDVolumeRestamp]). They keep its money rules: an unpriceable row is
// reported and left as is, never blanked or replaced by a second-choice estimate, and
// token/token pairs with no trustworthy leg stay out.

// xlmQuoteRestampStore is the slice of [timescale.Store] the XLM-quote mirror walks through, a seam so the walk can run against a scripted
// double.
type xlmQuoteRestampStore interface {
	PlanXLMQuoteUSDVolumeRestamp(ctx context.Context, p timescale.RestampScanParams) (*timescale.RestampPlan, error)
	ApplyUSDVolumeRestampPlan(ctx context.Context, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)
}

// cexFiatRestampStore is the same for the fiat-quote re-derive; its planner takes the as-of tolerance, which the shared walk knows nothing
// about.
type cexFiatRestampStore interface {
	PlanCEXFiatUSDVolumeRestamp(ctx context.Context, p timescale.RestampScanParams, maxStaleness time.Duration) (*timescale.RestampPlan, error)
	ApplyUSDVolumeRestampPlan(ctx context.Context, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)
}

// xlmQuoteTierProfile is the XLM-quote mirror's [estimatedTierProfile].
func xlmQuoteTierProfile(store xlmQuoteRestampStore) estimatedTierProfile {
	return estimatedTierProfile{
		Tier:         restampTierXLMQuote,
		Scope:        "DEX sources in scope: " + strings.Join(timescale.DEXSourceNames(), ", "),
		Plan:         store.PlanXLMQuoteUSDVolumeRestamp,
		Apply:        store.ApplyUSDVolumeRestampPlan,
		ScanLabel:    "source=DEX, quote=XLM form",
		DeclineLabel: "anchor declined",
	}
}

// cexFiatTierProfile is the fiat-quote re-derive's profile. The as-of tolerance also appears in
// the header and RESUME line because different tolerances leave different rows unpriced.
func cexFiatTierProfile(store cexFiatRestampStore, maxStaleness time.Duration) estimatedTierProfile {
	p := estimatedTierProfile{
		Tier: restampTierCEXFX,
		Scope: "CEX sources in scope: " + strings.Join(timescale.CEXSourceNames(), ", ") +
			"; rate source: fx_quotes at or before each trade, within " + maxStaleness.String() +
			" (prices_1m holds no fiat pair)",
		Plan: func(ctx context.Context, sp timescale.RestampScanParams) (*timescale.RestampPlan, error) {
			return store.PlanCEXFiatUSDVolumeRestamp(ctx, sp, maxStaleness)
		},
		Apply:        store.ApplyUSDVolumeRestampPlan,
		ScanLabel:    "source=CEX, quote=non-USD fiat",
		DeclineLabel: "fx declined",
		HeaderFlags:  fmt.Sprintf(" fx_max_staleness=%s", maxStaleness),
	}
	if maxStaleness != timescale.CEXFiatMaxQuoteStaleness {
		p.ResumeFlags = fmt.Sprintf(" -fx-max-staleness %s", maxStaleness)
	}
	return p
}

// runXLMQuoteRestamp is the `-tier xlm-quote` in-place entry point.
func runXLMQuoteRestamp(ctx context.Context, store xlmQuoteRestampStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions) error {
	return runEstimatedRestamp(ctx, newEstimatedRestampRun(opts, xlmQuoteTierProfile(store)), cfgPath, from, to, opts)
}

// runCEXFiatRestamp is the `-tier cex-fx` in-place entry point.
func runCEXFiatRestamp(ctx context.Context, store cexFiatRestampStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, maxStaleness time.Duration) error {
	return runEstimatedRestamp(ctx, newEstimatedRestampRun(opts, cexFiatTierProfile(store, maxStaleness)), cfgPath, from, to, opts)
}
