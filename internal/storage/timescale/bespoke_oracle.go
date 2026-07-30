package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// bespokeOracle builds the oracle bespoke block (reflector-dex/cex/fx, band,
// redstone) from oracle_updates scoped by source: feed count (distinct
// asset/quote), update cadence (updates/day), and a latest-prices table.
// price is a NUMERIC at the row's `decimals` scale — shown raw with the scale
// column, not rescaled (decimals can vary per feed).
import (
	"context"
	"fmt"
)

func (s *Store) bespokeOracle(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "oracle",
		Notes: []string{
			"Scoped to oracle_updates.source = the protocol's feed contract. price is the raw on-chain integer at the row's `decimals` scale (not rescaled — decimals can differ per feed). asset/quote are the feed pair; asset is a token contract id shown shortened, quote is often `native`.",
		},
	}

	var updates, feeds, perDay string
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*)::text,
		       count(DISTINCT (asset, quote))::text,
		       CASE WHEN $2 > 0 THEN round(count(*)::numeric / $2, 1)::text ELSE '0' END
		FROM oracle_updates
		WHERE source = $1 AND ts > now() - $3::interval`, source, windowDays, since).
		Scan(&updates, &feeds, &perDay)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeOracle KPIs: %w", err)
	}
	if updates == "0" {
		return nil, nil // no updates for this feed in the window — omit, don't show zeros
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Updates (%dd)", windowDays), Value: updates},
		BespokeKPI{Label: "Distinct feeds", Value: feeds, Hint: "distinct asset/quote pairs seen in the window"},
		BespokeKPI{Label: "Updates / day", Value: perDay, Hint: "mean update cadence over the window"},
	)

	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ts), 'YYYY-MM-DD'), count(*)::text
		FROM oracle_updates WHERE source = $1 AND ts > now() - $2::interval
		GROUP BY 1 ORDER BY 1 ASC`, source, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily updates", Unit: "updates", Points: series})
	}

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Latest feed prices", Columns: []string{"Asset", "Quote", "Price (raw)", "Decimals", "Updated"}},
		`SELECT asset,
		        quote,
		        COALESCE(price::text,'—'),
		        COALESCE(decimals::text,'—'),
		        to_char(ts, 'YYYY-MM-DD HH24:MI')
		   FROM (
		     SELECT DISTINCT ON (asset, quote) asset, quote, price, decimals, ts
		       FROM oracle_updates
		      WHERE source = $1 AND ts > now() - $2::interval
		      ORDER BY asset, quote, ts DESC
		   ) latest
		  ORDER BY ts DESC LIMIT 50`, source, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return blk, nil
}
