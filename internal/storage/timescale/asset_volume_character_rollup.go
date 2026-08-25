package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Volume-character rollup worker store methods (wash-and-scam-signals
// design §2). RefreshAssetVolumeCharacter runs the ALL-ASSET single-pass
// generalization of assetVolumeCharacterSQL over the `trades` hypertable
// and upserts one row per canonical asset into asset_volume_character
// (migration 0149); AssetVolumeCharacterRollup is the keyed-on-PK read the
// /v1/assets{,/{id}} paths use instead of the per-request 14-day trades
// roll (measured 4.09s on the USDC detail — the scan this moves off the
// request path). The rollup only MOVES the compute: every signal value it
// stores is byte-for-byte what the per-asset [Store.AssetVolumeCharacter]
// returns for the same asset, because both derive from the SAME raw
// double-precision sums through the SAME Go path
// ([volumeCharacterFromSums]).

// assetVolumeCharacterRollupSQLTemplate is the all-asset generalization of
// assetVolumeCharacterSQL. Where the per-asset query takes an alias array
// for ONE asset (base OR quote = ANY($1)), this projects EVERY trade onto
// BOTH its base_asset and its quote_asset (as "the asset", with the other
// side as counterpart) — a UNION ALL of the two projections — folds each
// raw side onto its CANONICAL asset via the alias_map (the same fold
// assetAliasArray applies per-asset, so a SAC twin and its classic agree),
// then GROUP BYs the canonical asset.
//
// The {{ALIAS_VALUES}} token is replaced (strings.Replace, not Sprintf —
// the LIKE patterns carry literal % that a format verb would mangle) by the
// alias-fold VALUES rows built from the process AliasRegistry (see
// buildAliasMapValues). $1 is the trailing window as an interval string;
// the alias-pair params follow from $2.
//
// Every per-asset signal is reproduced exactly:
//   - total_vol / *_vol are SUM(usd_volume::double precision) — the SAME
//     double the per-asset query sums, so the Go-derived shares match to
//     full precision.
//   - the (maker,taker) pair is UNORDERED (LEAST/GREATEST) so a round-trip
//     folds to the one concentrated pair it economically is.
//   - issuer is derived per canonical asset_id: the G-strkey suffix of a
//     classic 'CODE-GISSUER' id, ” for native/soroban/fiat/crypto — the
//     same value canonical.ParseAsset(assetID).Issuer gives the per-asset
//     query, so the issuer-side predicate matches.
//   - market_styled tests the RAW counterpart (native / fiat:% / USDC-%),
//     never the folded form — identical to the per-asset predicate.
//   - total_vol_num is the EXACT NUMERIC sum (ADR-0003) for the stored
//     volume_usd column; it renders the identical 2dp wire value.
const assetVolumeCharacterRollupSQLTemplate = `
WITH alias_map(form, canon) AS (
  VALUES {{ALIAS_VALUES}}
),
legs AS (
  SELECT
    COALESCE(bm.canon, t.base_asset)  AS asset_id,
    t.maker, t.taker,
    t.quote_asset                     AS counterpart,
    t.usd_volume::double precision     AS v,
    t.usd_volume                       AS v_num
  FROM trades t
  LEFT JOIN alias_map bm ON bm.form = t.base_asset
  WHERE t.ts >= now() - $1::interval
    AND t.usd_volume IS NOT NULL
  UNION ALL
  SELECT
    COALESCE(qm.canon, t.quote_asset) AS asset_id,
    t.maker, t.taker,
    t.base_asset                      AS counterpart,
    t.usd_volume::double precision     AS v,
    t.usd_volume                       AS v_num
  FROM trades t
  LEFT JOIN alias_map qm ON qm.form = t.quote_asset
  WHERE t.ts >= now() - $1::interval
    AND t.usd_volume IS NOT NULL
),
legs_i AS (
  SELECT
    asset_id, maker, taker, counterpart, v, v_num,
    CASE WHEN asset_id ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'
         THEN split_part(asset_id, '-', 2)
         ELSE '' END AS issuer
  FROM legs
),
pairs AS (
  SELECT asset_id, SUM(v) AS pv
    FROM legs_i
   WHERE maker IS NOT NULL AND taker IS NOT NULL
   GROUP BY asset_id, LEAST(maker, taker), GREATEST(maker, taker)
),
top_pair AS (
  SELECT asset_id, MAX(pv) AS top_pair_vol
    FROM pairs
   GROUP BY asset_id
)
SELECT
    a.asset_id,
    SUM(a.v)                                                                    AS total_vol,
    SUM(a.v_num)::text                                                          AS total_vol_num,
    COUNT(DISTINCT a.maker) FILTER (WHERE a.maker IS NOT NULL)                  AS distinct_makers,
    COUNT(DISTINCT a.taker) FILTER (WHERE a.taker IS NOT NULL)                  AS distinct_takers,
    COALESCE(MAX(tp.top_pair_vol), 0)                                           AS top_pair_vol,
    COALESCE(SUM(a.v) FILTER (WHERE a.maker IS NOT NULL AND a.maker = a.taker), 0) AS self_cross_vol,
    COALESCE(SUM(a.v) FILTER (WHERE a.issuer <> '' AND (a.maker = a.issuer OR a.taker = a.issuer)), 0) AS issuer_side_vol,
    COALESCE(SUM(a.v) FILTER (WHERE a.counterpart = 'native'
                                 OR a.counterpart LIKE 'fiat:%'
                                 OR a.counterpart LIKE 'USDC-%'), 0)            AS market_styled_vol
  FROM legs_i a
  LEFT JOIN top_pair tp ON tp.asset_id = a.asset_id
 GROUP BY a.asset_id
`

// assetVolumeCharacterRow is one scanned all-asset roll: the raw sums the
// Go derivation turns into shares + character. total is the double the
// per-asset query sums (drives the shares to full precision);
// totalVolNumeric is the exact NUMERIC text stored in volume_usd.
type assetVolumeCharacterRow struct {
	assetID                                             string
	total, topPair, selfCross, issuerSide, marketStyled float64
	totalVolNumeric                                     string
	makers, takers                                      int64
}

// buildAliasMapValues renders the alias-fold VALUES rows + their args from
// the process AliasRegistry, starting at placeholder startIdx. Forms are
// bound as params (never string-concatenated) and cast ::text so the CTE
// columns type as text for the join against trades.base/quote_asset.
func buildAliasMapValues(startIdx int) (valuesSQL string, args []any) {
	forms := canonical.AllAliasForms()
	if len(forms) == 0 {
		// Defensive: an empty registry never happens in the aggregator
		// (it installs the registry before the worker starts), but a
		// single no-op self-row keeps the VALUES list non-empty and the
		// COALESCE a pass-through. base/quote_asset is never '' in real
		// data, so this row matches nothing.
		return "('', '')", nil
	}
	keys := make([]string, 0, len(forms))
	for k := range forms {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic placeholder order
	var b strings.Builder
	idx := startIdx
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "($%d::text, $%d::text)", idx, idx+1)
		args = append(args, k, forms[k])
		idx += 2
	}
	return b.String(), args
}

// volumeCharacterFromSums derives the §2 signals + character from the raw
// double-precision sums. It is the SINGLE derivation shared by the
// per-asset [Store.AssetVolumeCharacter] and the all-asset rollup, so the
// rollup can never drift from the value the detail used to compute live —
// same inputs, same round4, same deriveVolumeCharacter.
func volumeCharacterFromSums(total, topPair, selfCross, issuerSide, marketStyled float64, makers, takers int64) AssetVolumeCharacter {
	out := AssetVolumeCharacter{
		WindowDays:     int(volumeCharacterWindow.Hours()) / 24,
		VolumeUSD:      total,
		DistinctMakers: makers,
		DistinctTakers: takers,
	}
	if total > 0 {
		out.TopAccountPairVolShare = round4(topPair / total)
		out.SelfCrossShare = round4(selfCross / total)
		out.IssuerSideShare = round4(issuerSide / total)
		out.MarketStyledShare = round4(marketStyled / total)
	}
	out.IsMarketStyled = out.MarketStyledShare >= volumeCharacterMarketStyledThreshold
	out.Character = deriveVolumeCharacter(out)
	return out
}

// refreshAssetVolumeCharacterPrune drops assets whose priced volume lapsed
// out of the window this pass — same one-transaction now() trick as the
// asset_volume_24h rollup: just-upserted rows carry computed_at = now() and
// survive; stale rows carry an older timestamp and are deleted.
const refreshAssetVolumeCharacterPrune = `DELETE FROM asset_volume_character WHERE computed_at < now()`

// RefreshAssetVolumeCharacter recomputes the per-asset volume-character
// rollup from the live trailing-window trades roll and atomically replaces
// its contents. Called on a slow cadence by the aggregator's
// assetcharacterrollup worker so /v1/assets{,/{id}} never runs the
// all-asset account-structure roll per request.
//
// The heavy all-asset SELECT runs OUTSIDE the transaction (a read); the
// upsert + prune run in ONE transaction (row-level locks only — no ACCESS
// EXCLUSIVE lock that would stall concurrent listing/detail reads on the
// customer-facing endpoints). computed_at is stamped to the transaction
// clock in SQL so the sibling prune sees just-written rows as current.
func (s *Store) RefreshAssetVolumeCharacter(ctx context.Context) error {
	rows, err := s.rollAssetVolumeCharacter(ctx)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertAssetVolumeCharacter(ctx, tx, rows); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, refreshAssetVolumeCharacterPrune); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter commit: %w", err)
	}
	return nil
}

// rollAssetVolumeCharacter runs the all-asset single-pass roll and returns
// one row per canonical asset with priced volume in the window.
func (s *Store) rollAssetVolumeCharacter(ctx context.Context) ([]assetVolumeCharacterRow, error) {
	window := fmt.Sprintf("%d hours", int(volumeCharacterWindow.Hours()))
	aliasValues, aliasArgs := buildAliasMapValues(2)
	query := strings.Replace(assetVolumeCharacterRollupSQLTemplate, "{{ALIAS_VALUES}}", aliasValues, 1)
	args := append([]any{window}, aliasArgs...)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: RefreshAssetVolumeCharacter roll: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []assetVolumeCharacterRow
	for rows.Next() {
		var r assetVolumeCharacterRow
		if err := rows.Scan(
			&r.assetID, &r.total, &r.totalVolNumeric,
			&r.makers, &r.takers, &r.topPair,
			&r.selfCross, &r.issuerSide, &r.marketStyled,
		); err != nil {
			return nil, fmt.Errorf("timescale: RefreshAssetVolumeCharacter scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: RefreshAssetVolumeCharacter rows: %w", err)
	}
	return out, nil
}

// assetVolumeCharacterUpsertCols is the column count per upserted row (11
// value columns; computed_at is the now() literal, not a param).
const assetVolumeCharacterUpsertCols = 11

// assetVolumeCharacterUpsertBatch bounds one multi-row INSERT so the param
// count (11/row) stays well under Postgres's 65535 limit.
const assetVolumeCharacterUpsertBatch = 400

// upsertAssetVolumeCharacter writes every rolled row in batched multi-row
// upserts. Rows absent this pass are handled by the sibling prune.
func upsertAssetVolumeCharacter(ctx context.Context, tx *sql.Tx, rows []assetVolumeCharacterRow) error {
	for start := 0; start < len(rows); start += assetVolumeCharacterUpsertBatch {
		end := start + assetVolumeCharacterUpsertBatch
		if end > len(rows) {
			end = len(rows)
		}
		if err := execAssetVolumeCharacterUpsert(ctx, tx, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func execAssetVolumeCharacterUpsert(ctx context.Context, tx *sql.Tx, batch []assetVolumeCharacterRow) error {
	var (
		placeholders = make([]string, 0, len(batch))
		args         = make([]any, 0, len(batch)*assetVolumeCharacterUpsertCols)
	)
	for i, r := range batch {
		vc := volumeCharacterFromSums(r.total, r.topPair, r.selfCross, r.issuerSide, r.marketStyled, r.makers, r.takers)
		base := i * assetVolumeCharacterUpsertCols
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,now())",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11,
		))
		args = append(args,
			r.assetID, vc.WindowDays, r.totalVolNumeric,
			vc.DistinctMakers, vc.DistinctTakers,
			vc.TopAccountPairVolShare, vc.SelfCrossShare,
			vc.IssuerSideShare, vc.MarketStyledShare,
			vc.IsMarketStyled, vc.Character,
		)
	}
	//nolint:gosec // G201: placeholders are compile-time-shaped ($N), never data.
	stmt := `INSERT INTO asset_volume_character AS t
	  (asset_id, window_days, volume_usd, distinct_makers, distinct_takers,
	   top_account_pair_vol_share, self_cross_share, issuer_side_share,
	   market_styled_share, is_market_styled, character, computed_at)
	VALUES ` + strings.Join(placeholders, ", ") + `
	ON CONFLICT (asset_id) DO UPDATE SET
	   window_days                = EXCLUDED.window_days,
	   volume_usd                 = EXCLUDED.volume_usd,
	   distinct_makers            = EXCLUDED.distinct_makers,
	   distinct_takers            = EXCLUDED.distinct_takers,
	   top_account_pair_vol_share = EXCLUDED.top_account_pair_vol_share,
	   self_cross_share           = EXCLUDED.self_cross_share,
	   issuer_side_share          = EXCLUDED.issuer_side_share,
	   market_styled_share        = EXCLUDED.market_styled_share,
	   is_market_styled           = EXCLUDED.is_market_styled,
	   character                  = EXCLUDED.character,
	   computed_at                = EXCLUDED.computed_at`
	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter upsert: %w", err)
	}
	return nil
}

// assetVolumeCharacterRollupSQL is the keyed-on-PK read the detail path
// uses. volume_usd is read back as double precision so VolumeUSD renders
// the identical 2dp wire value the per-asset path produced.
const assetVolumeCharacterRollupSQL = `
SELECT window_days, volume_usd::double precision, distinct_makers, distinct_takers,
       top_account_pair_vol_share, self_cross_share, issuer_side_share,
       market_styled_share, is_market_styled, character
  FROM asset_volume_character
 WHERE asset_id = $1`

// AssetVolumeCharacterRollup returns the pre-computed §2 signals + character
// for one asset from the asset_volume_character rollup — a keyed-on-PK
// lookup, no per-request trades roll. The assetID is folded to its
// canonical form first (the rollup keys canonical assets), so a SAC-form
// or crypto:XLM request resolves onto the same row its classic/native twin
// wrote. found=false means the asset has no rolled row (no priced trades in
// the window, or the worker hasn't run yet) — the caller omits the fields.
func (s *Store) AssetVolumeCharacterRollup(ctx context.Context, assetID string) (AssetVolumeCharacter, bool, error) {
	key := assetID
	if a, err := canonical.ParseAsset(assetID); err == nil {
		key = canonical.CanonicalAsset(a).String()
	}
	var out AssetVolumeCharacter
	err := s.db.QueryRowContext(ctx, assetVolumeCharacterRollupSQL, key).Scan(
		&out.WindowDays, &out.VolumeUSD, &out.DistinctMakers, &out.DistinctTakers,
		&out.TopAccountPairVolShare, &out.SelfCrossShare, &out.IssuerSideShare,
		&out.MarketStyledShare, &out.IsMarketStyled, &out.Character,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AssetVolumeCharacter{}, false, nil
	}
	if err != nil {
		return AssetVolumeCharacter{}, false, fmt.Errorf("timescale: AssetVolumeCharacterRollup %s: %w", assetID, err)
	}
	return out, true, nil
}
