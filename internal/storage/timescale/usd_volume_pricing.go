package timescale

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// UsdVolumePricingRow is one source's trade counts over a pricing window.
type UsdVolumePricingRow struct {
	Source string
	Trades int64
	Priced int64
	// Unpriced is usd_volume IS NULL on a routable pair. It includes thin
	// markets: the substance verdict is not stored on the row.
	Unpriced int64
	// Unroutable is unpriced AND both legs are credits of one issuer, a declared SAC wrapper folded onto its classic asset (no
	// independent market can value it); same rule as usdPopulatedLabel.
	Unroutable int64
}

// usdVolumePricingSQL is bounded by ts so chunk exclusion keeps it to the window's chunks; a classic id's issuer is its trailing 56
// characters, read after alias_fold maps a SAC leg to its classic form.
var usdVolumePricingSQL = `
WITH ` + aliasFoldCTE(4) + `
SELECT source,
       count(*),
       count(*) FILTER (WHERE usd_volume IS NOT NULL),
       count(*) FILTER (WHERE usd_volume IS NULL AND NOT same_issuer),
       count(*) FILTER (WHERE usd_volume IS NULL AND same_issuer)
  FROM (
        SELECT source, usd_volume,
               (b ~ '^.+-G[A-Z2-7]{55}$'
                AND q ~ '^.+-G[A-Z2-7]{55}$'
                AND right(b, 56) = right(q, 56)) AS same_issuer
          FROM (
                SELECT tr.source, tr.usd_volume,
                       COALESCE(bf.canon, tr.base_asset) AS b, COALESCE(qf.canon, tr.quote_asset) AS q
                  FROM trades tr
                  LEFT JOIN alias_fold bf ON bf.form = tr.base_asset
                  LEFT JOIN alias_fold qf ON qf.form = tr.quote_asset
                 WHERE tr.ts >= $1::timestamptz AND tr.ts < $2::timestamptz
                   AND tr.source = ANY($3::text[])
               ) folded
       ) t
 GROUP BY source
 ORDER BY source`

// UsdVolumePricingStats counts priced, unpriced and unroutable trades per source for sources over [from, to).
func (s *Store) UsdVolumePricingStats(ctx context.Context, from, to time.Time, sources []string) ([]UsdVolumePricingRow, error) {
	forms, canons := sacAliasFoldBind()
	rows, err := s.db.QueryContext(ctx, usdVolumePricingSQL, from, to, sources, forms, canons)
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
