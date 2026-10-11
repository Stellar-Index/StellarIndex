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

// ─── `-tier xlm-quote` and `-tier cex-fx` — the two mirror re-derives ───
//
// These repair the populations either side of the xlm-base tier through the
// same run, walk, chunk driver, generation guard, `-fill-null` opt-in and
// fail-closed dry run; each supplies only a planner.
//
//	-tier xlm-quote: on-chain DEX trades whose QUOTE leg is XLM (`native` or
//	  the SAC) and whose BASE leg is neither XLM nor a declared USD peg.
//	  usd_volume = quote_amount/1e7 x XLM/USD at ts, through the xlm-base
//	  tier's anchor ([timescale.Store.PlanXLMQuoteUSDVolumeRestamp]).
//
//	-tier cex-fx: off-chain CEX trades quoted in a non-USD fiat.
//	  usd_volume = quote_amount/10^<source scale> x <fiat>/USD at ts, read
//	  from `fx_quotes` because prices_1m holds no fiat pair. The as-of rule
//	  is on [timescale.Store.PlanCEXFiatUSDVolumeRestamp].
//
// Both keep the xlm-base money rules: an unpriceable row is REPORTED and left
// as it is (NULL stays NULL, a value is never blanked or replaced by a
// second-choice estimate at a high derive_generation, which a later
// correction cannot claw back), and token/token pairs with no trustworthy leg
// stay outside both scans (the substance gate, usd_volume_restamp_legs.go).

// xlmQuoteRestampStore is the slice of [timescale.Store] the XLM-quote
// mirror walks through. A seam rather than the concrete store so the walk
// can be driven against a scripted double; production passes the store.
type xlmQuoteRestampStore interface {
	PlanXLMQuoteUSDVolumeRestamp(ctx context.Context, p timescale.RestampScanParams) (*timescale.RestampPlan, error)
	ApplyUSDVolumeRestampPlan(ctx context.Context, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)
}

// cexFiatRestampStore is the same for the fiat-quote re-derive. Its
// planner takes the as-of tolerance, the one input the shared walk knows
// nothing about.
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

// cexFiatTierProfile is the fiat-quote re-derive's profile. The as-of
// tolerance rides in the header and in the RESUME line as well as in the
// planner: a run at a narrowed tolerance and a run at the default leave
// different rows unpriced, and an operator reading a report has to be
// able to tell which one produced it.
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
