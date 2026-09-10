package timescale

import (
	"context"
	"fmt"
)

// ContractCatalogueRows reads catalogue rows for an EXPLICIT set of
// contract addresses, keyed by contract id.
//
// # Why the listing spine could not be reused
//
// Both existing reads of a Soroban asset — the listing's
// `catalogue_assets` CTE and `GetAssetBySlug` — gate the
// discovered-contract arm on `EXISTS (SELECT 1 FROM asset_volume_24h
// …)`. That gate is right for those callers and is documented as such:
// discovered_assets holds ~117k contracts because SEP-41 event discovery
// catches anything emitting token events, and an asset LISTING wants the
// ones with a market. Measured on r1 2026-08-28, it admits 60 of them.
//
// It is wrong for this caller, and not marginally. A tokenized treasury
// or money-market fund is held, not traded: it can carry a nine-figure
// on-chain supply and never appear in a 24h volume rollup. Under that
// gate such a token is absent from the catalogue entirely — /v1/assets
// does not list it and /v1/assets/{id} 404s it — so a membership
// decision that admitted it would be followed by a join that silently
// dropped it. That is the exact defect the RWA funnel's
// `admitted_but_never_observed_on_chain` drop was added to expose, and
// exposing it here would report a coverage failure that was really a
// query-shape choice.
//
// The set this reads is bounded by MEMBERSHIP, decided before any
// number: the caller passes the contracts an independent party named and
// the definition admitted. There is no pagination to protect and no
// ranking to compute, so none of the spine's machinery applies.
//
// # What it shares with the listing, deliberately
//
// The price side is the SAME rollup under the SAME staleness floor:
// asset_price_snapshot joined with `computed_at > now() -
// assetPriceSnapshotMaxAge`, so a wedged aggregator makes this join MISS
// rather than serve an indefinitely-old price, exactly as it does for
// /v1/assets. The floor is the shared constant, not a copy of its value,
// so the two cannot drift apart. Volume, source count and volume
// character come from the same rollups by the same keys. What is absent
// is absent there too: market_cap_usd and circulating_supply are NULL in
// the spine for every asset, and the API layer fills them.
//
// Rows the catalogue has never observed are simply missing from the
// returned map. The caller reports that as a drop; it is not an error
// here.
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
		       avc.character
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
			&r.SourceCount, &r.VolumeCharacter,
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
