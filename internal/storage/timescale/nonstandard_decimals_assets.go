package timescale

import (
	"context"
	"fmt"
	"time"
)

// NonstandardDecimalsAsset is one confirmed non-7-decimal Soroban asset row from
// `nonstandard_decimals_assets`, the control table behind the dex-nonstandard-decimals serving
// guard. The aggregator's decimals-guard sweep writes it; the API's NonstandardDecimalsCache
// reads it to NORMALIZE served prices for pairs touching Asset, because prices_1m stores raw
// quote/base ratios and a non-7 leg needs an exact 10^(baseDecimals-quoteDecimals) correction
// (aggregate.AdjustPrice). See docs/operations/runbooks/dex.md.
//
// This is about per-unit PRICE, which needs real decimals. It is deliberately NOT consulted for
// usd_volume, where the token's scale cancels (see timescale.baseAnchorEligible).
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

// UpsertNonstandardDecimalsAsset records (or refreshes) a CONFIRMED non-7 decimals() declaration
// for asset. Idempotent: re-confirmation refreshes source/confirmed_at, and DO UPDATE also keeps
// `decimals` honest if a later sweep sees a corrected lake read. This is the WRITE half of the
// serving guard: the aggregator confirms, the API enforces.
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

// LoadNonstandardDecimalsAssets returns every confirmed non-7-decimal asset row. The API cache
// reads it on a ~60s background refresh; the table is tiny, so the unfiltered read is cheap and
// there is no per-request query on this path.
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

// DeleteNonstandardDecimalsAsset removes asset's confirmed-offender row. Deleting an absent row
// is not an error, since the guard's reconcile may race a manual operator clean-up.
//
// The one caller is the decimals-guard lockstep reconcile (Guard.Reconcile): the table projects
// the lake's on-chain decimals() for the non-7 subset, and its CHECK (decimals <> 7) means a
// token the lake now confirms as 7 dp cannot be corrected in place, so the row must be dropped.
// It is never called for a token the lake cannot resolve (found=false or a read error): that is
// not evidence the row is wrong, and an operator hand-seeded row for an uncaptured instance must
// survive a lake that cannot see it.
func (s *Store) DeleteNonstandardDecimalsAsset(ctx context.Context, asset string) error {
	const q = `DELETE FROM nonstandard_decimals_assets WHERE asset = $1`
	if _, err := s.db.ExecContext(ctx, q, asset); err != nil {
		return fmt.Errorf("timescale: DeleteNonstandardDecimalsAsset %s: %w", asset, err)
	}
	return nil
}
