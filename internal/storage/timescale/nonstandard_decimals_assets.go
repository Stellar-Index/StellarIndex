package timescale

import (
	"context"
	"fmt"
	"time"
)

// NonstandardDecimalsAsset is one confirmed non-7-decimal Soroban asset row
// from `nonstandard_decimals_assets` (migration 0093) — the read-side
// control table backing the dex-nonstandard-decimals serving guard.
// Written by the aggregator's decimals-guard sweep (internal/decimalsguard)
// on confirmation; consulted by the API's process-local
// NonstandardDecimalsCache (internal/api/v1) to NORMALIZE served prices for
// pairs touching Asset — prices_1m stores raw quote/base ratios, so a
// non-7 leg needs an exact 10^(baseDecimals-quoteDecimals) correction
// (aggregate.AdjustPrice). Earlier revisions declined to serve instead;
// that was the placeholder, normalization is the shipped behaviour.
// See docs/operations/runbooks/dex-nonstandard-decimals.md.
//
// NOTE this table is about per-unit PRICE, which does need real decimals.
// It is deliberately NOT consulted for usd_volume, where the token's
// scale cancels — see timescale.baseAnchorEligible.
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

// UpsertNonstandardDecimalsAsset records (or refreshes) a CONFIRMED non-7
// decimals() declaration for asset. Idempotent: a later sweep re-confirming
// the same asset just refreshes source/confirmed_at — a token's decimals()
// is effectively immutable, so `decimals` itself is not expected to change,
// but DO UPDATE keeps the row honest if a later sweep ever observes a
// corrected lake read.
//
// Called by internal/decimalsguard.Guard at the same point it fires
// obs.DEXTradeNonstandardDecimalsTotal — this is the WRITE half of the
// read-time serving guard; the aggregator confirms, the API enforces.
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

// LoadNonstandardDecimalsAssets returns every confirmed non-7-decimal asset
// row. Read by the API's NonstandardDecimalsCache (internal/api/v1) on a
// ~60s background refresh cadence — the table is tiny (confirmed offenders
// should be near-zero) so an unfiltered full-table read is cheap and there
// is no per-request query on this path.
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
