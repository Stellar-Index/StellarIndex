package timescale

import (
	"context"
	"fmt"
)

// ContractCatalogueRows reads catalogue rows for an explicit set of contract addresses, keyed by
// contract id.
//
// The listing spine cannot be reused: its `catalogue_assets` CTE and `GetAssetBySlug` gate the
// discovered-contract arm on a row in asset_volume_24h. A tokenized treasury or money-market fund
// is held, not traded, and can carry a nine-figure supply while absent from every 24h volume
// rollup, so that gate would silently drop a contract the membership decision admitted. The set
// here is bounded by membership, so there is no pagination or ranking.
//
// The price side is deliberately the same rollup under the same floor as the listing
// (asset_price_snapshot with `computed_at > now() - assetPriceSnapshotMaxAge`), so a wedged
// aggregator makes the join miss rather than serve an indefinitely old price. market_cap_usd and
// circulating_supply are NULL here; the API layer fills them.
//
// Contracts never observed are missing from the map; the caller reports that as a drop, not an
// error.
func (s *Store) ContractCatalogueRows(ctx context.Context, contractIDs []string) (map[string]AssetRow, error) {
	if len(contractIDs) == 0 {
		return map[string]AssetRow{}, nil
	}
	// ROUND(price, 10)::text and the to_char change formats are copied from the listing SELECT so
	// the wire string for a contract asset matches /v1/assets digit for digit.
	//
	// The rounding is safe for non-7-decimals contracts only because asset_price_snapshot stores
	// the decimals-corrected price (snapshotNormalizedPriceUSDExpr): a raw prices_1m ratio for an
	// 18-decimals token worth 1 USD is 1e-11, which ROUND would zero. The caller multiplies this
	// price by supply for market cap and must not scale it again.
	const q = `
		SELECT d.contract_id,
		       d.first_seen_ledger,
		       d.last_seen_ledger,
		       d.event_count,
		       ROUND(aps.price_usd, 10)::text,
		       vol.vol_usd,
		       to_char(aps.change_1h_pct,  'FM999999990.00'),
		       to_char(aps.change_24h_pct, 'FM999999990.00'),
		       to_char(aps.change_7d_pct,  'FM999999990.00'),
		       aps.source_count,
		       avc.character,
		       COALESCE(vol.unpriced_trades, 0) > 0
		  FROM discovered_assets d
		  LEFT JOIN asset_volume_24h       vol ON vol.asset_id = d.contract_id
		  LEFT JOIN asset_price_snapshot   aps ON aps.asset_id = d.contract_id
		                                      AND aps.computed_at > now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'
		  LEFT JOIN asset_volume_character avc ON avc.asset_id = d.contract_id
		 WHERE d.contract_id = ANY($1)`
	rows, err := s.db.QueryContext(ctx, q, contractIDs)
	if err != nil {
		return nil, fmt.Errorf("timescale: ContractCatalogueRows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]AssetRow, len(contractIDs))
	for rows.Next() {
		var r AssetRow
		if err := rows.Scan(
			&r.AssetID, &r.FirstSeenLedger, &r.LastSeenLedger, &r.ObservationCount,
			&r.PriceUSD, &r.Volume24hUSD,
			&r.Change1hPct, &r.Change24hPct, &r.Change7dPct,
			&r.SourceCount, &r.VolumeCharacter, &r.VolumeLowerBound,
		); err != nil {
			return nil, fmt.Errorf("timescale: scan contract catalogue row: %w", err)
		}
		// Code and IssuerGStrkey stay empty as in the listing spine: a contract asset has no issuer
		// account or SEP-1 code. Slug falls back to the contract id like the listing's COALESCE.
		r.Slug = r.AssetID
		out[r.AssetID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: contract catalogue rows: %w", err)
	}
	return out, nil
}
