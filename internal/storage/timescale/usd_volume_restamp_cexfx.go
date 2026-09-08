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
// The two XLM tiers repair on-chain rows off the XLM leg. This one
// repairs the OFF-CHAIN population the same way, off a leg no market
// participant authors either: an exchange trade quoted in a non-USD fiat
// currency (binance BTC/EUR, kraken ETH/GBP, …), valued through the
// vendor FX feed.
//
//	usd_volume = quote_amount / 10^<source scale> x <fiat>/USD at ts
//
// Measured population on r1: ~12.6M rows, `fiat:EUR` and `fiat:GBP`
// today.
//
// # Why these rows are NULL, and why the rate is NOT prices_1m
//
// prices_1m is a continuous aggregate over `trades` and holds CRYPTO
// markets: there is no `fiat:EUR/fiat:USD` row in it and there never will
// be. So before the resolver learned to read `fx_quotes` (2026-07-22)
// every non-USD-quoted CEX pair fell through all four tiers of
// [tradeUSDVolume] and inserted with `usd_volume` NULL — ~$939M of
// unpriced volume on 2026-07-17 alone
// (docs/operations/usd-volume-coverage-plan.md). The insert path is
// correct at HEAD; what is left is the history behind it.
//
// The rate therefore comes from `fx_quotes` — the daily vendor snapshots
// the `massive` feed writes, with rate_usd = UNITS-OF-TICKER PER 1 USD
// (migration 0028) — read through the SAME resolver branch the insert
// path uses ([VWAPUSDFXResolver.usdPriceForFiat] →
// [Store.fxQuotesSnapAtOrBefore]), which inverts it in exact *big.Rat
// space. Nothing here re-spells that arithmetic.
//
// # The as-of rule
//
// fx_quotes buckets are DAILY (the forex worker truncates every write to
// 24h) and weekday-only for most tickers, so a trade's timestamp almost
// never lands on a bucket. The rule, applied per row:
//
//   - take the most recent bucket AT OR BEFORE the trade's `ts`. Never a
//     later one: a rate published after the trade is information the
//     trade did not have, and using it would make a backfilled value
//     depend on when the operator ran the tool. Never an interpolation
//     between two buckets either — that would be a rate the vendor never
//     published, invented by this tool on a money column.
//   - REFUSE the row when the nearest such bucket is more than
//     [CEXFiatMaxQuoteStaleness] old, and count the refusal
//     ([XLMBaseRestampStats.FXDeclinedStale]). A stored value is left
//     exactly as it is; a stored NULL stays NULL.
//
// The tolerance is 7 days because that is [fxQuotesSnapLookback] — the
// bound the LIVE insert path already applies through the same resolver.
// Any wider and this tool would write values `InsertTrade` would decline
// to write today, which is the one property the whole re-derive rests on;
// narrower is the operator's call (`-fx-max-staleness`) when they want a
// run to touch only rows with a near-contemporaneous quote. 7 days is
// also the longest routine weekend/holiday gap in the feed, so at the
// default the rule refuses staleness rather than ordinary calendar gaps.

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
			return tradeUSDVolumeViaFiatQuoteFor(ctx, t, s.usdVolumeFXResolver), nil
		},
	})
	if err != nil {
		return nil, err
	}
	plan.Stats.FXDeclinedStale = gate.refused
	return plan, nil
}
