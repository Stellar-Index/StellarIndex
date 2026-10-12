package timescale

import (
	"context"
	"fmt"
	"time"
)

// NonstandardDecimalsAsset is one confirmed non-7-decimal Soroban asset row behind the serving guard; prices_1m stores raw ratios, so pairs touching it need an exact 10^(baseDecimals-quoteDecimals) correction (not applied to usd_volume).
type NonstandardDecimalsAsset struct {
	// Asset is the token's C-strkey contract id.
	Asset string
	// Decimals is the on-chain decimals() value the guard resolved from the
	// certified lake. Always != 7 (that's the entire reason the row exists).
	Decimals int
	// Source is the DEX connector whose trade triggered the confirming
	// sweep (informational — the decline applies regardless of which
	// source produced the queried pair).
	Source      string
	ConfirmedAt time.Time
}

// UpsertNonstandardDecimalsAsset records (or refreshes) a CONFIRMED non-7 decimals() declaration for asset.
func (s *Store) UpsertNonstandardDecimalsAsset(ctx context.Context, asset string, decimals uint32, source string) error {
	const q = `
INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (asset) DO UPDATE SET
    decimals     = EXCLUDED.decimals,
    source       = EXCLUDED.source,
    confirmed_at = EXCLUDED.confirmed_at`
	if _, err := s.db.ExecContext(ctx, q, asset, int(decimals), source); err != nil {
		return fmt.Errorf("timescale: UpsertNonstandardDecimalsAsset %s: %w", asset, err)
	}
	return nil
}

// LoadNonstandardDecimalsAssets returns every confirmed non-7-decimal asset row.
func (s *Store) LoadNonstandardDecimalsAssets(ctx context.Context) ([]NonstandardDecimalsAsset, error) {
	const q = `SELECT asset, decimals, source, confirmed_at FROM nonstandard_decimals_assets`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LoadNonstandardDecimalsAssets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NonstandardDecimalsAsset
	for rows.Next() {
		var row NonstandardDecimalsAsset
		if err := rows.Scan(&row.Asset, &row.Decimals, &row.Source, &row.ConfirmedAt); err != nil {
			return nil, fmt.Errorf("timescale: LoadNonstandardDecimalsAssets scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LoadNonstandardDecimalsAssets rows: %w", err)
	}
	return out, nil
}

// DeleteNonstandardDecimalsAsset removes asset's confirmed-offender row (absent is not an error); only for a token the lake now confirms as 7 dp, never when the lake cannot resolve it, so hand-seeded rows survive.
func (s *Store) DeleteNonstandardDecimalsAsset(ctx context.Context, asset string) error {
	const q = `DELETE FROM nonstandard_decimals_assets WHERE asset = $1`
	if _, err := s.db.ExecContext(ctx, q, asset); err != nil {
		return fmt.Errorf("timescale: DeleteNonstandardDecimalsAsset %s: %w", asset, err)
	}
	return nil
}
