package timescale

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// UsdVolumePricingRow is one source's trade counts over a pricing window.
// Trades == Priced + Unpriced + Unroutable.
type UsdVolumePricingRow struct {
	Source string
	Trades int64
	Priced int64
	// Unpriced is usd_volume IS NULL on a routable pair. It includes thin
	// markets: the substance verdict is not stored on the row.
	Unpriced int64
	// Unroutable is unpriced AND both legs are classic assets of one issuer
	// (no independent market can value it); same rule as usdPopulatedLabel.
	Unroutable int64
}

// usdVolumePricingSQL is bounded by ts, so chunk exclusion keeps it to the
// window's chunks. A classic asset id is "<code>-<G… issuer>", issuer being
// the trailing 56 characters.
const usdVolumePricingSQL = `
SELECT source,
       count(*),
       count(*) FILTER (WHERE usd_volume IS NOT NULL),
       count(*) FILTER (WHERE usd_volume IS NULL AND NOT same_issuer),
       count(*) FILTER (WHERE usd_volume IS NULL AND same_issuer)
  FROM (
        SELECT source, usd_volume,
               (base_asset  ~ '^.+-G[A-Z2-7]{55}$'
                AND quote_asset ~ '^.+-G[A-Z2-7]{55}$'
                AND right(base_asset, 56) = right(quote_asset, 56)) AS same_issuer
          FROM trades
         WHERE ts >= $1::timestamptz AND ts < $2::timestamptz
           AND source = ANY($3::text[])
       ) t
 GROUP BY source
 ORDER BY source`

// UsdVolumePricingStats counts priced, unpriced and unroutable trades per
// source for sources over [from, to). Every requested source gets a row, in
// name order; one with no trades in the window has all counts zero, so a
// silent venue is visible rather than absent.
func (s *Store) UsdVolumePricingStats(ctx context.Context, from, to time.Time, sources []string) ([]UsdVolumePricingRow, error) {
	rows, err := s.db.QueryContext(ctx, usdVolumePricingSQL, from, to, sources)
	if err != nil {
		return nil, fmt.Errorf("timescale: UsdVolumePricingStats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]UsdVolumePricingRow, len(sources))
	for rows.Next() {
		var r UsdVolumePricingRow
		if err := rows.Scan(&r.Source, &r.Trades, &r.Priced, &r.Unpriced, &r.Unroutable); err != nil {
			return nil, fmt.Errorf("timescale: UsdVolumePricingStats scan: %w", err)
		}
		seen[r.Source] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := append([]string(nil), sources...)
	sort.Strings(names)
	out := make([]UsdVolumePricingRow, 0, len(names))
	for i, name := range names {
		if i > 0 && name == names[i-1] {
			continue
		}
		r, ok := seen[name]
		if !ok {
			r = UsdVolumePricingRow{Source: name}
		}
		out = append(out, r)
	}
	return out, nil
}
