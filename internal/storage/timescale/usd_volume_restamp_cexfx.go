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
// Re-derives off-chain trades quoted in a non-USD fiat through the vendor FX feed:
//
//	usd_volume = quote_amount / 10^<source scale> x <fiat>/USD at ts
//
// The rate comes from `fx_quotes` (rate_usd = units per 1 USD) through the insert path's own
// resolver, inverted in exact *big.Rat space.
//
// As-of rule, per row: take the newest daily bucket AT OR BEFORE `ts`, never a later one and never
// an interpolation (a rate the vendor never published). The worker rewrites today's bucket and the
// trailing 7 days, so runs inside that window can differ by intraday FX. REFUSE the row when the
// bucket is older than [CEXFiatMaxQuoteStaleness] ([XLMBaseRestampStats.FXDeclinedStale]).
//
// The default tolerance is the live insert path's bound, so this never writes what `InsertTrade`
// would decline; `-fx-max-staleness` may only narrow it.

// CEXFiatMaxQuoteStaleness is the default and maximum as-of tolerance for the cex-fx tier.
const CEXFiatMaxQuoteStaleness = fxQuotesSnapLookback

// cexFiatQuoteAssets is the quote-leg allow-list: every [canonical.KnownFiatCodes] code except USD (already exact, tier 1), as sorted
// `fiat:<ISO4217>`.
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

// fxAsOfGate applies the as-of rule above per window, caching per (ticker, UTC day) since fx_quotes has one bucket per ticker-day. Age
// is still measured from each row's OWN timestamp, so two trades in one day can fall either side of the tolerance.
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

// priceable reports whether fx_quotes holds a quote at or before `at` within tolerance; false is a counted REFUSAL, an error is a failed
// read that stops the run.
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

// PlanCEXFiatUSDVolumeRestamp scans one bounded window and returns the rows the re-derive would rewrite plus the disposition of every row it would not; READ-ONLY, and Store.ApplyUSDVolumeRestampPlan consumes the plan verbatim.
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
