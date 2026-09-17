package timescale

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// MonthlyUSDVWAP is one canonical asset's volume-weighted USD price for
// one calendar month (UTC), from this index's own USD-quoted markets —
// "what it was worth then", as opposed to the live rate every other
// cohort figure is valued at.
type MonthlyUSDVWAP struct {
	// Asset is the canonical id every alias form of the month's markets
	// folded onto: 'native' for XLM in all three spellings, the classic
	// CODE-ISSUER for a SAC-wrapped classic asset the registry knows.
	Asset string
	// Month is the first instant of the calendar month, UTC.
	Month time.Time
	// VWAPUSD is Σ quote / Σ base over every folded row, rendered like a
	// combined VWAP (25 significant digits, trailing zeros trimmed). The
	// rendering is the only lossy step; the fold is exact.
	VWAPUSD string
	// VolumeUSD is the folded rows' USD volume — a magnitude for ranking
	// and for judging how much market stood behind the price.
	VolumeUSD float64
}

// monthlyUSDVWAPsSQL reads the monthly CAGG's USD-quoted rows in
// [from, to). The quote set is the transitive resolver's usdProxyQuotes
// — the quotes whose VWAP is ALREADY a USD price — so a month priced here
// agrees with the live path about what counts as a dollar. $1/$2 are
// cast explicitly: an untyped bind beside a timestamptz column is the
// 42883 trap.
const monthlyUSDVWAPsSQL = `
SELECT bucket, base_asset, vwap::text, volume::text, COALESCE(volume_usd, 0)::text
  FROM prices_1mo
 WHERE quote_asset IN (` + usdProxyQuotes + `)
   AND bucket >= $1::timestamptz
   AND bucket <  $2::timestamptz
   AND vwap IS NOT NULL
   AND volume > 0
 ORDER BY bucket, base_asset`

// MonthlyUSDVWAPs folds prices_1mo's USD-quoted rows into ONE row per
// (canonical asset, month), volume-weighted across every alias spelling
// of the base and every USD proxy it was quoted in: a month where XLM
// traded as 'native' against USDC and as 'crypto:XLM' against fiat:USD
// yields a single 'native' row whose price is Σ quote / Σ base of both.
// The arithmetic is exact (big.Rat); only the final rendering rounds.
//
// XLM's own row rides along under 'native', so an asset quoted only in
// XLM can be priced through it by a follow-up. Rows are ascending by
// (month, asset). An empty result is a measurement — no USD-quoted
// market in the range — not an error.
func (s *Store) MonthlyUSDVWAPs(ctx context.Context, from, to time.Time) ([]MonthlyUSDVWAP, error) {
	rows, err := s.db.QueryContext(ctx, monthlyUSDVWAPsSQL, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type key struct {
		asset string
		month int64 // unix seconds of the month start
	}
	type fold struct {
		month            time.Time
		quote, base, usd *big.Rat
	}
	folds := map[key]*fold{}
	for rows.Next() {
		var (
			bucket                     time.Time
			base, vwapT, volT, usdVolT string
		)
		if err := rows.Scan(&bucket, &base, &vwapT, &volT, &usdVolT); err != nil {
			return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs scan: %w", err)
		}
		vwap, ok1 := new(big.Rat).SetString(vwapT)
		vol, ok2 := new(big.Rat).SetString(volT)
		usdVol, ok3 := new(big.Rat).SetString(usdVolT)
		if !ok1 || !ok2 || !ok3 {
			// A NUMERIC that does not parse is corruption, not a miss.
			return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs: unparseable numeric for %s @ %s (vwap=%q volume=%q volume_usd=%q)",
				base, bucket.UTC().Format(time.RFC3339), vwapT, volT, usdVolT)
		}
		month := bucket.UTC()
		k := key{asset: monthlyUSDVWAPAsset(base), month: month.Unix()}
		f := folds[k]
		if f == nil {
			f = &fold{month: month, quote: new(big.Rat), base: new(big.Rat), usd: new(big.Rat)}
			folds[k] = f
		}
		// vwap × volume re-derives the row's quote leg, so the fold is the
		// union market's own Σ quote / Σ base — not an average of prices.
		f.quote.Add(f.quote, new(big.Rat).Mul(vwap, vol))
		f.base.Add(f.base, vol)
		f.usd.Add(f.usd, usdVol)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs rows: %w", err)
	}

	out := make([]MonthlyUSDVWAP, 0, len(folds))
	for k, f := range folds {
		if f.base.Sign() <= 0 {
			continue
		}
		usd, _ := f.usd.Float64()
		out = append(out, MonthlyUSDVWAP{
			Asset:     k.asset,
			Month:     f.month,
			VWAPUSD:   formatCombinedVWAP(new(big.Rat).Quo(f.quote, f.base)),
			VolumeUSD: usd,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Month.Equal(out[j].Month) {
			return out[i].Month.Before(out[j].Month)
		}
		return out[i].Asset < out[j].Asset
	})
	return out, nil
}

// monthlyUSDVWAPAsset is the fold key for one stored base_asset spelling:
// its canonical form under the process alias registry (native for every
// XLM spelling; the classic form for a registered SAC), or the spelling
// itself when it is not a canonical asset id at all — such a row folds
// with nothing, which is the truth about it.
func monthlyUSDVWAPAsset(raw string) string {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return raw
	}
	return canonical.CanonicalAsset(a).String()
}
