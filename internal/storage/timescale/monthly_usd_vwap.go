package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceAmountDecimals reports the decimal scale a trade source stamps its
// amounts at (the source registry's AmountScaleDecimals). Storage takes it
// from the caller rather than importing the registry.
type SourceAmountDecimals func(source string) int

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
	// VWAPUSD is Σ quote / Σ base in whole units over every folded row
	// (see [monthlyUSDVWAPScale]), rendered like a
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
SELECT bucket, base_asset, vwap::text, volume::text, COALESCE(volume_usd, 0)::text, sources
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
// yields a single 'native' row whose price is Σ quote / Σ base of both,
// each row's volume taken in whole units so a spelling stored at 10^8
// does not outweigh one stored at 10^7. The arithmetic is exact
// (big.Rat); only the final rendering rounds.
//
// XLM's own row rides along under 'native', so an asset quoted only in
// XLM can be priced through it by a follow-up. Rows are ascending by
// (month, asset). An empty result is a measurement — no USD-quoted
// market in the range — not an error. sourceDecimals scales off-chain
// spellings; nil fails any off-chain row rather than guess its unit.
func (s *Store) MonthlyUSDVWAPs(ctx context.Context, from, to time.Time, sourceDecimals SourceAmountDecimals) ([]MonthlyUSDVWAP, error) {
	rows, err := s.db.QueryContext(ctx, monthlyUSDVWAPsSQL, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var in []monthlyUSDVWAPRow
	for rows.Next() {
		r, err := scanMonthlyUSDVWAPRow(rows)
		if err != nil {
			return nil, err
		}
		in = append(in, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs rows: %w", err)
	}
	return foldMonthlyUSDVWAPs(in, sourceDecimals)
}

// monthlyUSDVWAPRow is one prices_1mo row as stored: vwap and volume in
// the base spelling's raw stored unit.
type monthlyUSDVWAPRow struct {
	month             time.Time
	base              string
	vwap, vol, usdVol *big.Rat
	sources           []string
}

func scanMonthlyUSDVWAPRow(rows *sql.Rows) (monthlyUSDVWAPRow, error) {
	var (
		bucket                     time.Time
		base, vwapT, volT, usdVolT string
		sources                    stringArray
	)
	if err := rows.Scan(&bucket, &base, &vwapT, &volT, &usdVolT, &sources); err != nil {
		return monthlyUSDVWAPRow{}, fmt.Errorf("timescale: MonthlyUSDVWAPs scan: %w", err)
	}
	vwap, ok1 := new(big.Rat).SetString(vwapT)
	vol, ok2 := new(big.Rat).SetString(volT)
	usdVol, ok3 := new(big.Rat).SetString(usdVolT)
	if !ok1 || !ok2 || !ok3 {
		// A NUMERIC that does not parse is corruption, not a miss.
		return monthlyUSDVWAPRow{}, fmt.Errorf("timescale: MonthlyUSDVWAPs: unparseable numeric for %s @ %s (vwap=%q volume=%q volume_usd=%q)",
			base, bucket.UTC().Format(time.RFC3339), vwapT, volT, usdVolT)
	}
	return monthlyUSDVWAPRow{month: bucket.UTC(), base: base, vwap: vwap, vol: vol, usdVol: usdVol, sources: sources}, nil
}

// foldMonthlyUSDVWAPs is MonthlyUSDVWAPs' fold over scanned rows.
func foldMonthlyUSDVWAPs(in []monthlyUSDVWAPRow, sourceDecimals SourceAmountDecimals) ([]MonthlyUSDVWAP, error) {
	type key struct {
		asset string
		month int64 // unix seconds of the month start
	}
	type fold struct {
		month            time.Time
		quote, base, usd *big.Rat
	}
	folds := map[key]*fold{}
	for _, r := range in {
		scale, err := monthlyUSDVWAPScale(r.base, r.sources, sourceDecimals)
		if err != nil {
			return nil, fmt.Errorf("timescale: MonthlyUSDVWAPs: %s @ %s: %w", r.base, r.month.Format(time.RFC3339), err)
		}
		whole := new(big.Rat).Mul(r.vol, scale)
		k := key{asset: monthlyUSDVWAPAsset(r.base), month: r.month.Unix()}
		f := folds[k]
		if f == nil {
			f = &fold{month: r.month, quote: new(big.Rat), base: new(big.Rat), usd: new(big.Rat)}
			folds[k] = f
		}
		// vwap × whole-unit volume re-derives the row's quote leg in whole
		// units, so the fold is the union market's own Σ quote / Σ base —
		// not an average of prices, and not weighted by storage scale.
		f.quote.Add(f.quote, new(big.Rat).Mul(r.vwap, whole))
		f.base.Add(f.base, whole)
		f.usd.Add(f.usd, r.usdVol)
	}

	out := make([]MonthlyUSDVWAP, 0, len(folds))
	for k, f := range folds {
		if f.base.Sign() <= 0 {
			continue
		}
		usd, _ := f.usd.Float64() // i128:ok VolumeUSD is a ranking magnitude; the served VWAPUSD stays a decimal string
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

// classicAmountDecimals is the ledger's own amount scale: XLM and every
// classic asset (its SAC included) are stored in 10^-7 units whichever
// venue traded them.
const classicAmountDecimals = 7

// monthlyUSDVWAPScale is the factor (10^-decimals) that turns one row's
// stored base volume into whole units. The fold only needs rows sharing a
// key to agree, and keys cross spellings only through the alias registry:
//   - an on-chain classic spelling ('native', CODE-ISSUER, a registered
//     SAC) is stored at the ledger's 7 places;
//   - an off-chain spelling (crypto:, fiat:, …) is stored at its sources'
//     registered scale (CEX 10^8, FX 10^6), which the row's sources must
//     agree on — a row mixing scales is not one market's volume;
//   - anything else (a pure contract token, a non-asset spelling) folds
//     with its own spelling only, so it is left raw.
func monthlyUSDVWAPScale(raw string, sources []string, sourceDecimals SourceAmountDecimals) (*big.Rat, error) {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return big.NewRat(1, 1), nil
	}
	switch a.Type {
	case canonical.AssetNative, canonical.AssetClassic:
		return tenToMinus(classicAmountDecimals), nil
	case canonical.AssetSoroban:
		if t := canonical.CanonicalAsset(a).Type; t == canonical.AssetNative || t == canonical.AssetClassic {
			return tenToMinus(classicAmountDecimals), nil
		}
		return big.NewRat(1, 1), nil
	}
	if sourceDecimals == nil {
		return nil, fmt.Errorf("off-chain row and no source scale lookup")
	}
	decimals := -1
	for _, src := range sources {
		d := sourceDecimals(src)
		if decimals >= 0 && d != decimals {
			return nil, fmt.Errorf("sources %v stamp amounts at different scales (%d vs %d places)", sources, decimals, d)
		}
		decimals = d
	}
	if decimals < 0 {
		return nil, fmt.Errorf("off-chain row carries no source to read its amount scale from")
	}
	return tenToMinus(decimals), nil
}

func tenToMinus(decimals int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
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
