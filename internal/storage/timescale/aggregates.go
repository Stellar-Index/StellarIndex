package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// Vwap1mRow is one row from the prices_1m continuous aggregate.
// The fields mirror migrations/0002_create_price_aggregates.up.sql
// — see that file for the SQL semantics. Bucket is the START of the
// 1-minute window; the window's END is `bucket + 1 minute`.
type Vwap1mRow struct {
	Bucket     time.Time
	BaseAsset  string
	QuoteAsset string
	// VWAP, FirstPrice, LastPrice, HighPrice, LowPrice are decimal
	// strings exactly as Postgres serialises NUMERIC. Storing them
	// as strings avoids a float round-trip (ADR-0003) — handlers
	// that need a numeric value parse with big.Rat.
	VWAP       string
	TradeCount int64
	Sources    []string
}

// LatestClosedVWAP1mForPair returns the most-recent CLOSED 1-minute
// bucket from the prices_1m CAGG for the given pair. Per ADR-0015
// the API serves only closed buckets — this method explicitly
// excludes the in-progress bucket via a `bucket + 1 minute <= now()`
// guard, even though the CAGG's refresh policy already drops the
// open bucket from materialised rows.
//
// Returns [sql.ErrNoRows] when the pair has no closed bucket yet —
// callers translate that to the API's price-not-found problem or
// fall back to the latest-trade path.
func (s *Store) LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (Vwap1mRow, error) {
	const q = `
        SELECT bucket, base_asset, quote_asset, vwap::text, trade_count, sources
          FROM prices_1m
         WHERE base_asset = $1
           AND quote_asset = $2
           AND bucket + INTERVAL '1 minute' <= now()
         ORDER BY bucket DESC
         LIMIT 1
    `
	var row Vwap1mRow
	err := s.db.QueryRowContext(ctx, q,
		p.Base.String(), p.Quote.String(),
	).Scan(
		&row.Bucket,
		&row.BaseAsset,
		&row.QuoteAsset,
		&row.VWAP,
		&row.TradeCount,
		(*stringArray)(&row.Sources),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: LatestClosedVWAP1mForPair: %w", err)
	}
	return row, nil
}

// stringArray is a [sql.Scanner] for Postgres TEXT[] / VARCHAR[]
// columns scanning into a Go []string. Used by the `sources` column
// in prices_1m.
//
// Implements minimal parsing of the Postgres array text format:
// `{a,b,c}`. Quoted entries (with embedded commas) aren't supported
// — fine here because source names are identifier-shaped.
type stringArray []string

// Scan implements [sql.Scanner].
func (a *stringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("stringArray: unsupported scan type %T", src)
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("stringArray: malformed Postgres array literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*a = []string{}
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(inner); i++ {
		if i == len(inner) || inner[i] == ',' {
			elt := inner[start:i]
			// NULL elements come through as the literal "NULL"
			// (case-sensitive); array_agg(DISTINCT source) over a
			// non-null source column never produces these, but
			// guard anyway.
			if elt != "NULL" {
				out = append(out, elt)
			}
			start = i + 1
		}
	}
	*a = out
	return nil
}
