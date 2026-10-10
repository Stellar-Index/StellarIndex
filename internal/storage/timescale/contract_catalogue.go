package timescale

import (
	"context"
	"fmt"
)

// ContractCatalogueRows reads catalogue rows for an EXPLICIT set of contract
// addresses, keyed by contract id.
//
// Why the listing spine could not be reused: the listing's `catalogue_assets` CTE
// and `GetAssetBySlug` gate the discovered-contract arm on `EXISTS (SELECT 1 FROM
// asset_volume_24h ...)`, right for a listing that wants assets with a market
// (it admitted 60 of ~117k discovered contracts on r1). A tokenized treasury or
// money-market fund is held, not traded, and can carry a nine-figure supply while
// absent from every 24h volume rollup. Under that gate a membership decision
// admitting it would be followed by a join that silently dropped it, reporting a
// coverage failure that was really a query-shape choice. Here the set is bounded
// by MEMBERSHIP (the contracts an independent party named), so there is no
// pagination or ranking.
//
// Shared with the listing, deliberately: the price side is the SAME rollup under
// the SAME floor (asset_price_snapshot with `computed_at > now() -
// assetPriceSnapshotMaxAge`, the shared constant), so a wedged aggregator makes
// the join MISS rather than serve an indefinitely old price. Volume, source count
// and volume character come from the same rollups. market_cap_usd and
// circulating_supply are NULL in the spine; the API layer fills them.
//
// Rows never observed are missing from the map; the caller reports that as a
// drop. It is not an error here.
func (s *Store) ContractCatalogueRows(ctx context.Context, contractIDs []string) (map[string]AssetRow, error) {
	if len(contractIDs) == 0 {
		return map[string]AssetRow{}, nil
	}
	// ROUND(price, 10)::text and the to_char change formats are copied
	// from the listing SELECT for one reason: the wire string a consumer
	// reads for a contract asset must be the string /v1/assets would have
	// produced for it, digit for digit. A different rounding here would
	// show up as the two surfaces disagreeing about the same token's
	// price.
	//
	// The rounding is safe for a non-7-decimals contract — the class this
	// read exists for — only because asset_price_snapshot stores the
	// decimals-CORRECTED price (snapshotNormalizedPriceUSDExpr): the raw
	// prices_1m ratio of an 18-decimals token worth 1 USD is 1e-11, which
	// this ROUND would turn into zero. The caller multiplies this price
	// by supply to publish a market cap, and must not scale it again.
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
		// Code, IssuerGStrkey and Slug stay empty, matching what the
		// listing spine selects for the same arm: a contract asset has no
		// issuer account and no SEP-1 code. Slug falls back to the
		// contract id the way the listing's COALESCE does, so the two
		// paths hand the API layer the same shape.
		r.Slug = r.AssetID
		out[r.AssetID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: contract catalogue rows: %w", err)
	}
	return out, nil
}
