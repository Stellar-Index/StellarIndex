package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Volume-character rollup worker store methods. RefreshAssetVolumeCharacter runs the all-asset
// single-pass generalization of assetVolumeCharacterSQL over `trades` and upserts one row per
// canonical asset into asset_volume_character; AssetVolumeCharacterRollup is the keyed-on-PK read
// the /v1/assets{,/{id}} paths use instead of the per-request 14-day trades roll. The rollup only
// moves the compute: every stored signal equals what the per-asset [Store.AssetVolumeCharacter]
// returns, because both derive from the same raw double sums through [volumeCharacterFromSums].

// assetVolumeCharacterRollupSQLTemplate is the all-asset form of assetVolumeCharacterSQL: each trade is projected onto both legs, folded
// onto its canonical asset via the alias_map, then grouped. {{ALIAS_VALUES}} is swapped by strings.Replace (the LIKE patterns carry a
// literal %); {{WINDOW}} is an interval literal because a bind parameter stops chunk exclusion and this long roll starved the compression
// policy. total_vol and *_vol sum usd_volume::double precision exactly as the per-asset query does; total_vol_num is the exact NUMERIC
// sum.
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
  WHERE t.ts >= now() - interval '{{WINDOW}}'
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
  WHERE t.ts >= now() - interval '{{WINDOW}}'
    AND t.usd_volume IS NOT NULL
),
legs_i AS (
  SELECT
    asset_id, ` + volumeCharacterMakerSQL + `,
    taker, counterpart, v, v_num,
    CASE WHEN asset_id ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'
         THEN split_part(asset_id, '-', 2)
         ELSE '' END AS issuer
  FROM legs
),
pairs AS (
  SELECT asset_id, SUM(v) AS pv
    FROM legs_i
   WHERE ` + volumeCharacterPairFilterSQL + `
   GROUP BY asset_id, ` + volumeCharacterPairKeySQL + `
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

// assetVolumeCharacterRow is one scanned roll: total is the double sum driving shares; totalVolNumeric is the exact NUMERIC text for
// volume_usd.
type assetVolumeCharacterRow struct {
	assetID                                             string
	total, topPair, selfCross, issuerSide, marketStyled float64
	totalVolNumeric                                     string
	makers, takers                                      int64
}

// buildAliasMapValues renders the alias-fold VALUES rows and args from the AliasRegistry from placeholder startIdx; forms are bound, cast
// ::text.
func buildAliasMapValues(startIdx int) (valuesSQL string, args []any) {
	forms := canonical.AllAliasForms()
	if len(forms) == 0 {
		// Defensive: the aggregator installs the registry before the worker starts, but a no-op
		// self-row keeps the VALUES list non-empty and the COALESCE a pass-through. base/quote_asset
		// is never '' in real data, so this row matches nothing.
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

// volumeCharacterFromSums is the single derivation shared with [Store.AssetVolumeCharacter], so the rollup cannot drift from the live
// detail; the exact NUMERIC text is the volume, the double sums only form shares.
func volumeCharacterFromSums(totalNum string, total, topPair, selfCross, issuerSide, marketStyled float64, makers, takers int64) AssetVolumeCharacter {
	out := AssetVolumeCharacter{
		WindowDays:     int(volumeCharacterWindow.Hours()) / 24,
		VolumeUSD:      totalNum,
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

// assetVolumeCharacterRollTimeout bounds one roll; the two-scan roll measures
// ~23 min on the full lake, so the bound leaves headroom while still guarding a wedge.
const assetVolumeCharacterRollTimeout = "45min"

// AssetVolumeCharacterRollApplicationName tags the roll's backend so a deploy can pg_cancel_backend it before a migration ALTERs trades.
const AssetVolumeCharacterRollApplicationName = "stellarindex-asset-character-roll"

// refreshAssetVolumeCharacterPrune drops assets that lapsed out of the window; upserted rows carry computed_at = now() (same transaction)
// and survive.
const refreshAssetVolumeCharacterPrune = `DELETE FROM asset_volume_character WHERE computed_at < now()`

// refreshAssetVolumeCharacterPruneExpired is the zero-row pass's prune (see
// [pruneRollup]); the interval is volumeCharacterWindow, pinned by test.
const refreshAssetVolumeCharacterPruneExpired = `DELETE FROM asset_volume_character WHERE computed_at < now() - INTERVAL '14 days'`

// RefreshAssetVolumeCharacter recomputes the per-asset volume-character rollup from the trailing-window trades roll and atomically replaces its contents, so /v1/assets never runs the all-asset roll per request.
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
	if err := pruneRollup(ctx, tx, int64(len(rows)), refreshAssetVolumeCharacterPrune, refreshAssetVolumeCharacterPruneExpired); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolumeCharacter commit: %w", err)
	}
	return nil
}

// AssetVolumeCharacterComputedAt returns the newest roll time, zero when the rollup is empty.
func (s *Store) AssetVolumeCharacterComputedAt(ctx context.Context) (time.Time, error) {
	var at sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT max(computed_at) FROM asset_volume_character`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("timescale: AssetVolumeCharacterComputedAt: %w", err)
	}
	return at.Time, nil
}

// rollAssetVolumeCharacter returns one row per canonical asset with priced volume. Like [Store.refreshCAGGBounded], it restores the pinned
// connection's statement_timeout on a detached context, and marks the connection bad (driver.ErrBadConn) if the restore fails.
func (s *Store) rollAssetVolumeCharacter(ctx context.Context) (out []assetVolumeCharacterRow, err error) {
	window := fmt.Sprintf("%d hours", int(volumeCharacterWindow.Hours()))
	aliasValues, args := buildAliasMapValues(1)
	query := strings.Replace(assetVolumeCharacterRollupSQLTemplate, "{{ALIAS_VALUES}}", aliasValues, 1)
	query = strings.ReplaceAll(query, "{{WINDOW}}", window)

	// This all-asset roll scans the whole trailing window of trades and shares the primary with the
	// API, so it runs on a dedicated connection with a bounded footprint:
	//   - max_parallel_workers_per_gather=2 leaves cores free for the API.
	//   - statement_timeout=assetVolumeCharacterRollTimeout is a wedge guard: if it can't finish it
	//     aborts, the last good rollup stands and the worker retries next cycle.
	// Session settings, not SET LOCAL, so they cover the read that runs outside any transaction.
	conn, connErr := s.db.Conn(ctx)
	if connErr != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter conn: %w", connErr)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET max_parallel_workers_per_gather = 2"); err != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter cap parallelism: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "RESET max_parallel_workers_per_gather") }()

	var prevTimeout string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&prevTimeout); err != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter read statement_timeout: %w", err)
	}
	var prevAppName string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('application_name')`).Scan(&prevAppName); err != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter read application_name: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT set_config('application_name', $1, false)`, AssetVolumeCharacterRollApplicationName); err != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter set application_name: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = '"+assetVolumeCharacterRollTimeout+"'"); err != nil {
		return nil, fmt.Errorf("timescale: rollAssetVolumeCharacter set timeout: %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), caggRefreshRestoreTimeout)
		defer rcancel()
		_, rerr := conn.ExecContext(rctx, `SELECT set_config('statement_timeout', $1, false)`, prevTimeout)
		if rerr == nil {
			_, rerr = conn.ExecContext(rctx, `SELECT set_config('application_name', $1, false)`, prevAppName)
		}
		if rerr != nil {
			// Never hand a connection back with this call's bound still on
			// it. Discard it; the pool dials a fresh one.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("timescale: rollAssetVolumeCharacter restore statement_timeout: %w", rerr)
			}
		}
	}()

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: RefreshAssetVolumeCharacter roll: %w", err)
	}
	defer func() { _ = rows.Close() }()

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
		vc := volumeCharacterFromSums(r.totalVolNumeric, r.total, r.topPair, r.selfCross, r.issuerSide, r.marketStyled, r.makers, r.takers)
		base := i * assetVolumeCharacterUpsertCols
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,now())",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11,
		))
		args = append(args,
			r.assetID, vc.WindowDays, vc.VolumeUSD,
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

// assetVolumeCharacterRollupSQL is the PK read for the detail path; volume_usd is read as NUMERIC text so money never passes a double
// (ADR-0003).
const assetVolumeCharacterRollupSQL = `
SELECT window_days, volume_usd::text, distinct_makers, distinct_takers,
       top_account_pair_vol_share, self_cross_share, issuer_side_share,
       market_styled_share, is_market_styled, character
  FROM asset_volume_character
 WHERE asset_id = $1`

// AssetVolumeCharacterRollup returns one asset's precomputed signals and character by PK lookup; the assetID is folded to canonical form first so SAC and crypto:XLM spellings hit the same row as their classic/native twin.
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
