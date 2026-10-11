// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── the CEX FIAT-QUOTE usd_volume RE-DERIVE ──────────────────────────
//
// Re-derives off-chain trades quoted in a non-USD fiat (binance BTC/EUR,
// kraken ETH/GBP, …) through the vendor FX feed, a leg no market
// participant authors:
//
//	usd_volume = quote_amount / 10^<source scale> x <fiat>/USD at ts
//
// prices_1m holds only crypto markets, so such rows were NULL. The rate comes from `fx_quotes` (rate_usd = units
// per 1 USD, migration 0028) through the insert path's own resolver
// ([VWAPUSDFXResolver.usdPriceForFiat] → [Store.fxQuotesSnapAtOrBefore]),
// inverted in exact *big.Rat space.
//
// As-of rule, per row: take the newest daily bucket AT OR BEFORE `ts`,
// never a later one and never an interpolation, which would be a rate the
// vendor never published. The guarantee is per UTC day: the worker rewrites
// today's bucket and the trailing 7 days, so runs inside that window can
// differ by intraday FX. REFUSE the row when that bucket is older than
// [CEXFiatMaxQuoteStaleness] ([XLMBaseRestampStats.FXDeclinedStale]); stored
// values and NULLs are left as they are.
//
// The default tolerance is [fxQuotesSnapLookback] (7 days), the live insert
// path's bound, so this never writes what `InsertTrade` would decline;
// `-fx-max-staleness` may narrow it.

// CEXFiatMaxQuoteStaleness is the default (and maximum) as-of tolerance
// for the cex-fx tier: how far back the nearest `fx_quotes` bucket at or
// before a trade may sit before the row is refused rather than valued.
// It is [fxQuotesSnapLookback] — the live insert path's own bound — so a
// restamped row carries the value InsertTrade would write for it today.
const CEXFiatMaxQuoteStaleness = fxQuotesSnapLookback

// cexFiatQuoteAssets is the scan's quote-leg allow-list: every ADR-0010
// fiat code except USD, in the `fiat:<ISO4217>` wire form, sorted.
//
// Derived from [canonical.KnownFiatCodes] rather than from the two codes
// production carries today: a pair quoted in a third currency is then
// picked up the day it lands rather than the day someone remembers to
// widen a literal. USD is excluded because a USD quote is tier 1 —
// exact, and already the exact tier's business; scanning it would drag
// millions of correctly-valued rows through the walk for nothing.
func cexFiatQuoteAssets() []string {
	codes := canonical.KnownFiatCodes()
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		if code == usdFiatCode {
			continue
		}
		out = append(out, canonical.Asset{Type: canonical.AssetFiat, Code: code}.String())
	}
	return out
}

// cexFiatRestampSources resolves the scan's source list: the registered
// CEX venues filtered by the caller's allow-list.
func cexFiatRestampSources(allow map[string]bool) []string {
	return restampScanSources(CEXSourceNames(), allow)
}

// fxAsOfGate applies the as-of rule above to one window's rows.
//
// The read is cached per (ticker, UTC day): fx_quotes holds at most one
// bucket per ticker per day, so the nearest bucket at or before any
// instant in day D is the same row for every trade in D — but the AGE is
// measured from each row's OWN timestamp, so two trades in the same day
// can fall on opposite sides of the tolerance and the later one is
// refused on its own merits. Without the cache this would be one query
// per row against a 12.6M-row population.
type fxAsOfGate struct {
	store *Store
	max   time.Duration
	seen  map[fxAsOfKey]fxAsOfEntry
	// refused counts rows declined because the nearest bucket is outside
	// the tolerance (or there is none at all within it).
	refused int64
}

type fxAsOfKey struct {
	ticker string
	day    int64
}

type fxAsOfEntry struct {
	bucket time.Time
	ok     bool
}

func newFXAsOfGate(s *Store, tolerance time.Duration) *fxAsOfGate {
	if tolerance <= 0 {
		tolerance = CEXFiatMaxQuoteStaleness
	}
	return &fxAsOfGate{store: s, max: tolerance, seen: map[fxAsOfKey]fxAsOfEntry{}}
}

// priceable reports whether `fx_quotes` holds a quote for the fiat asset
// at or before `at` within the tolerance. A false answer is a REFUSAL to
// price the row, counted; an error is a failed read, which stops the run
// rather than being reported as an unpriceable population.
func (g *fxAsOfGate) priceable(ctx context.Context, asset canonical.Asset, at time.Time) (bool, error) {
	if asset.Type != canonical.AssetFiat {
		return false, nil
	}
	if asset.Code == usdFiatCode {
		// The rate_usd anchor: exactly 1, and no row to be stale.
		return true, nil
	}
	key := fxAsOfKey{ticker: asset.Code, day: at.UTC().Truncate(24 * time.Hour).Unix()}
	entry, cached := g.seen[key]
	if !cached {
		bucket, ok, err := g.store.FXQuoteBucketAtOrBefore(ctx, asset.Code, at, g.max)
		if err != nil {
			return false, err
		}
		entry = fxAsOfEntry{bucket: bucket, ok: ok}
		g.seen[key] = entry
	}
	if !entry.ok || at.UTC().Sub(entry.bucket) > g.max {
		g.refused++
		return false, nil
	}
	return true, nil
}

// PlanCEXFiatUSDVolumeRestamp scans one bounded window and returns the
// rows the fiat-quote re-derive would rewrite, plus the disposition of
// every row it would not. READ-ONLY; the plan is the whole of the dry run
// and [Store.ApplyUSDVolumeRestampPlan] consumes it verbatim.
//
// maxStaleness is the as-of tolerance documented above; <= 0 means
// [CEXFiatMaxQuoteStaleness]. A value WIDER than that is refused rather
// than honoured: the resolver would decline the quote anyway, so the run
// would report a tolerance it did not actually apply.
func (s *Store) PlanCEXFiatUSDVolumeRestamp(ctx context.Context, p RestampScanParams, maxStaleness time.Duration) (*RestampPlan, error) {
	if maxStaleness > CEXFiatMaxQuoteStaleness {
		return nil, fmt.Errorf("timescale: cex-fx restamp: as-of tolerance %s exceeds the live insert path's own fx_quotes lookback (%s); "+
			"a quote that stale is declined by the resolver, so the run would report a tolerance it never applied",
			maxStaleness, CEXFiatMaxQuoteStaleness)
	}
	gate := newFXAsOfGate(s, maxStaleness)
	plan, err := s.planRestampTier(ctx, p, restampTierScan{
		Tier:    "cex-fx",
		Sources: cexFiatRestampSources(p.Sources),
		Leg:     restampLegQuote,
		Assets:  cexFiatQuoteAssets(),
		Gate:    cexFiatTierFor,
		Value: func(t canonical.Trade) (*string, error) {
			ok, err := gate.priceable(ctx, t.Pair.Quote, t.Timestamp)
			if err != nil || !ok {
				return nil, err
			}
			return tradeUSDVolumeViaFiatQuoteFor(ctx, t, s.usdVolumeFXResolver)
		},
	})
	if err != nil {
		return nil, err
	}
	plan.Stats.FXDeclinedStale = gate.refused
	return plan, nil
}
