// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// xlmUSDSeriesTTL bounds how stale a sparkline's XLM/USD leg can be; asset pages already serve from a 120 s response cache.
const xlmUSDSeriesTTL = 30 * time.Second

// freezeFingerprintSQL changes whenever a freeze is recorded, recovered or removed, so a cached series never outlives the
// freeze state it was computed under: a frozen minute must stop anchoring at once, as it does in the uncached headline price.
const freezeFingerprintSQL = `
		SELECT count(*)::text || '|' || coalesce(max(frozen_at)::text, '') || '|' || coalesce(max(recovered_at)::text, '')
		  FROM freeze_events`

// The anchor per hour of the last 24h and per day of the last 7d. It is the same for every asset, and inside each
// sparkline query it cost ~100 ms (7d) per cold asset page, so it is read once per TTL and bound in.
var (
	xlmUSDPerHourSQL = `WITH` + xlmUSDAnchorPerPeriodCTE("p", "t", "hour",
		`date_trunc('hour', now() - INTERVAL '23 hours')`, "$1::text") + `
		SELECT t, vwap::text FROM p`
	xlmUSDPerDaySQL = `WITH` + xlmUSDAnchorPerPeriodCTE("p", "t", "day",
		`date_trunc('day', now() - INTERVAL '6 days')`, "$1::text") + `
		SELECT t, vwap::text FROM p`
)

// xlmUSDSeriesCache memoizes one anchor series; mu is held across the read so a crawl's concurrent misses share one query.
type xlmUSDSeriesCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	freezes   string // freezeFingerprintSQL at read time
	periods   []time.Time
	vwaps     []string // numeric as text, so the bound value is exact
}

// xlmUSDSeries returns the series q renders, re-read when older than [xlmUSDSeriesTTL] or when the freeze state changed.
func (s *Store) xlmUSDSeries(ctx context.Context, c *xlmUSDSeriesCache, q string) ([]time.Time, []string, error) {
	var freezes string
	if err := s.db.QueryRowContext(ctx, freezeFingerprintSQL).Scan(&freezes); err != nil {
		return nil, nil, fmt.Errorf("timescale: xlm/usd series freeze state: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < xlmUSDSeriesTTL && c.freezes == freezes {
		return c.periods, c.vwaps, nil
	}
	rows, err := s.db.QueryContext(ctx, q, canonical.NativeSACContractID())
	if err != nil {
		return nil, nil, fmt.Errorf("timescale: xlm/usd series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	periods, vwaps := []time.Time{}, []string{}
	for rows.Next() {
		var t time.Time
		var v string
		if err := rows.Scan(&t, &v); err != nil {
			return nil, nil, fmt.Errorf("timescale: xlm/usd series scan: %w", err)
		}
		periods, vwaps = append(periods, t), append(vwaps, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("timescale: xlm/usd series rows: %w", err)
	}
	c.fetchedAt, c.freezes, c.periods, c.vwaps = time.Now(), freezes, periods, vwaps
	return periods, vwaps, nil
}

// xlmUSDSeriesCTE renders CTE name(col, vwap) from a series bound as $p (periods) and $p+1 (vwaps).
func xlmUSDSeriesCTE(name, col string, p int) string {
	return fmt.Sprintf(`
		%s AS (
		  SELECT u.%s, u.v::numeric AS vwap
		    FROM unnest($%d::timestamptz[], $%d::text[]) AS u(%s, v)
		)`, name, col, p, p+1, col)
}
