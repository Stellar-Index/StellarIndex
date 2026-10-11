package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// assetAliasArray returns every canonical alias form of assetKey for `= ANY($n)`, so detail reads
// aggregate over all identities (XLM's native / crypto:XLM / SAC), not the caller's spelling.
// Order is canonical priority with the SAC last, so array_position prefers the deep classic pool.
// A key that does not parse falls back to itself.
func assetAliasArray(assetKey string) []string {
	a, err := canonical.ParseAsset(assetKey)
	if err != nil {
		return []string{assetKey}
	}
	return canonical.AssetAliasStrings(a)
}

// aliasVolumeLowerBoundSQL is true when any alias form in $1 (an
// [assetAliasArray]) has trades its served volume excludes as unpriced.
const aliasVolumeLowerBoundSQL = `EXISTS (SELECT 1 FROM asset_volume_24h u
		 WHERE u.asset_id = ANY($1) AND u.unpriced_trades > 0)`

// assetAliasRows is [assetAliasArray] for a batch read: every requested id
// expanded to its alias forms as parallel arrays (form, owning id, the
// form's priority), so the SQL can stay alias-complete per id while the
// result stays keyed on the id the caller passed.
func assetAliasRows(assetIDs []string) (forms, owners []string, prios []int64) {
	for _, id := range assetIDs {
		for i, form := range assetAliasArray(id) {
			forms = append(forms, form)
			owners = append(owners, id)
			prios = append(prios, int64(i+1))
		}
	}
	return forms, owners, prios
}

// AssetRow is the read-side projection of one asset-discovery row: classic_assets joined with the
// supply and activity counters. String fields keep it decoupled from the canonical package.
// VWAP, Volume24hUSD and MarketCapUSD are pointers so an asset with no computed value (new, or
// illiquid with no off-chain peg) emits JSON null.
type AssetRow struct {
	Slug             string
	AssetID          string
	Code             string
	IssuerGStrkey    string
	FirstSeenLedger  uint32
	LastSeenLedger   uint32
	ObservationCount int64
	// ObservationCountUnmeasured marks ObservationCount as not a count at
	// all (native XLM has no registry row); the API omits the field.
	ObservationCountUnmeasured bool

	// Latest VWAP-against-USD if available. Nil when:
	//   - no off-chain peg (illiquid Soroban-only token), or
	//   - the asset has no `fiat:USD` quote pair, or
	//   - the freeze writer has frozen this pair.
	PriceUSD *string
	// Trailing-24h USD-denominated trade volume. Nil when no
	// trades hit `usd_volume`-eligible quotes in 24h.
	Volume24hUSD *string
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time
	// usd_volume (asset_volume_24h.unpriced_trades > 0).
	VolumeLowerBound bool
	// Market cap in USD = price × circulating_supply when both
	// are known. Nil when either component is missing.
	MarketCapUSD *string
	// Circulating supply in canonical units (decimal string;
	// scale matches asset decimals). Nil for assets the supply
	// pipeline doesn't yet cover.
	CirculatingSupply *string
	// Trailing 1h / 24h / 7d price change as a signed percentage with two decimals ("+1.27").
	// Nil without a current price, or when no past prices_1m bucket exists within the window's
	// tolerance (5min for 1h, 30min for 24h, 2h for 7d).
	Change1hPct  *string
	Change24hPct *string
	Change7dPct  *string
	// SourceCount is the number of DISTINCT venues behind PriceUSD in the price's minute. The
	// market-cap guard suppresses the cap on a single venue with sub-floor volume. Nil means
	// "unmeasured" (triangulated or single-row price) and never suppresses.
	SourceCount *int

	// VolumeCharacter is the market|operational|concentrated label from the
	// asset_volume_character rollup, LEFT JOINed by asset_id. Nil when unrolled. Display only: it
	// never alters the raw Volume24hUSD.
	VolumeCharacter *string
	// SortVolume24hUSD is the concentration-adjusted 24h volume (raw x (1 - top_account_pair_vol_share)
	// for concentrated/operational) that the default volume sort ranks on. It is a SORT KEY only,
	// encoded in the keyset cursor so paging matches the ORDER BY; never displayed. Nil outside
	// the listing query.
	SortVolume24hUSD *string
	// RankTier is the listing's primary ascending sort key (see [listingRankTierExpr]: 0 rankable,
	// 1 unpriced, 2 directory-flagged). Like SortVolume24hUSD it is a sort key only, encoded in
	// the cursor. Nil outside the listing query.
	RankTier *int
}

// AssetsOrder selects the sort and cursor scheme used by ListAssets: ObservationCountDesc ranks
// by activity (default), Volume24hUSDDesc by live volume.
type AssetsOrder int

const (
	// AssetsOrderObservationCountDesc orders by all-time observation
	// count desc (a cheap activity proxy). Cursor is
	// `<obs_count>:<asset_id>`.
	AssetsOrderObservationCountDesc AssetsOrder = iota
	// AssetsOrderVolume24hUSDDesc orders by trailing-24h USD volume
	// desc (NULLS LAST), with `<asset_id>` as the tie-breaker.
	// Cursor is `<vol_or_blank>:<asset_id>`.
	AssetsOrderVolume24hUSDDesc
)

// ListAssetsOptions bundles the optional filters / paging
// parameters for ListAssets. Zero values are the API defaults.
type ListAssetsOptions struct {
	// Limit clamps to [1, 500]; 0 → 100.
	Limit int
	// Issuer, when non-empty, restricts to that G-strkey.
	Issuer string
	// Code, when non-empty, matches the classic asset code EXACTLY and case-sensitively (USDC and
	// usdc are distinct). Codes are not unique, so pair it with Issuer to pin one asset. Uses the
	// indexed classic_assets.code column.
	Code string
	// Type, when non-empty, narrows to one listing arm: "classic" or "soroban" (discovered
	// contracts, whose code / issuer / slug are NULL). Anything else is an ERROR from
	// ListAssetsExt, never an ignored filter: an ignored value would return the unfiltered page,
	// indistinguishable from a filtered one.
	Type string
	// Cursor is the keyset cursor returned by the previous
	// response's NextCursor field. Empty for the first page.
	Cursor string
	// Q, when non-empty, filters rows where code, slug, or
	// issuer_g_strkey contains the substring (case-insensitive).
	// Useful for the explorer's `/assets?q=…` search box —
	// otherwise a six-figure asset directory is unsearchable.
	Q string
	// Order controls the sort + cursor scheme. Zero value is
	// observation_count desc (preserves the historical contract).
	Order AssetsOrder
}

// ListAssets returns asset-directory rows by observation count desc, a cheap activity proxy. The
// keyset cursor `<observation_count>:<asset_id>` is the last row's tuple; empty means first page.
func (s *Store) ListAssets(ctx context.Context, limit int, issuer, cursor string) ([]AssetRow, error) {
	return s.ListAssetsExt(ctx, ListAssetsOptions{Limit: limit, Issuer: issuer, Cursor: cursor})
}

// SupplyObservation is one asset's most recent ADR-0011 supply observation. ObservedAt travels
// with the value because a supply figure without vintage is unfalsifiable, and the serving path
// decides whether to trust this arm over the lake's live flow sum by its age.
type SupplyObservation struct {
	// CirculatingSupply is the raw integer in the asset's smallest
	// unit (stroops for classic + native), as a decimal string —
	// NUMERIC, never a fixed-width Go type (ADR-0003).
	CirculatingSupply string
	// Basis is the ADR-0011 policy that produced the figure (`issuer_exclusion`, `override`, ...),
	// carried to the wire so listing and detail name the same basis.
	Basis string
	// ObservedAt is when the observer computed it.
	ObservedAt time.Time
}

// LatestSupplyObservations returns the most-recent supply observation per
// canonical asset_id, restricted to observations no older than maxAge.
// Keys are the listing's asset ids (CODE-ISSUER, `native`); coverage is the observer's watch-list.
//
// It reads the observation log, not the supply_1d CAGG: that daily roll-up's newest bucket is a
// completed previous day (~2.9h to ~26.9h old), which understates supply and would fail any 24h
// vintage bound. The log ticks every five minutes.
//
// maxAge must be positive; a non-positive value admits nothing, so the caller falls back to its
// next arm rather than publish an observation of unknown vintage.
func (s *Store) LatestSupplyObservations(ctx context.Context, maxAge time.Duration) (map[string]SupplyObservation, error) {
	// Postgres computes the cutoff, not the caller: rows carry the writer's clock, and a reader's
	// clock would turn skew into a phantom staleness verdict.
	//
	// $1 is cast and multiplied by an interval LITERAL: an untyped bind beside INTERVAL fails with
	// 42883 on every call.
	//
	// The bound is the ONLY predicate (no `OR unbounded` escape): a disjunction defeats plan-time
	// chunk exclusion and the read walks the whole hypertable at ~10x the cost.
	const q = `
        SELECT CASE WHEN asset_key = 'XLM' THEN 'native'
                    ELSE replace(asset_key, ':', '-') END AS asset_id,
               circulating_supply::text,
               basis,
               observed_at
          FROM (
            SELECT DISTINCT ON (asset_key)
                   asset_key,
                   circulating_supply,
                   basis,
                   "time" AS observed_at
              FROM asset_supply_history
             WHERE "time" >= now() - ($1::bigint * interval '1 second')
             ORDER BY asset_key, "time" DESC
          ) latest
    `
	rows, err := s.db.QueryContext(ctx, q, int64(maxAge/time.Second))
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestSupplyObservations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]SupplyObservation)
	for rows.Next() {
		var assetID string
		var obs SupplyObservation
		if err := rows.Scan(&assetID, &obs.CirculatingSupply, &obs.Basis, &obs.ObservedAt); err != nil {
			return nil, fmt.Errorf("timescale: LatestSupplyObservations scan: %w", err)
		}
		out[assetID] = obs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestSupplyObservations rows: %w", err)
	}
	return out, nil
}

// ListAssetsExt is ListAssets with the full options bag. ListAssets
// is kept as the 3-arg call so existing callers
// (handler, integration tests) compile unchanged; new callers
// pass ListAssetsOptions to opt into Q.
func (s *Store) ListAssetsExt(ctx context.Context, opts ListAssetsOptions) ([]AssetRow, error) {
	// Clamp to the page-size ceiling plus one row for the caller's overfetch-by-one sentinel (501,
	// not 500). Resetting to the default would truncate a 501-row overfetch to 100 and drop the cursor.
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 501:
		limit = 501
	}
	query, args, err := buildAssetsQuery(limit, opts.Issuer, opts.Code, opts.Cursor, opts.Q, opts.Type, opts.Order)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListAssets: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListAssets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AssetRow, 0, limit)
	for rows.Next() {
		r, err := scanAssetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListAssets rows: %w", err)
	}
	return out, nil
}

// scanAssetRow is the row projection shared by ListAssetsExt and GetAssetBySlug, which return the
// same column shape.
func scanAssetRow(scanner interface {
	Scan(dest ...any) error
},
) (AssetRow, error) {
	var r AssetRow
	var (
		firstLedger, lastLedger int64
		// slug, code and issuer_g_strkey are NULL for Soroban-native rows, yet AssetRow holds plain
		// strings; scanning NULL into them would fail the WHOLE request. Handled at the scan so one
		// fix covers any future nullable column.
		//
		// Empty string, NOT the contract id: a contract asset has no code or issuer, and the id
		// would state something false. (slug does fall back to asset_id: its real URL segment.)
		slug              sql.NullString
		code              sql.NullString
		issuerGStrkey     sql.NullString
		priceUSD          sql.NullString
		volume24hUSD      sql.NullString
		marketCapUSD      sql.NullString
		circulatingSupply sql.NullString
		change1hPct       sql.NullString
		change24hPct      sql.NullString
		change7dPct       sql.NullString
		sourceCount       sql.NullInt64
		volumeCharacter   sql.NullString
		sortVolume24hUSD  sql.NullString
		rankTier          sql.NullInt64
	)
	if err := scanner.Scan(
		&slug, &r.AssetID, &code, &issuerGStrkey,
		&firstLedger, &lastLedger, &r.ObservationCount,
		&priceUSD, &volume24hUSD, &marketCapUSD, &circulatingSupply,
		&change1hPct, &change24hPct, &change7dPct, &sourceCount,
		&volumeCharacter, &sortVolume24hUSD, &rankTier, &r.VolumeLowerBound,
	); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: scan asset: %w", err)
	}
	// slug carries a SQL COALESCE to asset_id; mirroring it here means a future query edit that
	// drops the COALESCE degrades to the contract id instead of failing the request.
	r.Slug = slug.String
	if r.Slug == "" {
		r.Slug = r.AssetID
	}
	r.Code = code.String                    // "" when NULL (Soroban-native)
	r.IssuerGStrkey = issuerGStrkey.String  // "" when NULL (Soroban-native)
	r.FirstSeenLedger = uint32(firstLedger) //nolint:gosec
	r.LastSeenLedger = uint32(lastLedger)   //nolint:gosec
	r.PriceUSD = nullStringPtr(priceUSD)
	r.Volume24hUSD = nullStringPtr(volume24hUSD)
	r.MarketCapUSD = nullStringPtr(marketCapUSD)
	r.CirculatingSupply = nullStringPtr(circulatingSupply)
	r.Change1hPct = nullStringPtr(change1hPct)
	r.Change24hPct = nullStringPtr(change24hPct)
	r.Change7dPct = nullStringPtr(change7dPct)
	if sourceCount.Valid {
		v := int(sourceCount.Int64)
		r.SourceCount = &v
	}
	r.VolumeCharacter = nullStringPtr(volumeCharacter)
	r.SortVolume24hUSD = nullStringPtr(sortVolume24hUSD)
	if rankTier.Valid {
		v := int(rankTier.Int64)
		r.RankTier = &v
	}
	return r, nil
}

func nullStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// adjustedVolume24hExpr is the concentration-adjusted 24h volume the default volume sort ranks
// on: raw volume (alias `vol`) times (1 - top_account_pair_vol_share) for assets that
// asset_volume_character (alias `avc`) labels `concentrated` or `operational`. Wash-traded assets
// sink while raw volume_24h_usd stays visible (demote, never hide or alter).
//
// It is a SORT KEY that must be identical in THREE places (the listing SELECT as sort_vol_usd,
// assetsOrderBy, assetsCursorPredicate), hence one const. It is all NUMERIC so the cursor
// round-trips exactly (::text out, ::numeric in).
const adjustedVolume24hExpr = `(COALESCE(vol.vol_usd, 0) * ` +
	`CASE WHEN avc.character IN ('concentrated', 'operational') ` +
	`THEN GREATEST(0::numeric, 1::numeric - avc.top_account_pair_vol_share::numeric) ` +
	`ELSE 1::numeric END)`

// listingPriceUSDExpr is the USD price the listing serves, before wire rounding. It is resolved
// once per aggregator pass into asset_price_snapshot and already decimals-normalised
// ([snapshotNormalizedPriceUSDExpr]), so it is never scaled again. The `aps` join carries the
// staleness ceiling, so "unpriced" and "too stale to serve" land in the same rank tier.
const listingPriceUSDExpr = `aps.price_usd`

// scamFlagTagsSQLArray inlines [DirectoryScamFlagTags] as a SQL array LITERAL, not a bind
// parameter, because the predicate is spliced at three different positional-argument offsets.
// The values are a compile-time vocabulary; mustSQLTextArrayLiteral panics at init outside [a-z].
var scamFlagTagsSQLArray = mustSQLTextArrayLiteral(DirectoryScamFlagTags)

// mustSQLTextArrayLiteral renders a lowercase-ASCII word list as a
// `ARRAY['a', 'b']::text[]` literal, panicking on any other character so
// a future tag can never smuggle a quote into composed SQL.
func mustSQLTextArrayLiteral(vals []string) string {
	if len(vals) == 0 {
		panic("timescale: refusing to build an empty SQL text-array literal")
	}
	quoted := make([]string, len(vals))
	for i, v := range vals {
		if v == "" {
			panic("timescale: SQL text-array literal: empty element")
		}
		for _, r := range v {
			if r < 'a' || r > 'z' {
				panic("timescale: SQL text-array literal accepts lowercase a-z only, got " + v)
			}
		}
		quoted[i] = "'" + v + "'"
	}
	return "ARRAY[" + strings.Join(quoted, ", ") + "]::text[]"
}

// directoryScamFlaggedExpr is TRUE when the row's ISSUER carries a scam-class directory tag. It
// is the SQL twin of pricingguard.IsDirectoryScamFlagged, lowercased on both sides, so a "Flagged"
// asset is exactly one this demotes. dir.tags is NULL for most rows; unnest(NULL) yields no
// rows, so EXISTS is false and the row ranks normally (fail-open).
var directoryScamFlaggedExpr = `EXISTS (SELECT 1 FROM unnest(dir.tags) t ` +
	`WHERE lower(t) = ANY(` + scamFlagTagsSQLArray + `))`

// rankTierMarker is replaced with [listingRankTierExpr] for the active order when the base
// SELECT renders: the SELECT is one const shared by both orders but the tier is not.
const rankTierMarker = "/*RANK_TIER*/"

// listingRankTierExpr is the PRIMARY ascending sort key of the /v1/assets listing, compared
// before the volume / observation-count key:
//
//	0 - rankable.
//	1 - unpriced (volume order only): a price/market-cap table must not rank an unpriced row above
//	    a priced one. The observation-count order ranks activity, so it keeps tiers {0, 2} only.
//	2 - directory-flagged (every order): the row stays but is never RANKED, since volume ranking
//	    would put a wash-traded token near the top.
//
// It is emitted as rank_tier so the keyset cursor encodes the value the ORDER BY ranks on (a
// cursor omitting the leading key skips or repeats rows), and repeated verbatim in assetsOrderBy
// and assetsCursorPredicate; keep the three in step as with adjustedVolume24hExpr.
func listingRankTierExpr(order AssetsOrder) string {
	if order == AssetsOrderVolume24hUSDDesc {
		return `(CASE WHEN ` + directoryScamFlaggedExpr + ` THEN 2 ` +
			`WHEN ` + listingPriceUSDExpr + ` IS NULL THEN 1 ELSE 0 END)`
	}
	return `(CASE WHEN ` + directoryScamFlaggedExpr + ` THEN 2 ELSE 0 END)`
}

// listAssetsBaseSelect is the SELECT shared by every WHERE permutation buildAssetsQuery composes.
//
// It reads NO hypertable and NO continuous aggregate: both money columns come from
// worker-maintained rollups keyed on asset_id (asset_volume_24h, asset_price_snapshot), so a page
// costs the spine plus three small hash joins whatever the limit / cursor / filter. Per-call
// `DISTINCT ON ... FROM prices_1m` CTEs cost seconds; add a rollup column instead.
//
// Volume is prices_1m.volume_usd over the trailing 24h where the asset is base OR quote
// (internal/aggregate/assetvolrollup). Price and change come from refreshAssetPriceSnapshotUpsert,
// whose staleness contract the join enforces.
//
// market_cap_usd and circulating_supply stay NULL: asset_supply_history does not cover the long
// tail of classic assets, and fabricating values would be a lie.
const listAssetsBaseSelect = `
		WITH catalogue_assets AS (
		  -- The listing spine. Was FROM classic_assets directly, which
		  -- silently narrowed this endpoint to CLASSIC assets only:
		  -- classic_assets requires a G-issuer (issuer_g_strkey NOT NULL),
		  -- so a Soroban-NATIVE contract asset can never have a row. The
		  -- documented contract for /v1/assets is "native XLM, classic
		  -- credits, Soroban tokens, and verified-catalogue currencies",
		  -- so the endpoint was promising a class of asset it structurally
		  -- could not return. Measured 2026-08-28 on r1: 0 Soroban-native
		  -- rows in the top-200-by-volume listing, including CAUP7 at
		  -- $71.8k/7d.
		  --
		  -- SAC-wrapped tokens are NOT excluded here and do not need to be:
		  -- v1.Server.foldAliasTwins already collapses each non-canonical
		  -- alias row onto its canonical twin, so AQUA/SHX/EURC cannot
		  -- appear twice.
		  SELECT asset_id, code, issuer_g_strkey, slug,
		         first_seen_ledger, last_seen_ledger, observation_count
		    FROM classic_assets
		  UNION ALL
		  -- Soroban-native contract assets. code/issuer/slug are NULL by
		  -- nature — a contract asset has no issuer account and no
		  -- SEP-1 code — and every downstream JOIN keys on asset_id only,
		  -- so the NULLs cost nothing.
		  --
		  -- Bounded by TRADED volume, not by discovery. discovered_assets
		  -- holds ~117k contracts because SEP-41 event discovery catches
		  -- anything emitting token events; an asset LISTING wants the
		  -- ones with a market. Measured on r1 2026-08-28:
		  --
		  --   all Soroban-native contracts   117,018
		  --   ...with an asset_volume_24h row     60
		  --   ...with positive volume             57
		  --
		  -- A recency filter (last_seen_at within 30 days) was tried first
		  -- and admitted 68,703 — a 35% increase in the spine, carried by
		  -- every LEFT JOIN on a latency-sensitive endpoint (see the #43
		  -- 2026-07-06 incident referenced above). Keying on the volume
		  -- rollup the listing ALREADY joins adds ~60 rows instead.
		  SELECT d.contract_id AS asset_id,
		         NULL::text    AS code,
		         NULL::text    AS issuer_g_strkey,
		         NULL::text    AS slug,
		         d.first_seen_ledger,
		         d.last_seen_ledger,
		         d.event_count  AS observation_count
		    FROM discovered_assets d
		   WHERE EXISTS (SELECT 1 FROM asset_volume_24h v WHERE v.asset_id = d.contract_id)
		     AND NOT EXISTS (SELECT 1 FROM classic_assets c WHERE c.asset_id = d.contract_id)
		),
		per_asset_24h_vol AS (
		  -- e0fbbbc3b (2026-07-06 latency incident): read the trailing-24h
		  -- per-asset USD volume from the asset_volume_24h rollup
		  -- (migration 0087) instead of re-summing prices_1m per
		  -- request. The aggregator's assetvolrollup worker runs the
		  -- exact base-OR-quote SUM this CTE used to inline; the listing
		  -- now LEFT JOINs a small keyed-on-PK table (one row per asset
		  -- with 24h volume), so the UNFILTERED /v1/assets page stops
		  -- paying the ~4.8s cold ~256k-row scan the incident measured.
		  -- vol_usd stays NUMERIC → the wire string is byte-identical to
		  -- the old inline SUM (the rollup moved the compute, not the
		  -- value). No asset-filter pushdown here: reading the rollup is
		  -- cheap regardless of the caller's asset filter.
		  SELECT asset_id, vol_usd, unpriced_trades
		    FROM asset_volume_24h
		)
		SELECT
		    -- asset_id is the LAST resort here, and it is load-bearing.
		    --
		    -- A Soroban-native row from catalogue_assets has slug AND
		    -- code both NULL (a contract asset has no issuer account and
		    -- no SEP-1 code), so the two-arm COALESCE this used to be
		    -- returned NULL — and the slug column scans into a
		    -- non-nullable Go string. Measured in production 2026-08-28:
		    -- GET /v1/assets with limit=500 returned HTTP 500,
		    -- "converting NULL to string is unsupported", which also
		    -- hard-failed the
		    -- explorer build (it refuses to bake fallback HTML rather
		    -- than ship a page built on a broken fetch). limit=100
		    -- survived only because the ordering had not yet reached one
		    -- of the 60 Soroban rows — the bug was live at limit>=150.
		    --
		    -- The contract id is the RIGHT fallback, not merely a
		    -- non-null one: it is already this asset's URL segment
		    -- (/assets/CAUP7NFA… resolves today), so the slug a caller
		    -- gets back is the slug that actually works.
		    COALESCE(ca.slug, ca.code, ca.asset_id) AS slug,
		    ca.asset_id,
		    ca.code,
		    ca.issuer_g_strkey,
		    ca.first_seen_ledger,
		    ca.last_seen_ledger,
		    ca.observation_count,
		    -- Round to 10 dp on the wire — NUMERIC * NUMERIC
		    -- preserves full precision (36+ digits) which is just
		    -- noise for a display value. 10 dp covers sub-millicent
		    -- precision (1e-10) which is finer than any asset's
		    -- meaningful tick size. asset_price_snapshot stores the
		    -- unrounded NUMERIC (snapshotPriceUSDExpr); for a CONFIRMED
		    -- non-7-decimals token that is the decimals-CORRECTED price
		    -- (snapshotNormalizedPriceUSDExpr), so this ROUND runs after
		    -- the correction and no reader may normalise it again.
		    ROUND(` + listingPriceUSDExpr + `, 10)::text  AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    -- 1h / 24h / 7d change, rendered from the rollup's stored
		    -- NUMERIC ratio with the SAME to_char format the inline
		    -- three-arm CASE used (native-XLM arm, direct-USD arm,
		    -- XLM-triangulated arm — all three now resolved once per
		    -- refresh pass in refreshAssetPriceSnapshotUpsert). NULL in,
		    -- NULL out: an asset with no comparable lookback renders no
		    -- change, exactly as before.
		    to_char(aps.change_1h_pct,  'FM999999990.00') AS change_1h_pct,
		    to_char(aps.change_24h_pct, 'FM999999990.00') AS change_24h_pct,
		    to_char(aps.change_7d_pct,  'FM999999990.00') AS change_7d_pct,
		    -- Distinct venues backing price_usd (the latest per-asset bucket's
		    -- prices_1m.sources) — the liquidity signal the API's market-cap
		    -- valuation guard reads. NULL for native XLM (triangulated price,
		    -- always liquid) and for any asset with no per-asset bucket.
		    aps.source_count                      AS source_count,
		    avc.character                         AS volume_character,
		    ` + adjustedVolume24hExpr + ` AS sort_vol_usd,
		    -- Leading (ascending) sort key — see listingRankTierExpr. The
		    -- marker is substituted per-order at render time; it is emitted
		    -- as a column so the keyset cursor can encode the same value
		    -- the ORDER BY ranks on.
		    ` + rankTierMarker + ` AS rank_tier,
		    COALESCE(vol.unpriced_trades, 0) > 0  AS volume_lower_bound
		  FROM catalogue_assets ca
		  LEFT JOIN per_asset_24h_vol vol         ON vol.asset_id        = ca.asset_id
		  -- Headline price + 1h/24h/7d change + backing source count
		  -- (migration 0154, #331 F1). Replaces the twelve
		  -- DISTINCT-ON-over-prices_1m CTEs this query used to
		  -- materialise for every asset on every uncached variant
		  -- (1,353ms of a measured 1,830ms, 51MB of temp spill); the
		  -- derivation itself is unchanged and now runs once per
		  -- aggregator pass in refreshAssetPriceSnapshotUpsert.
		  --
		  -- The computed_at floor is the STALENESS CEILING, enforced in
		  -- SQL rather than by convention: a wedged or dead aggregator
		  -- makes the join MISS instead of serving an indefinitely-old
		  -- price, and the row then renders exactly as an asset with no
		  -- price does (price_usd absent, rank tier 1). Fail-closed —
		  -- see assetPriceSnapshotMaxAge for the contract and the
		  -- reasoning about /v1/price.
		  LEFT JOIN asset_price_snapshot   aps    ON aps.asset_id        = ca.asset_id
		                                        AND aps.computed_at > now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'
		  LEFT JOIN asset_volume_character avc    ON avc.asset_id        = ca.asset_id
		  -- Curated third-party issuer labels (account_directory, migration
		  -- 0136). Read ONLY by listingRankTierExpr's scam-flag demotion —
		  -- the payload's issuer_directory_* fields are still stamped by the
		  -- API layer's batch lookup, so this join changes ranking, never
		  -- served data. Keyed exactly like v1.Server.fillIssuerDirectoryTags
		  -- (the issuer G-address, or a Soroban-native row's own contract
		  -- address), so "demoted" and "shows the ⚠ Flagged pill" are the
		  -- same set of rows. account_directory.address is the PRIMARY KEY,
		  -- so this join can never fan a listing row out into duplicates.
		  LEFT JOIN account_directory      dir    ON dir.address         = COALESCE(ca.issuer_g_strkey, ca.asset_id)
`

// listAssetsBaseSelectSQL renders [listAssetsBaseSelect] for the active
// order, substituting the one /*RANK_TIER*/ marker.
//
// There is no pushdown predicate: the price CTEs live in refreshAssetPriceSnapshotUpsert, so a
// filtered listing is narrowed by the outer WHERE on `ca` alone, over keyed-on-PK price lookups.
func listAssetsBaseSelectSQL(order AssetsOrder) string {
	// Substitute unconditionally: leaving the marker would ship `/*RANK_TIER*/ AS rank_tier`, a
	// syntax error. Exactly one occurrence is expected.
	base := strings.Replace(listAssetsBaseSelect, rankTierMarker, listingRankTierExpr(order), 1)
	if strings.Contains(base, rankTierMarker) {
		panic("listAssetsBaseSelectSQL: more than one " + rankTierMarker + " marker in the base SELECT")
	}
	return base
}

// refreshAssetVolumeUpsert recomputes the trailing-24h per-asset USD volume (asset as base OR
// quote) into asset_volume_24h as NUMERIC, never float (ADR-0003). computed_at is the transaction
// timestamp so the sibling prune drops assets whose volume lapsed this pass.
//
// prices_1m.volume_usd is sum(coalesce(usd_volume, 0)), so an unvalued trade adds 0; unpriced_trades
// counts them so readers flag vol_usd as a lower bound. The count is LEFT JOINed and never creates
// a row (the Soroban spine admits a contract on an asset_volume_24h row). Its window is a superset
// of the closed-bucket one: it can over-flag at the edges, never under-flag.
const refreshAssetVolumeUpsert = `
WITH vol AS (
  SELECT asset_id, SUM(volume_usd) AS vol_usd
    FROM (
      SELECT base_asset  AS asset_id, volume_usd
        FROM prices_1m
       WHERE bucket >= now() - INTERVAL '24 hours'
         AND bucket <= now() - INTERVAL '1 minute'
         AND volume_usd IS NOT NULL
      UNION ALL
      SELECT quote_asset AS asset_id, volume_usd
        FROM prices_1m
       WHERE bucket >= now() - INTERVAL '24 hours'
         AND bucket <= now() - INTERVAL '1 minute'
         AND volume_usd IS NOT NULL
    ) t
   GROUP BY asset_id
),
unpriced AS (
  SELECT leg.asset_id, count(*) AS n
    FROM trades tr
   CROSS JOIN LATERAL (VALUES (tr.base_asset), (tr.quote_asset)) AS leg(asset_id)
   WHERE tr.ts >= now() - INTERVAL '24 hours'
     AND tr.ts < now()
     AND tr.usd_volume IS NULL
   GROUP BY leg.asset_id
)
INSERT INTO asset_volume_24h AS t (asset_id, vol_usd, unpriced_trades, computed_at)
SELECT vol.asset_id, vol.vol_usd, COALESCE(unpriced.n, 0), now()
  FROM vol
  LEFT JOIN unpriced ON unpriced.asset_id = vol.asset_id
ON CONFLICT (asset_id) DO UPDATE
   SET vol_usd         = EXCLUDED.vol_usd,
       unpriced_trades = EXCLUDED.unpriced_trades,
       computed_at     = EXCLUDED.computed_at`

// refreshAssetVolumePrune deletes assets that fell out of the 24h window. Within one transaction
// now() is fixed, so rows just upserted carry computed_at = now() and survive while stale rows
// keep an older timestamp and are dropped.
const refreshAssetVolumePrune = `DELETE FROM asset_volume_24h WHERE computed_at < now()`

// refreshAssetVolumePruneExpired is the prune a pass whose upsert wrote no
// rows runs instead (see [pruneRollup]): it keeps last-good rows but never
// one older than the 24h window it summed, which is false by construction.
const refreshAssetVolumePruneExpired = `DELETE FROM asset_volume_24h WHERE computed_at < now() - INTERVAL '24 hours'`

// RefreshAssetVolume24h is the aggregator's entry point into the /v1/assets rollup refresh. It
// delegates to [Store.RefreshAssetListingRollups], which refreshes asset_volume_24h AND
// asset_price_snapshot in one transaction. The name is narrower than the behaviour because it is
// the method assetvolrollup.Refresher names.
func (s *Store) RefreshAssetVolume24h(ctx context.Context) error {
	return s.RefreshAssetListingRollups(ctx)
}

// buildAssetsQuery composes the WHERE + ORDER + LIMIT around listAssetsBaseSelectSQL from a slice
// of conditions and numbered placeholders. It errors on a `typ` the spine cannot express (see
// [assetsTypeCondition]); a query is never composed from a filter it did not understand.
func buildAssetsQuery(limit int, issuer, code, cursor, q, typ string, order AssetsOrder) (string, []any, error) {
	var (
		conds []string
		args  []any
	)
	if issuer != "" {
		args = append(args, issuer)
		conds = append(conds, fmt.Sprintf("ca.issuer_g_strkey = $%d", len(args)))
	}
	if code != "" {
		// Exact, case-sensitive match on the indexed classic_assets.code (classic_assets_code_idx).
		// A bare code is not unique (many issuers mint "USDC"); with issuer it pins one asset.
		args = append(args, code)
		conds = append(conds, fmt.Sprintf("ca.code = $%d", len(args)))
	}
	typeCond, err := assetsTypeCondition(typ)
	if err != nil {
		return "", nil, err
	}
	if typeCond != "" {
		conds = append(conds, typeCond)
	}
	if q != "" {
		// COALESCE(ca.slug, ca.code, ca.asset_id) mirrors the base SELECT's slug column: a
		// Soroban-native row has NULL code/slug/issuer, so without the asset_id fallback the
		// predicate is NULL and type=soroban&q=... matches nothing.
		args = append(args, "%"+escapeLikePattern(q)+"%")
		conds = append(conds, fmt.Sprintf(
			"(LOWER(ca.code) LIKE LOWER($%d) ESCAPE '\\' OR LOWER(COALESCE(ca.slug, ca.code, ca.asset_id)) LIKE LOWER($%d) ESCAPE '\\' OR LOWER(ca.issuer_g_strkey) LIKE LOWER($%d) ESCAPE '\\')",
			len(args), len(args), len(args)))
	}
	args = append(args, assetsCursorArgs(cursor, order)...)
	if cursor != "" {
		conds = append(conds, assetsCursorPredicate(order, len(args)))
	}
	args = append(args, limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	return listAssetsBaseSelectSQL(order) + where + assetsOrderBy(order) + " LIMIT " + limitPlaceholder, args, nil
}

// escapeLikePattern escapes the backslash and the LIKE wildcards `%` and `_` so a caller's `q`
// matches as literal text. Every LIKE it feeds needs an explicit `ESCAPE '\'` clause.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// assetsTypeCondition maps the `type` filter onto what separates the spine's two arms:
// classic_assets has a non-NULL issuer_g_strkey, the discovered-contract arm selects NULL. Empty
// means no predicate. The value is a closed enum, so it is matched, never interpolated.
//
// Any OTHER value is an error: skipping it would serve the unfiltered page under a 200. Callers
// fold `native` / `fiat` to an empty page first, as the spine holds no such row.
func assetsTypeCondition(typ string) (string, error) {
	switch typ {
	case "":
		return "", nil
	case "classic":
		return "ca.issuer_g_strkey IS NOT NULL", nil
	case "soroban":
		return "ca.issuer_g_strkey IS NULL", nil
	default:
		return "", fmt.Errorf("unsupported type filter %q (want \"classic\", \"soroban\" or empty)", typ)
	}
}

// assetsCursorArgs returns the positional args appended for the
// active cursor format: (rank_tier, sort_key, asset_id) — the full
// ORDER BY tuple, leading key first. Empty cursor → no args.
func assetsCursorArgs(cursor string, order AssetsOrder) []any {
	if cursor == "" {
		return nil
	}
	if order == AssetsOrderVolume24hUSDDesc {
		tier, vol, assetID := parseVolumeCursor(cursor)
		return []any{tier, vol, assetID}
	}
	tier, obsCount, assetID := parseAssetCursor(cursor)
	return []any{tier, obsCount, assetID}
}

// assetsCursorPredicate returns the WHERE clause resuming strictly past the cursor. `argEnd` is
// the last cursor placeholder (asset_id); the sort key is at argEnd-1 and the rank tier at
// argEnd-2.
//
// The tuple is (rank_tier ASC, sort_key, asset_id ASC) and the predicate mirrors it LEADING KEY
// FIRST: `t > ct OR (t = ct AND <rest>)`. Without the tier in the WHERE, page 2 would resume at
// the cursor's volume inside tier 0 and skip every higher-volume tier-1/2 row.
func assetsCursorPredicate(order AssetsOrder, argEnd int) string {
	tier := listingRankTierExpr(order)
	if order == AssetsOrderVolume24hUSDDesc {
		// Mixed-direction compare: rank_tier ASC, adjusted volume DESC, asset_id ASC. The cursor
		// encodes the same adjustedVolume24hExpr the ORDER BY ranks on (emitted as sort_vol_usd),
		// which COALESCEs raw volume to 0 so a NULL-volume asset sorts last.
		return fmt.Sprintf(
			"(%s > $%d::int OR (%s = $%d::int AND "+
				"(%s < $%d::numeric OR (%s = $%d::numeric AND ca.asset_id > $%d))))",
			tier, argEnd-2, tier, argEnd-2,
			adjustedVolume24hExpr, argEnd-1, adjustedVolume24hExpr, argEnd-1, argEnd)
	}
	// Spelled out like the volume arm: observation_count ranks DESC but asset_id ASC, and a SQL
	// row-constructor compare is same-direction on every element, so it would test asset_id < $m
	// on a tie. Ties are the norm in the long tail, so rows would repeat or be skipped.
	return fmt.Sprintf(
		"(%s > $%d::int OR (%s = $%d::int AND "+
			"(ca.observation_count < $%d OR "+
			"(ca.observation_count = $%d AND ca.asset_id > $%d))))",
		tier, argEnd-2, tier, argEnd-2, argEnd-1, argEnd-1, argEnd)
}

func assetsOrderBy(order AssetsOrder) string {
	// rank_tier leads every order: a directory-flagged issuer's
	// asset sorts below every unflagged one whatever the active sort key.
	orderBy := " ORDER BY " + listingRankTierExpr(order) + " ASC, "
	if order == AssetsOrderVolume24hUSDDesc {
		// §4-B "annotate + demote": within a tier, rank by the
		// concentration-adjusted volume, not raw, so wash/operational assets
		// don't sit atop the directory. The raw volume_24h_usd stays the
		// visible payload value.
		return orderBy + adjustedVolume24hExpr + " DESC, ca.asset_id ASC"
	}
	return orderBy + "ca.observation_count DESC, ca.asset_id ASC"
}

// splitAssetsCursor splits a listing cursor into rank tier, sort-key prefix and asset_id. It
// accepts "<tier>:<sort_key>:<asset_id>" (the tier leads as the leading ORDER BY key) and the
// two-field "<sort_key>:<asset_id>", read as tier 0 so an in-flight cursor resumes instead of
// 400ing.
//
// asset_id carries no ':' (CODE-Gxxxx, bare C-strkeys, "native"), so counting separators is
// unambiguous. ok=false means no ':' at all.
func splitAssetsCursor(cursor string) (tier, sortKey, assetID string, ok bool) {
	first := strings.IndexByte(cursor, ':')
	if first < 0 {
		return "", "", "", false
	}
	rest := cursor[first+1:]
	second := strings.IndexByte(rest, ':')
	if second < 0 {
		return "0", cursor[:first], rest, true
	}
	return cursor[:first], rest[:second], rest[second+1:], true
}

// parseVolumeCursor decodes a `<tier>:<vol_or_blank>:<asset_id>` cursor.
// Empty volume sorts as 0 (joins the null-volume tail); an unparseable
// tier falls back to 0. Malformed cursors are the handler's job to
// reject via [ValidateAssetsCursor] before this is reached.
func parseVolumeCursor(cursor string) (tier int, vol, assetID string) {
	tierStr, vol, assetID, ok := splitAssetsCursor(cursor)
	if !ok {
		return 0, "0", ""
	}
	if vol == "" {
		vol = "0"
	}
	return atoiOrZero(tierStr), vol, assetID
}

// atoiOrZero parses a non-negative decimal string, returning 0 for
// anything else (empty, signed, overflowing, non-digit).
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// AssetPricePoint is one USD-price sample: `T` the bucket end (RFC 3339), `P` the price rounded
// to 10 dp on the same path as price_usd. P is a pointer so a bucket with no trades is null, not zero.
type AssetPricePoint struct {
	T string
	P *string
}

// Rounding scale for catalogue reads that serve a USD price as ROUNDed text from RAW prices_1m
// ratios: the per-asset price_usd ([getAssetBySlugSQL]) and the price-history series.
//
// These reads stay RAW because the API corrects a confirmed non-7-decimals token
// (v1.Server.normalizeCatalogueUSD); correcting here too would apply the factor twice. Rounding
// runs before that correction: a flat ROUND(raw, 10) on an 18-decimals token would turn a 1 USD
// price (raw 1e-11) into zero. Rounding the raw ratio to 10 + k places, where the correction is
// 10^k, IS rounding the corrected price:
//
//	ROUND(raw, 10 + k) * 10^k  ==  ROUND(raw * 10^k, 10)
//
// k comes from nonstandard_decimals_assets, floored at zero; the API reads it back from the text,
// as ROUND(x, n)::text has exactly n fraction places.
//
// Three spellings of one expression differ only in how the asset is named. MAX because the
// alias-array form may match more than one spelling.
const (
	catalogueRoundPlacesHead = `10 + COALESCE((SELECT MAX(GREATEST(nda.decimals - 7, 0))
		                          FROM nonstandard_decimals_assets nda
		                         WHERE nda.asset = `
	catalogueRoundPlacesTail = `), 0)`

	// $1 is the asset's alias array (single-asset history reads).
	catalogueRoundPlacesForAliases = catalogueRoundPlacesHead + `ANY($1)` + catalogueRoundPlacesTail
	// `ca` is the chosen catalogue row (GetAssetBySlug).
	catalogueRoundPlacesForRow = catalogueRoundPlacesHead + `ca.asset_id` + catalogueRoundPlacesTail
	// `w` is the wanted-ids row of a batch history read; forms are its aliases.
	catalogueRoundPlacesForWanted = catalogueRoundPlacesHead + `ANY(w.forms)` + catalogueRoundPlacesTail
)

// GetAssetPriceHistory24h returns up to 24 hourly USD price samples, oldest first, using the
// same direct-then-XLM-triangulated path as price_usd. Buckets with no trades give a null P, left
// to the UI to gap or interpolate.
func (s *Store) GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	rows, err := s.db.QueryContext(ctx, getAssetPriceHistory24hSQL, assetAliasArray(assetID), canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetPricePoint, 0, 24)
	for rows.Next() {
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h rows: %w", err)
	}
	return out, nil
}

// getAssetPriceHistory24hSQL is GetAssetPriceHistory24h's query, a constant to keep funlen happy.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var getAssetPriceHistory24hSQL = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		direct_per_hour AS (
		  -- Alias-complete + priority-preserving: pick, per hour, the vwap
		  -- of the HIGHEST-priority alias form (array_position; SAC last)
		  -- and within that form the latest bucket, so a thin SAC pool
		  -- never outranks the deep classic/native form.
		  -- Quote set = fiat:USD OR USDC (classic or its SAC form)
		  -- per the stablecoin-proxy policy (see the listing
		  -- query's direct_usd CTE): a USDC-quoted vwap is taken
		  -- AS the USD price.
		  SELECT h, vwap FROM (
		    SELECT date_trunc('hour', bucket) AS h, vwap::numeric AS vwap,
		           row_number() OVER (
		             PARTITION BY date_trunc('hour', bucket)
		             ORDER BY array_position($1::text[], base_asset), bucket DESC,
		                      ` + usdQuotePref + `
		           ) AS rn
		      FROM prices_1m
		     WHERE base_asset = ANY($1)
		       AND quote_asset IN (
		         'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		         'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		         'fiat:USD'
		       )
		       AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		       AND vwap IS NOT NULL
		  ) z WHERE rn = 1
		),
		asset_xlm_per_hour AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and in BOTH
		  -- stored DIRECTIONS: the base-side arm (asset, XLM) UNION an
		  -- inverted arm (XLM, asset) at 1/vwap, for markets the
		  -- aquarius decoder stores with the XLM SAC as BASE — see the
		  -- listing query's asset_vs_xlm CTE. Without the inverted arm
		  -- such an asset had a headline price (#254) but an EMPTY
		  -- sparkline. Per hour, base-side rows are preferred over
		  -- inverted ones (ORDER BY inverted FIRST), then the same
		  -- alias-priority + latest-bucket pick as before, so a hour
		  -- that already had a base-side row keeps a byte-identical
		  -- point; the inverted arm only fills hours with none.
		  SELECT h, vwap FROM (
		    SELECT h, vwap,
		           row_number() OVER (
		             PARTITION BY h
		             ORDER BY inverted, prio, bucket DESC, xlm_prio
		           ) AS rn
		      FROM (
		        SELECT date_trunc('hour', bucket) AS h, vwap::numeric AS vwap,
		               array_position($1::text[], base_asset) AS prio,
		               bucket, 0 AS inverted, ` + xlmFormPrefOpenBound2 + `quote_asset) AS xlm_prio
		          FROM prices_1m
		         WHERE base_asset = ANY($1)
		           AND quote_asset IN ('native', $2::text)
		           AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		           AND vwap IS NOT NULL
		        UNION ALL
		        SELECT date_trunc('hour', bucket), 1::numeric / vwap,
		               array_position($1::text[], quote_asset),
		               bucket, 1, ` + xlmFormPrefOpenBound2 + `base_asset)
		          FROM prices_1m
		         WHERE base_asset IN ('native', $2::text)
		           AND quote_asset = ANY($1)
		           AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		           AND vwap > 0
		      ) u
		  ) z WHERE rn = 1
		),
		` + xlmUSDAnchorPerPeriodCTE("xlm_usd_per_hour", "h", "hour",
	`date_trunc('hour', now() - INTERVAL '23 hours')`, "$2::text") + `
		SELECT
		    to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      -- XLM itself: use the xlm_usd_per_hour CTE directly.
		      -- Without this, XLM's sparkline is all nulls because
		      -- direct_per_hour and asset_xlm_per_hour filter on the
		      -- asset's own forms (native / crypto:XLM / SAC) with
		      -- quote_asset 'fiat:USD' / 'native' — neither has rows.
		      CASE WHEN 'native' = ANY($1) THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), ` + catalogueRoundPlacesForAliases + `)::text AS p
		  FROM hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY hours.bucket ASC
`

// GetAssetPriceHistory7d returns up to 7 daily USD price samples, oldest first, on the same path
// as GetAssetPriceHistory24h. Buckets with no trades give a null P.
func (s *Store) GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	rows, err := s.db.QueryContext(ctx, getAssetPriceHistory7dSQL, assetAliasArray(assetID), canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetPricePoint, 0, 7)
	for rows.Next() {
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d rows: %w", err)
	}
	return out, nil
}

// getAssetPriceHistory7dSQL is GetAssetPriceHistory7d's query, a constant so
// TestProxyQuoteLists_Lockstep can pin its XLM arms beside getAssetPriceHistory24hSQL.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var getAssetPriceHistory7dSQL = `
		WITH days AS (
		  SELECT generate_series(
		    date_trunc('day', now() - INTERVAL '6 days'),
		    date_trunc('day', now()),
		    INTERVAL '1 day'
		  ) AS bucket
		),
		direct_per_day AS (
		  -- Alias-complete + priority-preserving pick (SAC last); see
		  -- GetAssetPriceHistory24h.direct_per_hour for the rationale.
		  -- Quote set = fiat:USD OR USDC (classic or its SAC form)
		  -- per the stablecoin-proxy policy (see the listing
		  -- query's direct_usd CTE).
		  SELECT d, vwap FROM (
		    SELECT date_trunc('day', bucket) AS d, vwap::numeric AS vwap,
		           row_number() OVER (
		             PARTITION BY date_trunc('day', bucket)
		             ORDER BY array_position($1::text[], base_asset), bucket DESC,
		                      ` + usdQuotePref + `
		           ) AS rn
		      FROM prices_1m
		     WHERE base_asset = ANY($1)
		       AND quote_asset IN (
		         'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		         'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		         'fiat:USD'
		       )
		       AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		       AND vwap IS NOT NULL
		  ) z WHERE rn = 1
		),
		asset_xlm_per_day AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and in BOTH
		  -- stored DIRECTIONS: the base-side arm (asset, XLM) UNION an
		  -- inverted arm (XLM, asset) at 1/vwap, for markets the
		  -- aquarius decoder stores with the XLM SAC as BASE — see the
		  -- listing query's asset_vs_xlm CTE. Without the inverted arm
		  -- such an asset had a headline price (#254) but an EMPTY
		  -- sparkline. Per day, base-side rows are preferred over
		  -- inverted ones (ORDER BY inverted FIRST), then the same
		  -- alias-priority + latest-bucket pick as before, so a day
		  -- that already had a base-side row keeps a byte-identical
		  -- point; the inverted arm only fills days with none.
		  SELECT d, vwap FROM (
		    SELECT d, vwap,
		           row_number() OVER (
		             PARTITION BY d
		             ORDER BY inverted, prio, bucket DESC, xlm_prio
		           ) AS rn
		      FROM (
		        SELECT date_trunc('day', bucket) AS d, vwap::numeric AS vwap,
		               array_position($1::text[], base_asset) AS prio,
		               bucket, 0 AS inverted, ` + xlmFormPrefOpenBound2 + `quote_asset) AS xlm_prio
		          FROM prices_1m
		         WHERE base_asset = ANY($1)
		           AND quote_asset IN ('native', $2::text)
		           AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		           AND vwap IS NOT NULL
		        UNION ALL
		        SELECT date_trunc('day', bucket), 1::numeric / vwap,
		               array_position($1::text[], quote_asset),
		               bucket, 1, ` + xlmFormPrefOpenBound2 + `base_asset)
		          FROM prices_1m
		         WHERE base_asset IN ('native', $2::text)
		           AND quote_asset = ANY($1)
		           AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		           AND vwap > 0
		      ) u
		  ) z WHERE rn = 1
		),
		` + xlmUSDAnchorPerPeriodCTE("xlm_usd_per_day", "d", "day",
	`date_trunc('day', now() - INTERVAL '6 days')`, "$2::text") + `
		SELECT
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN 'native' = ANY($1) THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), ` + catalogueRoundPlacesForAliases + `)::text AS p
		  FROM days
		  LEFT JOIN direct_per_day      d  ON d.d  = days.bucket
		  LEFT JOIN asset_xlm_per_day   x  ON x.d  = days.bucket
		  LEFT JOIN xlm_usd_per_day     xu ON xu.d = days.bucket
		 ORDER BY days.bucket ASC
`

// AssetATH is the asset's all-time-high USD price and its day, across USD-quoted day-buckets in
// `prices_1d` (direct USD-stablecoin pairs and `fiat:USD`). Triangulated asset/XLM x XLM/USD
// paths are excluded: one bad XLM/USD reading on a thin day could fabricate an ATH.
//
// The metric is the highest day-VWAP, not the highest tick, so volume weighting rejects dust prints.
// The `USDT-GCQTGZQQ...` issuer is EXCLUDED from every USD allowlist (that asset trades unpegged).
// USD proxies are [usdProxyQuotes]; new ones need a verified-catalogue entry.
//
// A day-bucket counts only if its pair cleared [athMinDayVolumeUSD] and [athMinDayTrades], so a
// lone print cannot set the high. The floor is a display filter, not attack-resistant.
type AssetATH struct {
	USD string // numeric, fixed-point string (preserves precision)
	At  string // RFC-3339 day-bucket the high was set
}

// The per-(pair, day) substance floor a prices_1d row must clear to set an ATH.
const (
	athMinDayVolumeUSD = 100
	athMinDayTrades    = 3
)

// GetAssetATH returns the asset's all-time-high USD price: the (vwap, bucket_day) with maximal
// vwap in `prices_1d` over [usdProxyQuotes] (canonical USDC and its SAC, plus `fiat:USD`).
// Native XLM is the BASE of every USD pair, so no special case is needed. Returns (nil, nil) when
// no USD-quoted day cleared the substance floor.
func (s *Store) GetAssetATH(ctx context.Context, assetID string) (*AssetATH, error) {
	// Alias-complete: max USD day-VWAP across EVERY canonical form; volume weighting keeps a thin
	// SAC pool's dust from fabricating a high.
	const q = `
		SELECT
		    vwap::text,
		    to_char(bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d
		 WHERE base_asset = ANY($1)
		   AND quote_asset IN (` + usdProxyQuotes + `)
		   AND vwap IS NOT NULL
		   AND volume_usd >= $2::numeric
		   AND trade_count >= $3::bigint
		 ORDER BY vwap DESC, bucket
		 LIMIT 1
	`
	var ath AssetATH
	row := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID), athMinDayVolumeUSD, athMinDayTrades)
	switch err := row.Scan(&ath.USD, &ath.At); {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("timescale: GetAssetATH: %w", err)
	}
	return &ath, nil
}

// GetAssetsATHBatch returns the ATH USD price and day per asset_id in one round trip, with
// GetAssetATH's semantics per id (all alias forms, same allowlist and floor, earliest day on a
// tie). Empty input gives an empty map; ids with no USD-quoted history are absent.
func (s *Store) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]AssetATH, error) {
	out := make(map[string]AssetATH, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	const q = `
		WITH want AS (
		  SELECT * FROM unnest($1::text[], $2::text[]) AS w(form, asset_id)
		)
		SELECT DISTINCT ON (w.asset_id)
		    w.asset_id,
		    p.vwap::text,
		    to_char(p.bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d p
		  JOIN want w ON w.form = p.base_asset
		 WHERE p.base_asset = ANY($1)
		   AND p.quote_asset IN (` + usdProxyQuotes + `)
		   AND p.vwap IS NOT NULL
		   AND p.volume_usd >= $3::numeric
		   AND p.trade_count >= $4::bigint
		 ORDER BY w.asset_id, p.vwap DESC, p.bucket
	`
	forms, owners, _ := assetAliasRows(assetIDs)
	rows, err := s.db.QueryContext(ctx, q, forms, owners, athMinDayVolumeUSD, athMinDayTrades)
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsATHBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var assetID string
		var ath AssetATH
		if err := rows.Scan(&assetID, &ath.USD, &ath.At); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsATHBatch scan: %w", err)
		}
		out[assetID] = ath
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsATHBatch rows: %w", err)
	}
	return out, nil
}

// AssetTopMarket is one entry in the top-markets preview returned
// alongside a single asset lookup. Compact summary suitable for an
// asset detail page header — the full markets list lives on
// /v1/markets.
type AssetTopMarket struct {
	Counterparty  string  // the OTHER side of the pair (the side that's NOT this asset)
	Side          string  // "base" if this asset was base, else "quote"
	Volume24hUSD  *string // trailing-24h USD volume for this pair; nil if no USD-equivalent trades
	TradeCount24h int64
}

// GetAssetMarketsCount returns the number of distinct markets the asset traded in over the
// trailing 24h (non-null prices_1m bucket). A market is its canonical orientation, as on
// /v1/markets, so one stored in both directions counts once. Returns 0 with no rows.
func (s *Store) GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, assetMarketsCountQuery(), assetAliasArray(assetID), canonical.NativeSACContractID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: GetAssetMarketsCount: %w", err)
	}
	return n, nil
}

// GetAssetTradeCount24h returns the trades the asset took part in (base OR quote) over the
// trailing 24h. It reads the `trades` hypertable directly, so the count is per trade rather than
// per prices_1m bucket. Returns 0 with no trades.
func (s *Store) GetAssetTradeCount24h(ctx context.Context, assetID string) (int64, error) {
	const q = `
		SELECT COUNT(*)
		  FROM trades
		 WHERE ts >= now() - INTERVAL '24 hours'
		   AND (base_asset = ANY($1) OR quote_asset = ANY($1))
	`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID)).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: GetAssetTradeCount24h: %w", err)
	}
	return n, nil
}

// GetAssetTopMarkets returns up to `limit` markets the asset trades in (base OR quote), by
// trailing-24h USD volume desc. limit clamps to [1, 20]; default 5.
func (s *Store) GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]AssetTopMarket, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx, assetTopMarketsQuery(), assetAliasArray(assetID), limit, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetTopMarkets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetTopMarket, 0, limit)
	for rows.Next() {
		var m AssetTopMarket
		var vol sql.NullString
		if err := rows.Scan(&m.Counterparty, &m.Side, &vol, &m.TradeCount24h); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetTopMarkets scan: %w", err)
		}
		if vol.Valid && vol.String != "" && vol.String != "0" {
			v := vol.String
			m.Volume24hUSD = &v
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetTopMarkets rows: %w", err)
	}
	return out, nil
}

// assetMarketsCountQuery builds GetAssetMarketsCount's query: DISTINCT
// on canonOrientSQL's folded (base, quote), never the raw stored pair.
func assetMarketsCountQuery() string {
	canonBase, canonQuote, _ := canonOrientSQL(2)
	return `
		SELECT COUNT(*) FROM (
		  SELECT DISTINCT ` + canonBase + `, ` + canonQuote + `
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		) t
	`
}

// assetTopMarketsQuery builds GetAssetTopMarkets's query. Both stored orientations fold into the
// canonical (base, quote) before volume and count are summed, so one market fills one slot with
// its whole volume; side is the asset's side of that pair.
//
// Alias-complete: a market where ANY canonical form of the asset is base or quote counts, and the
// counterparty/side CASE tests membership so a pair keyed by the crypto:XLM or SAC form is not
// mislabelled as a counterparty.
func assetTopMarketsQuery() string {
	canonBase, canonQuote, _ := canonOrientSQL(3)
	return `
		WITH per_pair_24h AS (
		  SELECT ` + canonBase + ` AS base_asset, ` + canonQuote + ` AS quote_asset,
		         SUM(volume_usd) AS vol_usd
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND volume_usd IS NOT NULL
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		   GROUP BY 1, 2
		),
		per_pair_count AS (
		  SELECT ` + canonBase + ` AS base_asset, ` + canonQuote + ` AS quote_asset,
		         COUNT(*) FILTER (WHERE ts > now() - INTERVAL '24 hours') AS n
		    FROM trades
		   WHERE ts >= now() - INTERVAL '24 hours'
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		   GROUP BY 1, 2
		)
		SELECT
		    CASE WHEN p.base_asset = ANY($1) THEN p.quote_asset ELSE p.base_asset END AS counterparty,
		    CASE WHEN p.base_asset = ANY($1) THEN 'base' ELSE 'quote' END             AS side,
		    p.vol_usd::text                                                           AS vol_24h_usd,
		    COALESCE(c.n, 0)                                                          AS n_24h
		  FROM per_pair_24h p
		  LEFT JOIN per_pair_count c
		    ON c.base_asset = p.base_asset AND c.quote_asset = p.quote_asset
		 ORDER BY p.vol_usd DESC NULLS LAST, p.base_asset, p.quote_asset
		 LIMIT $2
	`
}

// GetAssetBySlug returns one row matching the slug, with ListAssets's per-row metric shape. It
// returns sql.ErrNoRows when the slug matches no classic asset or traded Soroban-native contract
// (the discovered_assets arm the listing spine admits).
// getAssetBySlugSQL is its SQL; the chosen-CTE pattern keeps the volume sum and price
// triangulation on the same canonical row.
var getAssetBySlugSQL = `
		WITH chosen AS (
		  -- Three input shapes accepted (in tiebreak preference):
		  --   1. friendly slug (USDC, AQUA, EURC)        — slug column
		  --   2. raw code without issuer (USDC, AQUA)    — code column
		  --   3. canonical asset_id (USDC-GA5Z…)         — asset_id column
		  -- The OR-WHERE catches all three; the ORDER BY tiebreaks in
		  -- preference order so a slug input wins over a code-only
		  -- collision (the disambiguation guard from scam-token
		  -- protection still applies on the friendly-slug path because
		  -- a curated slug column value beats every code-only match).
		  -- Pre-2026-05-10 canonical asset_id form (CODE-ISSUER) 404'd
		  -- because the WHERE only matched COALESCE(slug, code).
		  --
		  -- Same spine as the listing's catalogue_assets: classic_assets
		  -- UNION the traded Soroban-native contracts from
		  -- discovered_assets (same asset_volume_24h bound, so an asset
		  -- the listing shows is one the detail can find and vice
		  -- versa). Without the UNION a Soroban contract id 404'd here
		  -- and /v1/assets/{id} depended entirely on the transitive
		  -- fill — its own XLM market could never price it.
		  SELECT asset_id, code, issuer_g_strkey, slug,
		         first_seen_ledger, last_seen_ledger, observation_count
		    FROM (
		      SELECT asset_id, code, issuer_g_strkey, slug,
		             first_seen_ledger, last_seen_ledger, observation_count
		        FROM classic_assets
		       WHERE COALESCE(slug, code) = $1
		          OR asset_id = $1
		      UNION ALL
		      SELECT d.contract_id AS asset_id,
		             NULL::text    AS code,
		             NULL::text    AS issuer_g_strkey,
		             NULL::text    AS slug,
		             d.first_seen_ledger,
		             d.last_seen_ledger,
		             d.event_count  AS observation_count
		        FROM discovered_assets d
		       WHERE d.contract_id = $1
		         AND EXISTS (SELECT 1 FROM asset_volume_24h v WHERE v.asset_id = d.contract_id)
		         AND NOT EXISTS (SELECT 1 FROM classic_assets c WHERE c.asset_id = d.contract_id)
		    ) u
		   ORDER BY (slug = $1) DESC NULLS LAST,
		            (asset_id = $1) DESC NULLS LAST,
		            observation_count DESC, asset_id ASC
		   LIMIT 1
		),
		per_asset_24h_vol AS (
		  SELECT SUM(volume_usd) AS vol_usd
		    FROM (
		      SELECT volume_usd FROM prices_1m
		       WHERE base_asset = (SELECT asset_id FROM chosen)
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = (SELECT asset_id FROM chosen)
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND volume_usd IS NOT NULL
		    ) t
		),
		` + assetPriceArmCTEs("(SELECT asset_id FROM chosen)", xlmQuotesBound(2)) + `,
		` + xlmUSDNativeCTEs("$2::text") + `
		SELECT
		    -- asset_id is the LAST resort here, and it is load-bearing.
		    --
		    -- A Soroban-native row from catalogue_assets has slug AND
		    -- code both NULL (a contract asset has no issuer account and
		    -- no SEP-1 code), so the two-arm COALESCE this used to be
		    -- returned NULL — and the slug column scans into a
		    -- non-nullable Go string. Measured in production 2026-08-28:
		    -- GET /v1/assets with limit=500 returned HTTP 500,
		    -- "converting NULL to string is unsupported", which also
		    -- hard-failed the
		    -- explorer build (it refuses to bake fallback HTML rather
		    -- than ship a page built on a broken fetch). limit=100
		    -- survived only because the ordering had not yet reached one
		    -- of the 60 Soroban rows — the bug was live at limit>=150.
		    --
		    -- The contract id is the RIGHT fallback, not merely a
		    -- non-null one: it is already this asset's URL segment
		    -- (/assets/CAUP7NFA… resolves today), so the slug a caller
		    -- gets back is the slug that actually works.
		    COALESCE(ca.slug, ca.code, ca.asset_id) AS slug,
		    ca.asset_id, ca.code, ca.issuer_g_strkey,
		    ca.first_seen_ledger, ca.last_seen_ledger, ca.observation_count,
		    -- One arm per asset, chosen and read exactly as the listing
		    -- rollup does (priceArmJoins below).
		    ROUND((` + snapshotPriceUSDExpr + `), ` + catalogueRoundPlacesForRow + `)::text AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    to_char(` + priceChangePctExpr("1h") + `, 'FM999999990.00') AS change_1h_pct,
		    to_char(` + priceChangePctExpr("24h") + `, 'FM999999990.00') AS change_24h_pct,
		    to_char(` + priceChangePctExpr("7d") + `, 'FM999999990.00') AS change_7d_pct,
		    -- Single-row slug lookup serves catalogue-verified currencies,
		    -- which are never dust — leave source_count unmeasured so the
		    -- valuation guard never suppresses here (shared scanAssetRow shape).
		    NULL::int                             AS source_count,
		    -- volume_character / sort_vol_usd / rank_tier are listing-only
		    -- (single-asset reads don't rank); the detail stamps
		    -- volume_character via the keyed AssetVolumeCharacterRollup
		    -- lookup, not this projector.
		    NULL::text                            AS volume_character,
		    NULL::numeric                         AS sort_vol_usd,
		    NULL::int                             AS rank_tier,
		    -- Scoped to the one asset_id vol sums; mergeContractArmVolume
		    -- reads each alias through here and ORs the flags.
		    EXISTS (SELECT 1 FROM asset_volume_24h u
		             WHERE u.asset_id = ca.asset_id AND u.unpriced_trades > 0) AS volume_lower_bound
		  FROM chosen ca
		  LEFT JOIN per_asset_24h_vol vol ON true` + priceArmJoins(xlmUSDLateralJoin("$2::text")) + `
`

// GetAssetBySlug looks up by friendly slug (USDC, AQUA, EURC),
// canonical asset_id (USDC-GA5Z…), OR raw code with case-insensitive
// retry — see the SQL's WHERE clause + the handler's case-fallback
// for the full input-shape table.
func (s *Store) GetAssetBySlug(ctx context.Context, slug string) (AssetRow, error) {
	r, err := scanAssetRow(s.db.QueryRowContext(ctx, getAssetBySlugSQL, slug, canonical.NativeSACContractID()))
	if err != nil {
		// Surface sql.ErrNoRows unwrapped so handler errors.Is checks
		// keep matching; scanAssetRow wraps with %w which preserves it.
		return AssetRow{}, err
	}
	return r, nil
}

// GetAssetByAssetID is a thin wrapper kept for clarity at the
// handler layer — the underlying SQL accepts canonical asset_id
// alongside friendly slug, so this just calls GetAssetBySlug.
func (s *Store) GetAssetByAssetID(ctx context.Context, assetID string) (AssetRow, error) {
	return s.GetAssetBySlug(ctx, assetID)
}

// GetNativeAssetRow returns the synthetic AssetRow for native XLM, which has no classic_assets row
// (that table tracks issued assets only). Without it `/v1/coins/XLM` would 404 or let an issued
// token with code "XLM" (a scam token) win the disambiguation tiebreak.
//
//   - Slug / Code "XLM"; AssetID "native"; no issuer; first/last seen ledger 0 (unset).
//   - ObservationCount 0 with ObservationCountUnmeasured set; nothing cheap counts native trades.
//   - PriceUSD and Change*Pct use the same xlm_usd stablecoin-proxy chain as the listing.
//   - Volume24hUSD sums volume_usd where native is base or quote over 24h.
//   - MarketCapUSD and CirculatingSupply are NULL; native has no supply row.
//
// It always returns a populated row (no sql.ErrNoRows path): the CTEs LEFT JOIN out to NULL.
func (s *Store) GetNativeAssetRow(ctx context.Context) (AssetRow, error) {
	row, err := scanAssetRow(s.db.QueryRowContext(ctx, getNativeAssetSQL, canonical.NativeSACContractID()))
	if err != nil {
		return row, err
	}
	row.ObservationCountUnmeasured = true
	return row, nil
}

// getNativeAssetSQL is GetNativeAssetRow's query, a constant to keep funlen happy. Its columns
// match listAssetsBaseSelect and getAssetBySlugSQL so scanAssetRow reads all three.
var getNativeAssetSQL = `
		WITH per_asset_24h_vol AS (
		  SELECT SUM(volume_usd) AS vol_usd
		    FROM (
		      SELECT volume_usd FROM prices_1m
		       WHERE base_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND volume_usd IS NOT NULL
		    ) t
		),
		` + xlmUSDNativeCTEs("$1::text") + `,
		ledger_bounds AS (
		  -- Always return one row with placeholder zeros — the
		  -- previous version scanned the trades hypertable for
		  -- 7 days OF native-touching rows, which is millions of
		  -- rows on a busy ledger and timed out under lock-table
		  -- pressure (53200). Native XLM is well-known; the
		  -- explorer doesn't need an accurate first_seen_ledger
		  -- for it. observation_count is unmeasured for native (no
		  -- registry row counts its trades); GetNativeAssetRow flags it.
		  SELECT
		    0::bigint AS first_ledger,
		    0::bigint AS last_ledger,
		    0::bigint AS obs_count
		)
		SELECT
		    'XLM'                                  AS slug,
		    'native'                               AS asset_id,
		    'XLM'                                  AS code,
		    ''                                     AS issuer_g_strkey,
		    lb.first_ledger                        AS first_seen_ledger,
		    lb.last_ledger                         AS last_seen_ledger,
		    lb.obs_count                           AS observation_count,
		    ROUND((SELECT vwap FROM xlm_usd), 10)::text AS price_usd,
		    vol.vol_usd                            AS volume_24h_usd,
		    NULL::numeric                          AS market_cap_usd,
		    NULL::numeric                          AS circulating_supply,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_1h_pct,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_24h_pct,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_7d_pct,
		    -- Native XLM is always deep-liquidity — leave source_count
		    -- unmeasured (shared scanAssetRow shape); the guard never fires.
		    NULL::int                              AS source_count,
		    NULL::text                             AS volume_character,
		    NULL::numeric                          AS sort_vol_usd,
		    NULL::int                              AS rank_tier,
		    EXISTS (SELECT 1 FROM asset_volume_24h u
		             WHERE u.asset_id = 'native' AND u.unpriced_trades > 0) AS volume_lower_bound
		  FROM ledger_bounds lb
		  LEFT JOIN per_asset_24h_vol vol ON true
`

// LatestAssetStats returns per-asset 24h volume and supply stats for /v1/assets/{id}. Volume sums
// prices_1m.volume_usd where the asset is base or quote (as Volume24hUSDForAsset). Supply is
// always null: classic_asset_stats_5m never got a writer. A row with no stats is not an error.
func (s *Store) LatestAssetStats(ctx context.Context, assetID string) (AssetRow, error) {
	// Alias-complete: sum the asset's volume across all its canonical
	// forms (mirrors Volume24hUSDForAsset), so the /v1/assets/{id} volume
	// figure includes the crypto:XLM (CEX) and SAC (Soroban) legs.
	const q = `
		SELECT COALESCE(SUM(volume_usd), 0)::text, ` + aliasVolumeLowerBoundSQL + `
		  FROM (
		    SELECT volume_usd FROM prices_1m
		     WHERE base_asset = ANY($1)
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket <= now() - INTERVAL '1 minute'
		    UNION ALL
		    SELECT volume_usd FROM prices_1m
		     WHERE quote_asset = ANY($1)
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket <= now() - INTERVAL '1 minute'
		  ) t
	`
	var (
		vol        string
		lowerBound bool
	)
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID)).Scan(&vol, &lowerBound); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: LatestAssetStats: %w", err)
	}
	out := AssetRow{AssetID: assetID}
	if vol != "" && vol != "0" {
		out.Volume24hUSD = &vol
		out.VolumeLowerBound = lowerBound
	}
	return out, nil
}

// ValidateAssetsCursor errors if a non-empty `cursor` does not match the shape the listing emits
// for the order; an empty cursor is valid. Handlers should answer 400: a bad cursor otherwise
// degrades the keyset predicate to match no rows, an empty page that looks like end-of-pagination.
func ValidateAssetsCursor(cursor string, order AssetsOrder) error {
	if cursor == "" {
		return nil
	}
	tier, prefix, suffix, ok := splitAssetsCursor(cursor)
	if !ok {
		return fmt.Errorf("missing ':' separator")
	}
	if suffix == "" {
		return fmt.Errorf("missing asset_id suffix")
	}
	// Rank tier — the leading ORDER BY key. Synthesised as "0" for a
	// two-field cursor, so this only rejects a genuinely
	// malformed three-field one.
	if !isDigitString(tier) {
		return fmt.Errorf("non-numeric rank-tier prefix")
	}
	// The tier binds to `$n::int`, so it must fit int32; isDigitString alone accepts "2147483648",
	// which would error at the database instead of the boundary. Bounded by type, not a `> 2`
	// reject: the tier set is order-dependent, and a literal maximum would add a fourth site to
	// the three-call-site lockstep contract.
	if _, err := strconv.ParseInt(tier, 10, 32); err != nil {
		return fmt.Errorf("rank-tier prefix out of range")
	}
	if order == AssetsOrderVolume24hUSDDesc {
		// Volume prefix may be empty (last row had a null vol_usd)
		// or a Postgres-style numeric: digits with at most one '.'.
		if prefix != "" && !isNumericPrefix(prefix) {
			return fmt.Errorf("non-numeric volume prefix")
		}
		return nil
	}
	if prefix == "" {
		return fmt.Errorf("missing observation_count prefix")
	}
	if !isDigitString(prefix) {
		return fmt.Errorf("non-numeric observation_count prefix")
	}
	// Must fit the int64 the keyset binds. An over-int64 digit run passes isDigitString, then
	// parseAssetCursor's ParseInt fails and degrades the cursor to (0, 0, ""), a silent 200 with
	// an empty page that reads as end-of-pagination. A malformed cursor must 400.
	if _, err := strconv.ParseInt(prefix, 10, 64); err != nil {
		return fmt.Errorf("observation_count prefix out of range")
	}
	return nil
}

// isDigitString reports whether s is a non-empty run of ASCII digits.
func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for j := 0; j < len(s); j++ {
		if s[j] < '0' || s[j] > '9' {
			return false
		}
	}
	return true
}

// isNumericPrefix returns true for a digit string with at most one '.'; there are no negative
// volumes, so no leading '-'. At least ONE digit is required: a lone "." would otherwise pass
// validation and reach the keyset as a $n::numeric bind Postgres rejects. The empty prefix is
// valid and handled by the caller (the last row had a NULL vol_usd).
func isNumericPrefix(s string) bool {
	dot := false
	digit := false
	for j := 0; j < len(s); j++ {
		c := s[j]
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digit
}

// parseAssetCursor decodes a `<tier>:<obs_count>:<asset_id>` cursor. Empty and malformed cursors
// both give (0, 0, ""), so handlers must reject bad ones via ValidateAssetsCursor first.
func parseAssetCursor(cursor string) (tier int, obsCount int64, assetID string) {
	if cursor == "" {
		return 0, 0, ""
	}
	tierStr, obsStr, assetID, ok := splitAssetsCursor(cursor)
	if !ok || !isDigitString(obsStr) {
		return 0, 0, ""
	}
	n, err := strconv.ParseInt(obsStr, 10, 64)
	if err != nil {
		return 0, 0, ""
	}
	return atoiOrZero(tierStr), n, assetID
}

// EncodeAssetsCursor renders the keyset cursor for the LAST row of a page:
// `<rank_tier>:<sort_key>:<asset_id>`, the full ORDER BY tuple exactly as the query ranked the row.
// It pairs with [parseAssetCursor] / [parseVolumeCursor] and lives here so the encode side cannot
// drift from assetsOrderBy and assetsCursorPredicate. A nil RankTier encodes as tier 0.
func EncodeAssetsCursor(row AssetRow, order AssetsOrder) string {
	tier := 0
	if row.RankTier != nil {
		tier = *row.RankTier
	}
	sortKey := ""
	if order == AssetsOrderVolume24hUSDDesc {
		if row.SortVolume24hUSD != nil {
			sortKey = *row.SortVolume24hUSD
		}
	} else {
		sortKey = strconv.FormatInt(row.ObservationCount, 10)
	}
	return fmt.Sprintf("%d:%s:%s", tier, sortKey, row.AssetID)
}

// GetAssetsPriceHistory24hBatch returns 24h hourly USD-price series for many assets in one query,
// keyed by asset_id, up to 24 points oldest first; an asset with no trades gets an empty slice.
// It uses the same alias-complete pick as GetAssetPriceHistory24h so a listing sparkline and its
// detail chart agree.
func (s *Store) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	forms, owners, prios := assetAliasRows(assetIDs)
	rows, err := s.db.QueryContext(ctx, getAssetsPriceHistory24hBatchSQL, forms, owners, prios, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]AssetPricePoint, len(assetIDs))
	for rows.Next() {
		var assetID string
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&assetID, &pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out[assetID] = append(out[assetID], pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch rows: %w", err)
	}
	return out, nil
}

// getAssetsPriceHistory24hBatchSQL is GetAssetsPriceHistory24hBatch's
// query, hoisted so TestProxyQuoteLists_Lockstep can pin its XLM arms.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var getAssetsPriceHistory24hBatchSQL = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		want AS (
		  -- One row per (alias form, requested id); see assetAliasRows.
		  SELECT * FROM unnest($1::text[], $2::text[], $3::int8[]) AS w(form, asset_id, prio)
		),
		want_ids AS (
		  SELECT asset_id, array_agg(form) AS forms, bool_or(form = 'native') AS is_xlm
		    FROM want GROUP BY asset_id
		),
		direct_per_hour AS (
		  -- The single-asset pick per requested id: highest-priority alias
		  -- form, then the latest bucket, then usdQuotePref. Quote set =
		  -- fiat:USD OR USDC (classic or its SAC form) per the
		  -- stablecoin-proxy policy (see the listing query's direct_usd CTE).
		  SELECT DISTINCT ON (asset_id, h) asset_id, h, vwap
		    FROM (
		      SELECT w.asset_id, date_trunc('hour', p.bucket) AS h, p.vwap::numeric AS vwap,
		             w.prio, p.bucket, ` + usdQuotePref + ` AS quote_prio
		        FROM prices_1m p
		        JOIN want w ON w.form = p.base_asset
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN (
		           'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		           'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		           'fiat:USD'
		         )
		         AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		         AND vwap IS NOT NULL
		    ) z
		   ORDER BY asset_id, h, prio, bucket DESC, quote_prio
		),
		asset_xlm_per_hour AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and BOTH
		  -- stored directions, base-side preferred per (asset, hour);
		  -- see GetAssetPriceHistory24h's asset_xlm_per_hour.
		  SELECT DISTINCT ON (asset_id, h) asset_id, h, vwap
		    FROM (
		      SELECT w.asset_id, date_trunc('hour', p.bucket) AS h, p.vwap::numeric AS vwap,
		             w.prio, p.bucket, 0 AS inverted, ` + xlmFormPrefOpenBound4 + `quote_asset) AS xlm_prio
		        FROM prices_1m p
		        JOIN want w ON w.form = p.base_asset
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN ('native', $4::text)
		         AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT w.asset_id, date_trunc('hour', p.bucket), 1::numeric / p.vwap,
		             w.prio, p.bucket, 1, ` + xlmFormPrefOpenBound4 + `base_asset)
		        FROM prices_1m p
		        JOIN want w ON w.form = p.quote_asset
		       WHERE base_asset IN ('native', $4::text)
		         AND quote_asset = ANY($1)
		         AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, h, inverted, prio, bucket DESC, xlm_prio
		),
		` + xlmUSDAnchorPerPeriodCTE("xlm_usd_per_hour", "h", "hour",
	`date_trunc('hour', now() - INTERVAL '23 hours')`, "$4::text") + `
		SELECT
		    w.asset_id,
		    to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.is_xlm THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), ` + catalogueRoundPlacesForWanted + `)::text AS p
		  FROM want_ids w
		  CROSS JOIN hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY w.asset_id, hours.bucket ASC
`

// GetAssetsPriceHistory7dBatch returns 7d daily USD-price series for
// many assets in one query. 7-bucket-deep daily grain, mirroring
// the per-asset GetAssetPriceHistory7d. Same alias-complete
// direct-then-XLM-triangulated pick; one query for many asset_ids.
func (s *Store) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	forms, owners, prios := assetAliasRows(assetIDs)
	rows, err := s.db.QueryContext(ctx, getAssetsPriceHistory7dBatchSQL, forms, owners, prios, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]AssetPricePoint, len(assetIDs))
	for rows.Next() {
		var assetID string
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&assetID, &pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out[assetID] = append(out[assetID], pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch rows: %w", err)
	}
	return out, nil
}

// getAssetsPriceHistory7dBatchSQL is GetAssetsPriceHistory7dBatch's
// query, hoisted so TestProxyQuoteLists_Lockstep can pin its XLM arms.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var getAssetsPriceHistory7dBatchSQL = `
		WITH days AS (
		  SELECT generate_series(
		    date_trunc('day', now() - INTERVAL '6 days'),
		    date_trunc('day', now()),
		    INTERVAL '1 day'
		  ) AS bucket
		),
		want AS (
		  -- One row per (alias form, requested id); see assetAliasRows.
		  SELECT * FROM unnest($1::text[], $2::text[], $3::int8[]) AS w(form, asset_id, prio)
		),
		want_ids AS (
		  SELECT asset_id, array_agg(form) AS forms, bool_or(form = 'native') AS is_xlm
		    FROM want GROUP BY asset_id
		),
		direct_per_day AS (
		  -- The single-asset pick per requested id: highest-priority alias
		  -- form, then the latest bucket, then usdQuotePref. Quote set =
		  -- fiat:USD OR USDC (classic or its SAC form) per the
		  -- stablecoin-proxy policy (see the listing query's direct_usd CTE).
		  SELECT DISTINCT ON (asset_id, d) asset_id, d, vwap
		    FROM (
		      SELECT w.asset_id, date_trunc('day', p.bucket) AS d, p.vwap::numeric AS vwap,
		             w.prio, p.bucket, ` + usdQuotePref + ` AS quote_prio
		        FROM prices_1m p
		        JOIN want w ON w.form = p.base_asset
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN (
		           'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		           'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		           'fiat:USD'
		         )
		         AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		         AND vwap IS NOT NULL
		    ) z
		   ORDER BY asset_id, d, prio, bucket DESC, quote_prio
		),
		asset_xlm_per_day AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and BOTH
		  -- stored directions, base-side preferred per (asset, day);
		  -- see GetAssetPriceHistory24h's asset_xlm_per_hour.
		  SELECT DISTINCT ON (asset_id, d) asset_id, d, vwap
		    FROM (
		      SELECT w.asset_id, date_trunc('day', p.bucket) AS d, p.vwap::numeric AS vwap,
		             w.prio, p.bucket, 0 AS inverted, ` + xlmFormPrefOpenBound4 + `quote_asset) AS xlm_prio
		        FROM prices_1m p
		        JOIN want w ON w.form = p.base_asset
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN ('native', $4::text)
		         AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT w.asset_id, date_trunc('day', p.bucket), 1::numeric / p.vwap,
		             w.prio, p.bucket, 1, ` + xlmFormPrefOpenBound4 + `base_asset)
		        FROM prices_1m p
		        JOIN want w ON w.form = p.quote_asset
		       WHERE base_asset IN ('native', $4::text)
		         AND quote_asset = ANY($1)
		         AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, d, inverted, prio, bucket DESC, xlm_prio
		),
		` + xlmUSDAnchorPerPeriodCTE("xlm_usd_per_day", "d", "day",
	`date_trunc('day', now() - INTERVAL '6 days')`, "$4::text") + `
		SELECT
		    w.asset_id,
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.is_xlm THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), ` + catalogueRoundPlacesForWanted + `)::text AS p
		  FROM want_ids w
		  CROSS JOIN days
		  LEFT JOIN direct_per_day     d  ON d.d  = days.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_day  x  ON x.d  = days.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_day    xu ON xu.d = days.bucket
		 ORDER BY w.asset_id, days.bucket ASC
`
