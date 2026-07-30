package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// bespokeDEX builds the DEX/AMM bespoke block (soroswap / phoenix / aquarius
// / comet / sdex) from the dex_volume_by_pair_1d continuous aggregate
// (migration 0064): windowed USD volume + trade count + unique pairs KPIs, a
// daily USD-volume series, and a top-pairs-by-volume table. Queries the CAGG,
// never raw `trades` (a direct 90d GROUP BY over the 313M-row hypertable
// measured ~15.7s).
import (
	"context"
	"fmt"
)

func (s *Store) bespokeDEX(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "dex",
		Notes: []string{
			"Volume is summed USD volume (vol) from the dex_volume_by_pair_1d daily continuous aggregate over the window; trades whose quote never resolved to a USD price contribute 0 to volume but still count toward trade and pair totals.",
			"Base turnover is in base-asset base units (per-asset decimals), not USD.",
		},
	}

	// KPIs: window USD volume, trade count, unique pairs.
	var vol, txCount, pairs string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(vol),0)::text,
		       COALESCE(sum(trades),0)::text,
		       count(DISTINCT (base_asset, quote_asset))::text
		FROM dex_volume_by_pair_1d
		WHERE source = $1 AND bucket > now() - $2::interval`, source, since).
		Scan(&vol, &txCount, &pairs)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeDEX KPIs: %w", err)
	}
	if txCount != "0" {
		blk.KPIs = append(blk.KPIs,
			BespokeKPI{Label: fmt.Sprintf("USD volume (%dd)", windowDays), Value: vol, Unit: "USD", Hint: "summed usd_volume of priced trades over the window"},
			BespokeKPI{Label: fmt.Sprintf("Trades (%dd)", windowDays), Value: txCount},
			BespokeKPI{Label: fmt.Sprintf("Active pairs (%dd)", windowDays), Value: pairs, Hint: "distinct base/quote pairs traded in the window"},
		)

		// Daily USD-volume series.
		series, err := s.scanDailySeries(ctx, `
			SELECT to_char(bucket, 'YYYY-MM-DD'), COALESCE(sum(vol),0)::text
			FROM dex_volume_by_pair_1d
			WHERE source = $1 AND bucket > now() - $2::interval
			GROUP BY 1 ORDER BY 1 ASC`, source, since)
		if err != nil {
			return nil, err
		}
		if len(series) > 0 {
			blk.Series = append(blk.Series, BespokeSeries{Name: "Daily USD volume", Unit: "USD", Points: series})
		}

		// Top pairs by USD volume.
		tbl, err := s.scanTable(ctx,
			BespokeTable{Title: "Top pairs by USD volume", Columns: []string{"Base", "Quote", "Trades", "USD volume", "Base turnover"}},
			`SELECT base_asset, quote_asset,
			        COALESCE(sum(trades),0)::text,
			        COALESCE(sum(vol),0)::text,
			        COALESCE(sum(base_vol),0)::text
			   FROM dex_volume_by_pair_1d
			  WHERE source = $1 AND bucket > now() - $2::interval
			  GROUP BY base_asset, quote_asset
			  ORDER BY sum(vol) DESC NULLS LAST, sum(trades) DESC LIMIT 25`, source, since)
		if err != nil {
			return nil, err
		}
		if len(tbl.Rows) > 0 {
			blk.Tables = append(blk.Tables, tbl)
		}
	}

	// Per-source captured-liquidity augments (reserves / net-flow depth /
	// staking / skim) — independent of trade volume so a quiet window still
	// surfaces depth, and empty-safe until the decoders have captured
	// anything on r1.
	if err := s.dexSourceAugments(ctx, blk, source, windowDays); err != nil {
		return nil, err
	}

	// Omit an all-empty block — a dormant DEX, or soroswap-router whose
	// swaps are ContractCall-derived and never land in `trades` — rather
	// than render an all-zero DEX panel.
	if len(blk.KPIs) == 0 {
		return nil, nil
	}
	return blk, nil
}

// dexSourceAugments adds the per-source captured-liquidity surfaces to a DEX
// block, keyed on source. Each augment is empty-safe (a no-op when its table
// holds nothing in the window):
//
//	aquarius → pool reserves / depth (aquarius_reserves, migration 0089)
//	comet    → net liquidity flow (comet_liquidity, migration 0042) + CS-026 caveat
//	phoenix  → net liquidity flow (phoenix_liquidity) + LP staking (phoenix_stake_events, migration 0044)
//	soroswap → skim KPIs (soroswap_skim_events, migration 0043)
//
// Split out of bespokeDEX so the per-source dispatch stays under the
// cognitive-complexity ceiling.
