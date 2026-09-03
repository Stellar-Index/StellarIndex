package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// assetAliasArray returns the canonical alias forms of assetKey as a
// []string bound directly as a Postgres array for `= ANY($n)` membership. It is
// the single seam that makes an asset-detail read alias-complete: an
// asset's volume/ATH/markets is the aggregate over EVERY canonical form
// of that asset (XLM's native / crypto:XLM / SAC split, plus any
// operator-configured classic↔SAC wrapper), not the single spelling the
// caller happened to pass. The forms come back in canonical priority
// order (SAC LAST — see [canonical.AssetAliases]) so a price PICK that
// orders by array_position prefers the deep classic/native form and only
// falls back to a thin SAC pool. A key that doesn't parse falls back to
// itself, preserving the pre-alias single-form behaviour.
func assetAliasArray(assetKey string) []string {
	a, err := canonical.ParseAsset(assetKey)
	if err != nil {
		return []string{assetKey}
	}
	return canonical.AssetAliasStrings(a)
}

// AssetRow is the read-side projection of one row from the
// asset-discovery view: classic_assets joined with whatever supply +
// activity counters we have today. Pure-string fields keep the
// surface decoupled from the canonical types package.
//
// VWAP / Volume24hUSD / MarketCapUSD are nullable when the
// aggregator hasn't yet computed values for the asset — newly-
// observed assets, illiquid tokens with no off-chain peg, etc.
// Pointer types let the wire layer emit `null` cleanly.
type AssetRow struct {
	Slug             string
	AssetID          string
	Code             string
	IssuerGStrkey    string
	FirstSeenLedger  uint32
	LastSeenLedger   uint32
	ObservationCount int64

	// Latest VWAP-against-USD if available. Nil when:
	//   - no off-chain peg (illiquid Soroban-only token), or
	//   - the asset has no `fiat:USD` quote pair, or
	//   - the freeze writer has frozen this pair.
	PriceUSD *string
	// Trailing-24h USD-denominated trade volume. Nil when no
	// trades hit `usd_volume`-eligible quotes in 24h.
	Volume24hUSD *string
	// Market cap in USD = price × circulating_supply when both
	// are known. Nil when either component is missing.
	MarketCapUSD *string
	// Circulating supply in canonical units (decimal string;
	// scale matches asset decimals). Nil for assets the supply
	// pipeline doesn't yet cover.
	CirculatingSupply *string
	// Trailing-1h / 24h / 7d price change as a signed percentage
	// with two fractional digits (e.g. "+1.27", "-0.05", "0.00").
	// Nil when the asset has no current price, or when no
	// matching past-bucket exists in prices_1m within the
	// window-specific tolerance (±5min for 1h, ±30min for 24h,
	// ±2h for 7d).
	Change1hPct  *string
	Change24hPct *string
	Change7dPct  *string
	// SourceCount is the number of DISTINCT venues that backed PriceUSD —
	// array_length(prices_1m.sources) of the latest bucket the listing price
	// came from. It is the liquidity signal the API's market-cap valuation
	// guard reads (single-venue AND sub-floor volume → suppress the cap).
	// Nil when the price wasn't derived from a per-asset bucket on this query
	// (a triangulated native-XLM price, or the single-row slug/native queries
	// which serve verified currencies that are never dust) — a nil count is
	// treated as "unmeasured" and never suppresses.
	SourceCount *int

	// VolumeCharacter is the derived market|operational|concentrated label
	// from the asset_volume_character rollup (migration 0149,
	// wash-and-scam-signals design §2), LEFT JOINed by canonical asset_id.
	// Nil when the asset has no rolled row (no priced trades in the window,
	// or the worker hasn't run). ANALYTICS / DISPLAY — the raw
	// Volume24hUSD chain-fact is never altered by it.
	VolumeCharacter *string
	// SortVolume24hUSD is the concentration-adjusted 24h volume the default
	// AssetsOrderVolume24hUSDDesc sort ranks on (§4-B "annotate + demote":
	// raw × (1 − top_account_pair_vol_share) for concentrated/operational,
	// raw otherwise). It is a SORT KEY only — the keyset cursor encodes it
	// so pagination stays consistent with the ORDER BY. Never the displayed
	// number: Volume24hUSD stays the raw, visible chain fact. Nil outside
	// the listing query (the single-asset reads don't sort).
	SortVolume24hUSD *string
	// RankTier is the listing's PRIMARY (ascending) sort key — see
	// [listingRankTierExpr]: 0 rankable, 1 unpriced, 2 directory-flagged.
	// Like SortVolume24hUSD it is a SORT KEY only, never a displayed value,
	// and the keyset cursor encodes it so pagination stays consistent with
	// the ORDER BY. Nil outside the listing query (the single-asset reads
	// don't rank).
	RankTier *int
}

// AssetsOrder controls the sort + cursor scheme used by ListAssets.
// Default ObservationCountDesc preserves the original "rank by
// activity" semantics; Volume24hUSDDesc is the volume-first view
// the explorer's /assets table can opt into for live-volume
// rankings.
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
	// Code, when non-empty, restricts to rows whose classic asset
	// code matches EXACTLY (case-sensitive — Stellar codes are
	// case-significant; USDC and usdc are distinct assets). Codes
	// are not unique on Stellar, so combine with Issuer to pin a
	// single asset. Pushes down to the indexed classic_assets.code
	// column (BACKLOG #54).
	Code string
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

// ListAssets returns asset-directory rows ordered by observation
// count desc (a cheap proxy for activity).
//
// Pagination uses a keyset cursor: the cursor encodes the
// (observation_count, asset_id) tuple of the last row from the
// previous page. Empty cursor means "first page". Cursor format:
// `<observation_count>:<asset_id>`.
func (s *Store) ListAssets(ctx context.Context, limit int, issuer, cursor string) ([]AssetRow, error) {
	return s.ListAssetsExt(ctx, ListAssetsOptions{Limit: limit, Issuer: issuer, Cursor: cursor})
}

// LatestCirculatingSupply returns the most-recent circulating supply per
// canonical asset_id from the supply_1d CAGG — the set the three-domain
// supply pipeline currently tracks (the major classic + native assets).
// Keys are canonical asset_ids: supply_1d's asset_key is CODE:ISSUER
// (colon) and `XLM`, which we translate to the listing's CODE-ISSUER
// (dash) and `native`. The /v1/assets listing uses this to fill
// market_cap WHERE supply exists rather than leaving every row null;
// coverage grows as the supply pipeline expands (it's small today).
func (s *Store) LatestCirculatingSupply(ctx context.Context) (map[string]string, error) {
	const q = `
        SELECT CASE WHEN asset_key = 'XLM' THEN 'native'
                    ELSE replace(asset_key, ':', '-') END AS asset_id,
               circulating_supply::text
          FROM supply_1d s1
         WHERE circulating_supply IS NOT NULL
           AND bucket = (SELECT max(bucket) FROM supply_1d s2 WHERE s2.asset_key = s1.asset_key)
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestCirculatingSupply: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var assetID, circ string
		if err := rows.Scan(&assetID, &circ); err != nil {
			return nil, fmt.Errorf("timescale: LatestCirculatingSupply scan: %w", err)
		}
		out[assetID] = circ
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestCirculatingSupply rows: %w", err)
	}
	return out, nil
}

// ListAssetsExt is ListAssets with the full options bag. ListAssets
// is preserved as the legacy 3-arg call so existing callers
// (handler, integration tests) compile unchanged; new callers
// pass ListAssetsOptions to opt into Q.
func (s *Store) ListAssetsExt(ctx context.Context, opts ListAssetsOptions) ([]AssetRow, error) {
	// Clamp to the documented page size, allowing one extra row for the
	// caller's overfetch-by-one pagination sentinel (501, not 500).
	// F-1326/G3-03: the previous `> 500 → 100` reset silently truncated
	// a 501-row overfetch back to 100 and dropped the cursor — clamp to
	// the ceiling instead of resetting to the default.
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 501:
		limit = 501
	}
	query, args := buildAssetsQuery(limit, opts.Issuer, opts.Code, opts.Cursor, opts.Q, opts.Order)
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

// scanAssetRow is the row-projection shared by ListAssetsExt and
// GetAssetBySlug. The two queries return the same column shape, so
// the scan + nullable-string unpack lives in one place. Pulled
// out of the listing loop so ListAssetsExt stays under the gocognit
// threshold as the wire shape grows.
func scanAssetRow(scanner interface {
	Scan(dest ...any) error
},
) (AssetRow, error) {
	var r AssetRow
	var (
		firstLedger, lastLedger int64
		// code / issuer_g_strkey are NULL for every Soroban-native row
		// in catalogue_assets — a contract asset has no issuer account
		// and no SEP-1 code — while AssetRow types both as a plain
		// string. Scanning NULL into those is a hard error that fails
		// the WHOLE request, not just the row.
		//
		// Fixed here rather than with another SQL COALESCE because the
		// first attempt at this bug (v0.47.1) COALESCEd only `slug` and
		// shipped, whereupon production moved straight on to "column
		// index 2, code". The defect was never about one column: it is
		// that three columns are NULL by nature and all three scan into
		// non-nullable strings. Handling it at the scan covers the set,
		// and any future nullable column added to the Soroban arm.
		//
		// Empty string, NOT the contract id: a Soroban asset genuinely
		// has no code and no issuer, and substituting its contract id
		// would state something false. (slug keeps its COALESCE to
		// asset_id — that one IS true, because the contract id really
		// is the asset's URL segment.)
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
		&volumeCharacter, &sortVolume24hUSD, &rankTier,
	); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: scan asset: %w", err)
	}
	// slug carries a SQL COALESCE to asset_id, so it should never arrive
	// NULL — this mirrors that fallback in Go so a future query edit
	// that drops the COALESCE degrades to the contract id instead of
	// failing the whole request. Belt and braces, deliberately: the
	// braces are what broke in production.
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

// adjustedVolume24hExpr is the §4-B "annotate + demote" concentration-
// adjusted 24h volume the default AssetsOrderVolume24hUSDDesc sort ranks on
// (wash-and-scam-signals design §4, operator-CONFIRMED). It multiplies the
// RAW 24h USD volume (asset_volume_24h rollup, LEFT JOIN alias `vol`) by
// (1 − top_account_pair_vol_share) for assets the asset_volume_character
// rollup (LEFT JOIN alias `avc`) labels `concentrated` or `operational`,
// and leaves `market` / unrated assets at their raw volume. Effect: a
// wash/operational asset with fabricated volume sinks in the ranking
// proportional to how concentrated it is, so it no longer sits atop the
// directory painting legitimacy — while the raw volume_24h_usd chain fact
// stays UNCHANGED and visible in the payload, and the asset stays present
// (annotate + demote, never hide or alter the raw number).
//
// It is the SORT KEY, not a displayed value. It is referenced in THREE
// places that must stay identical — the listing SELECT (as sort_vol_usd,
// so the keyset cursor can encode it), assetsOrderBy, and
// assetsCursorPredicate — which is why it is one const: an all-NUMERIC
// expression so the cursor round-trips exactly (::text out, ::numeric in).
const adjustedVolume24hExpr = `(COALESCE(vol.vol_usd, 0) * ` +
	`CASE WHEN avc.character IN ('concentrated', 'operational') ` +
	`THEN GREATEST(0::numeric, 1::numeric - avc.top_account_pair_vol_share::numeric) ` +
	`ELSE 1::numeric END)`

// listingPriceUSDExpr is the USD price the listing serves, BEFORE the
// wire rounding — extracted from the price_usd column so the rank-tier
// expression can ask "does this row have a price at all?" without a
// second, drifting copy of the direct-or-XLM-triangulated chain.
const listingPriceUSDExpr = `COALESCE(
		      CASE WHEN ca.asset_id = 'native'
		           THEN (SELECT vwap FROM xlm_usd)
		           ELSE NULL
		      END,
		      direct.vwap,
		      vs_xlm.vwap * (SELECT vwap FROM xlm_usd)
		    )`

// scamFlagTagsSQLArray inlines [DirectoryScamFlagTags] as a SQL array
// LITERAL rather than a bind parameter. Deliberate: the predicate below
// is spliced into the SELECT list, the ORDER BY and the keyset WHERE —
// three places composed at three different positional-argument offsets —
// so it has to be one parameter-free string (the same reason
// adjustedVolume24hExpr is a const). The values are a compile-time
// vocabulary, never caller input, and mustSQLTextArrayLiteral panics at
// package init on anything outside [a-z], matching
// listAssetsBaseSelectSQL's fail-loud posture on its pushdown anchor.
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

// directoryScamFlaggedExpr is TRUE when the row's ISSUER carries a
// scam-class curated-directory tag — the SQL twin of
// pricingguard.IsDirectoryScamFlagged over the SAME
// [DirectoryScamFlagTags] list, lowercased on both sides so the two
// predicates agree tag for tag (an asset that shows the explorer's
// "⚠ Flagged" pill is exactly an asset this demotes).
//
// dir.tags is NULL for the overwhelming majority of rows (issuer absent
// from the ~18.5k-row curated directory) and for every Soroban-native row
// (a contract asset has no issuer account); unnest(NULL) yields zero rows
// so EXISTS is false and the row ranks normally — fail-OPEN, matching the
// directory overlay and the scam pricing gate.
var directoryScamFlaggedExpr = `EXISTS (SELECT 1 FROM unnest(dir.tags) t ` +
	`WHERE lower(t) = ANY(` + scamFlagTagsSQLArray + `))`

// rankTierMarker is replaced with [listingRankTierExpr] for the active
// order when [listAssetsBaseSelectSQL] renders the base SELECT. The
// SELECT is one const shared by both orders but the tier is not, so the
// substitution happens at render time — same marker idiom as the
// /*PUSHDOWN_…*/ comments.
const rankTierMarker = "/*RANK_TIER*/"

// listingRankTierExpr is the PRIMARY, ASCENDING sort key of the
// /v1/assets listing (#356): a small integer tier that is compared
// BEFORE the volume / observation-count key, so it dominates whatever
// the active sort is.
//
//	0 — rankable.
//	1 — unpriced: no USD price at all. Volume order only — that listing
//	    IS a price/market-cap table, so a row with no price must not
//	    outrank one that has a price. The observation-count order is an
//	    ACTIVITY ranking whose contract says nothing about price, so it
//	    keeps tiers {0, 2} only.
//	2 — directory-flagged: the issuer carries a scam-class tag. Applies
//	    to EVERY order. The row and its "⚠ Flagged" pill stay — we do not
//	    hide a flagged asset, we refuse to RANK it. Withholding its price
//	    (pricingguard.ScamGate + the API payload suppression) while still
//	    ranking it above real assets on 24h volume was the half-measure
//	    #356 reported: a wash-traded scam token sat at #12 on the
//	    flagship /assets page with no price, no market cap and a red pill.
//
// It is emitted as the rank_tier column so the keyset cursor can encode
// the same value the ORDER BY ranks on — a cursor that omits the LEADING
// sort key skips or repeats rows across pages — and is repeated verbatim
// in assetsOrderBy + assetsCursorPredicate. Same three-call-site contract
// as adjustedVolume24hExpr; keep the three in step.
func listingRankTierExpr(order AssetsOrder) string {
	if order == AssetsOrderVolume24hUSDDesc {
		return `(CASE WHEN ` + directoryScamFlaggedExpr + ` THEN 2 ` +
			`WHEN ` + listingPriceUSDExpr + ` IS NULL THEN 1 ELSE 0 END)`
	}
	return `(CASE WHEN ` + directoryScamFlaggedExpr + ` THEN 2 ELSE 0 END)`
}

// listAssetsBaseSelect is the CTE-laden SELECT shared by every
// permutation of WHERE-clause buildAssetsQuery composes. Pulled
// out of the function body so buildAssetsQuery stays under the
// funlen threshold and the SQL is editable as a single block.
//
// Volume aggregation: prices_1m.volume_usd summed across the
// trailing 24h, where the asset participates as base OR quote.
// classic_asset_stats_5m used to be the intended source; it never
// got a writer and migration 0152 dropped it (#358). Most classic
// assets have no direct fiat:USD pair either. The CTE-with-UNION
// sidesteps both.
//
// Price + 24h change: latest + 24h-ago snapshots, with XLM
// triangulation when no direct USD-quote pair (fiat:USD or the
// USDC stablecoin proxy — see the direct_usd CTE comment)
// exists. DISTINCT ON gives one "latest per asset" row without a
// window function. ±30min tolerance on 24h-ago so sparse-trade
// assets still produce a change %. Change is computed as
// (latest / ago - 1) * 100 to two fractional digits; NULL when
// either side is missing.
//
// market_cap_usd + circulating_supply remain NULL — their proper
// sources (asset_supply_history) aren't running for the long
// tail of classic assets today, and fabricating values would
// defeat the "stop lying" rule.
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
		  -- #43 (2026-07-06 latency incident): read the trailing-24h
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
		  SELECT asset_id, vol_usd
		    FROM asset_volume_24h
		),
		direct_usd AS (
		  -- "Direct USD" = quoted in fiat:USD (CEX feeds) OR in
		  -- USDC — the same stablecoin-proxy policy the xlm_usd
		  -- CTEs below already apply (CLAUDE.md: "stablecoin
		  -- fiat-proxy is aggregator policy" — USDC ≈ USD, ~0.1%
		  -- peg error accepted). A USDC-quoted vwap is taken AS
		  -- the USD price. Without the USDC member, assets whose
		  -- only markets quote in USDC (AUDD, EURC, every *allow
		  -- variant…) had 24h volume but a NULL price_usd — 473 of
		  -- 500 listing rows priceless (2026-08-24 operator
		  -- report). USDC counts in BOTH identity forms: the
		  -- classic G-issuer line AND its SAC contract (CCW67T… —
		  -- Soroban-venue trades carry the SAC id; the
		  -- AliasRegistry in internal/canonical/alias.go unifies
		  -- these on serving paths, but this SQL joins literal
		  -- strings, and USDC↔SAC is one of the two compile-time-
		  -- known wrapper families, operator-pinned via r1's
		  -- [supply.sac_wrappers]). DISTINCT ON + bucket DESC
		  -- keeps the freshest row across all quote forms.
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap,
		         array_length(sources, 1) AS source_count
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_1h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_24h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_7d AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm AS (
		  -- XLM leg in BOTH identity forms: 'native' AND its SAC
		  -- contract (CAS3J7… = canonical.XLMSacContractID,
		  -- alias.go) — soroswap/phoenix/aquarius trades quote in
		  -- the SAC form, which the AliasRegistry unifies on
		  -- serving paths but literal-string SQL must list
		  -- explicitly. Both legs multiply by the same xlm_usd.
		  -- BASE-side identity folding (a SAC-form base_asset row
		  -- surfacing alongside its classic twin — "USDC shows
		  -- twice") is now done in the API listing handler by
		  -- v1.Server.foldAliasTwins, which collapses each
		  -- non-canonical alias row onto its canonical row via the
		  -- same AliasRegistry (task #28 Part A).
		  --
		  -- ...and in BOTH stored DIRECTIONS. Sources that write swap
		  -- direction (aquarius: base = token_in, no canonical.Orient)
		  -- store a token bought with XLM as (XLM-SAC, token) — XLM as
		  -- BASE. Reading only quote_asset IN (XLM forms) left that
		  -- whole market invisible: r1 2026-08-28, CBIJ… had $730k/7d
		  -- against the XLM SAC and served price_usd null with no
		  -- withheld verdict. The inverted arm reads (XLM, token) and
		  -- inverts (vwap is "XLM priced in token", so 1/vwap is the
		  -- token in XLM). The volume path already read both
		  -- directions (soroban_volume.go) — which is exactly why the
		  -- asset had volume and no price.
		  --
		  -- Base-side rows are preferred over inverted ones (ORDER BY
		  -- inverted BEFORE bucket) so every asset that already priced
		  -- keeps byte-identical output; the inverted arm only fills
		  -- assets that had NO base-side row in the window.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap, source_count
		    FROM (
		      SELECT base_asset AS asset_id, vwap,
		             array_length(sources, 1) AS source_count,
		             bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= now() - INTERVAL '7 days'
		         AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap,
		             array_length(sources, 1),
		             bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= now() - INTERVAL '7 days'
		         AND vwap > 0 /*PUSHDOWN_QUOTE*/
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_1h AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		         AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		         AND vwap > 0 /*PUSHDOWN_QUOTE*/
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_24h AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		         AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		         AND vwap > 0 /*PUSHDOWN_QUOTE*/
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_7d AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		         AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		         AND vwap > 0 /*PUSHDOWN_QUOTE*/
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		xlm_usd AS (
		  -- prices_1m doesn't carry (native, fiat:USD) rows — XLM's
		  -- USD price is computed by the aggregator's triangulation
		  -- worker and lives in Redis, not the materialised view.
		  -- Mirror the aggregator's stablecoin-proxy policy in SQL
		  -- (CLAUDE.md: "stablecoin fiat-proxy is aggregator policy"
		  -- — USDC ≈ USD): use the latest on-chain XLM/USDC vwap as
		  -- the XLM/USD price. Circle's USDC issuer G-strkey is
		  -- hardcoded; a future revision pulls the list from
		  -- [trades].usd_pegged_classic_assets.
		  --
		  -- The 24h floor on bucket is REQUIRED, not just an
		  -- optimisation. With no time predicate TimescaleDB cannot
		  -- chunk-prune, so ORDER BY bucket DESC LIMIT 1 across the
		  -- 3 quote_assets must consider EVERY prices_1m chunk
		  -- (thousands post-backfill). Warm + idle that is ~13ms,
		  -- but the all-chunks access pattern degrades badly under
		  -- concurrent load + cold buffers -- observed ~40s in
		  -- pg_stat_activity during /v1/assets/{id} fan-out, the
		  -- dominant tax on every native to USD price path
		  -- (this query is #18 == #21). Bounded to 24h it touches
		  -- ~1 day of chunks (~2-3ms) and stays resilient under
		  -- load. It is also MORE correct: the unbounded form could
		  -- surface a days-stale vwap as the *current* price.
		  -- XLM/USDC is among the highest-volume pairs (trades
		  -- every minute) so a 24h floor never realistically misses
		  -- the latest. Mirrors the already-bounded
		  -- sources_stats.go xlm_usd CTE.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_1h AS (
		  -- 1h-ago XLM/USD via the same stablecoin-proxy policy.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_24h AS (
		  -- 24h-ago XLM/USD via the same stablecoin-proxy policy
		  -- as xlm_usd above.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_7d AS (
		  -- 7d-ago XLM/USD via the same stablecoin-proxy policy.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
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
		    -- meaningful tick size.
		    -- XLM itself (asset_id='native') has no rows in
		    -- asset_vs_xlm — (native, native) never exists — and
		    -- its 24h-bounded USD price lives in the xlm_usd CTE:
		    -- use that first. Since the USDC quote joined
		    -- direct_usd, native CAN pick up a direct row
		    -- (XLM/USDC, 7d window); it only surfaces when the 24h
		    -- xlm_usd is empty — the same 7d staleness tolerance
		    -- every other asset gets.
		    ROUND(` + listingPriceUSDExpr + `, 10)::text  AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_1h.vwap IS NOT NULL
		           AND direct_1h.vwap > 0
		      THEN to_char((direct.vwap / direct_1h.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_1h.vwap IS NOT NULL
		           AND vs_xlm_1h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_1h.vwap * (SELECT vwap FROM xlm_usd_1h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_1h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_24h.vwap IS NOT NULL
		           AND direct_24h.vwap > 0
		      THEN to_char((direct.vwap / direct_24h.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_24h.vwap IS NOT NULL
		           AND vs_xlm_24h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((vs_xlm.vwap     * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_24h.vwap * (SELECT vwap FROM xlm_usd_24h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_24h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_7d.vwap IS NOT NULL
		           AND direct_7d.vwap > 0
		      THEN to_char((direct.vwap / direct_7d.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_7d.vwap IS NOT NULL
		           AND vs_xlm_7d.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_7d.vwap * (SELECT vwap FROM xlm_usd_7d))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_7d_pct,
		    -- Distinct venues backing price_usd (the latest per-asset bucket's
		    -- prices_1m.sources) — the liquidity signal the API's market-cap
		    -- valuation guard reads. NULL for native XLM (triangulated price,
		    -- always liquid) and for any asset with no per-asset bucket.
		    CASE WHEN ca.asset_id = 'native' THEN NULL::int
		         ELSE COALESCE(direct.source_count, vs_xlm.source_count)
		    END                                   AS source_count,
		    avc.character                         AS volume_character,
		    ` + adjustedVolume24hExpr + ` AS sort_vol_usd,
		    -- Leading (ascending) sort key — see listingRankTierExpr. The
		    -- marker is substituted per-order at render time; it is emitted
		    -- as a column so the keyset cursor can encode the same value
		    -- the ORDER BY ranks on.
		    ` + rankTierMarker + ` AS rank_tier
		  FROM catalogue_assets ca
		  LEFT JOIN per_asset_24h_vol vol         ON vol.asset_id        = ca.asset_id
		  LEFT JOIN direct_usd        direct      ON direct.asset_id     = ca.asset_id
		  LEFT JOIN direct_usd_1h     direct_1h   ON direct_1h.asset_id  = ca.asset_id
		  LEFT JOIN direct_usd_24h    direct_24h  ON direct_24h.asset_id = ca.asset_id
		  LEFT JOIN direct_usd_7d     direct_7d   ON direct_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm      vs_xlm      ON vs_xlm.asset_id     = ca.asset_id
		  LEFT JOIN asset_vs_xlm_1h   vs_xlm_1h   ON vs_xlm_1h.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm_24h  vs_xlm_24h  ON vs_xlm_24h.asset_id = ca.asset_id
		  LEFT JOIN asset_vs_xlm_7d   vs_xlm_7d   ON vs_xlm_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_volume_character avc    ON avc.asset_id        = ca.asset_id
		  -- Curated third-party issuer labels (account_directory, migration
		  -- 0136). Read ONLY by listingRankTierExpr's scam-flag demotion —
		  -- the payload's issuer_directory_* fields are still stamped by the
		  -- API layer's batch lookup, so this join changes ranking, never
		  -- served data. Keyed on the issuer G-address exactly like
		  -- v1.Server.fillIssuerDirectoryTags, so "demoted" and "shows the
		  -- ⚠ Flagged pill" are the same set of rows. account_directory.address
		  -- is the PRIMARY KEY, so this join can never fan a listing row out
		  -- into duplicates; a NULL issuer (every Soroban-native row) simply
		  -- misses and ranks normally.
		  LEFT JOIN account_directory      dir    ON dir.address         = ca.issuer_g_strkey
`

// listAssetsBaseSelectSQL renders [listAssetsBaseSelect] with optional
// asset-filter pushdown into each per-asset CTE (#27). When
// pushdownPredicate is empty, the function strips the
// /*PUSHDOWN_BASE*/ + /*PUSHDOWN_QUOTE*/ marker comments and
// returns the SQL unchanged (each CTE materialises stats for
// every asset). When non-empty, it prepends a `chosen_assets`
// CTE that holds the asset_id list matching pushdownPredicate
// (which the caller must construct using positional args that
// match the rest of the query) and replaces each marker with
// `AND <side>_asset IN (SELECT asset_id FROM chosen_assets)`.
//
// Measured impact (2026-05-20 on r1, /v1/assets?issuer=GA5Z…):
// each per-asset price CTE without pushdown reads 256k rows
// for a 1-row result (1.3M buffer hits); with pushdown it reads
// ~9 rows. Applies to the eight direct_usd* / asset_vs_xlm* CTEs.
// per_asset_24h_vol no longer takes part: since #43 it reads the
// asset_volume_24h rollup (migration 0087), which is a small
// keyed-on-PK table regardless of the caller's filter, so it carries
// no PUSHDOWN markers. The four xlm_usd CTEs deliberately stay
// unfiltered — they look up XLM specifically, not the caller-supplied
// asset.
//
// pushdownPredicate is the WHERE-expression body for the
// chosen_assets CTE — e.g. "issuer_g_strkey = $1" or any
// combination using positional args $N matching the caller's
// args slice. buildAssetsQuery owns the arg-numbering contract
// (issuer is $1 when set, so the predicate is the literal
// string "issuer_g_strkey = $1").
func listAssetsBaseSelectSQL(pushdownPredicate string, order AssetsOrder) string {
	// The rank-tier marker is substituted FIRST and unconditionally: the
	// SELECT is one const shared by both orders but the tier is not, and
	// leaving the marker in place would ship `/*RANK_TIER*/ AS rank_tier`
	// — a syntax error, not a harmless comment. Exactly one occurrence.
	base := strings.Replace(listAssetsBaseSelect, rankTierMarker, listingRankTierExpr(order), 1)
	if strings.Contains(base, rankTierMarker) {
		panic("listAssetsBaseSelectSQL: more than one " + rankTierMarker + " marker in the base SELECT")
	}
	if pushdownPredicate == "" {
		// No pushdown — strip the marker comments. Leaving them
		// in is harmless (they're valid SQL comments) but stripping
		// keeps EXPLAIN output and pg_stat_statements normalised.
		s := strings.ReplaceAll(base, "/*PUSHDOWN_BASE*/", "")
		s = strings.ReplaceAll(s, "/*PUSHDOWN_QUOTE*/", "")
		return s
	}
	chosenCTE := "\n\t\tWITH chosen_assets AS (SELECT asset_id FROM classic_assets WHERE " + pushdownPredicate + "),\n\t\t"
	// Prepend the chosen_assets CTE (chosen first so its asset_id pool is
	// materialised once and reused by every downstream CTE).
	//
	// The anchor is the query's FIRST CTE, which is catalogue_assets.
	// It used to be per_asset_24h_vol; when catalogue_assets was added in
	// front of it this Replace silently became a NO-OP — strings.Replace
	// does not error on a missing needle — so every filtered listing lost
	// its pushdown and fell back to a full scan. The pushdown tests caught
	// it. Keep this anchor in step with whatever CTE comes first.
	s := strings.Replace(
		base,
		"\n\t\tWITH catalogue_assets AS (",
		chosenCTE+"catalogue_assets AS (",
		1,
	)
	if !strings.Contains(s, "chosen_assets AS (") {
		// Fail loud rather than serve an un-pushed-down query: a silent
		// miss here is a latency regression on the filtered paths, which
		// is exactly what the anchor drift above would have caused.
		panic("listAssetsBaseSelectSQL: pushdown anchor not found — the first CTE was renamed")
	}
	s = strings.ReplaceAll(s, "/*PUSHDOWN_BASE*/", "AND base_asset IN (SELECT asset_id FROM chosen_assets)")
	s = strings.ReplaceAll(s, "/*PUSHDOWN_QUOTE*/", "AND quote_asset IN (SELECT asset_id FROM chosen_assets)")
	return s
}

// refreshAssetVolumeUpsert recomputes the trailing-24h per-asset USD
// volume (single-sided: the asset as base OR quote) and upserts one row
// per asset into asset_volume_24h. This is the exact SUM the
// per_asset_24h_vol CTE used to inline per request (minus the pushdown
// markers, which don't apply to a full recompute), so the rollup value
// is byte-identical to the old live figure — only NUMERIC, never float
// (ADR-0003). computed_at is stamped to the transaction timestamp so
// the sibling prune can drop assets whose volume lapsed this pass.
const refreshAssetVolumeUpsert = `
INSERT INTO asset_volume_24h AS t (asset_id, vol_usd, computed_at)
SELECT asset_id, SUM(volume_usd) AS vol_usd, now()
  FROM (
    SELECT base_asset  AS asset_id, volume_usd
      FROM prices_1m
     WHERE bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
    UNION ALL
    SELECT quote_asset AS asset_id, volume_usd
      FROM prices_1m
     WHERE bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
  ) t
 GROUP BY asset_id
ON CONFLICT (asset_id) DO UPDATE
   SET vol_usd     = EXCLUDED.vol_usd,
       computed_at = EXCLUDED.computed_at`

// refreshAssetVolumePrune deletes assets that fell out of the
// trailing-24h window (no prices_1m volume counted them this pass, so
// their computed_at stayed at the previous run). Same one-transaction
// now() trick as the protocol-events rollup: just-upserted rows carry
// computed_at = now() and survive; stale rows carry an older timestamp
// and are dropped.
const refreshAssetVolumePrune = `DELETE FROM asset_volume_24h WHERE computed_at < now()`

// RefreshAssetVolume24h recomputes the asset_volume_24h rollup from the
// live trailing-24h prices_1m sum and atomically replaces its contents.
// Called on a slow cadence by the aggregator's assetvolrollup worker so
// the /v1/assets listing (per_asset_24h_vol CTE) never runs the
// all-asset ~256k-row SUM per request (2026-07-06 latency incident, #43).
//
// Upsert + prune run in one transaction (row-level locks only — no
// ACCESS EXCLUSIVE table lock that would stall concurrent listing reads
// on the customer-facing endpoint).
func (s *Store) RefreshAssetVolume24h(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, refreshAssetVolumeUpsert); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshAssetVolumePrune); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h commit: %w", err)
	}
	return nil
}

// buildAssetsQuery composes the WHERE + ORDER + LIMIT around
// listAssetsBaseSelectSQL, given the limit / issuer-filter /
// code-filter / keyset cursor / search query. The combinatorial
// explosion of (issuer × code × cursor × q) is too painful as a
// switch; use a slice + numbered placeholders.
func buildAssetsQuery(limit int, issuer, code, cursor, q string, order AssetsOrder) (string, []any) {
	var (
		conds         []string
		args          []any
		pushdownConds []string
	)
	if issuer != "" {
		args = append(args, issuer)
		conds = append(conds, fmt.Sprintf("ca.issuer_g_strkey = $%d", len(args)))
		// #27 pushdown: the issuer filter narrows the asset set
		// drastically (a single G-strkey typically issues handfuls
		// of assets, not thousands). chosen_assets materialises that
		// set once and every per-asset CTE filters against it,
		// dropping per_asset_24h_vol's row count from ~256k to ~9
		// in the GA5Z… case measured 2026-05-20. Reuses $1 (the
		// issuer arg) so no additional placeholder is needed.
		pushdownConds = append(pushdownConds, fmt.Sprintf("issuer_g_strkey = $%d", len(args)))
	}
	if code != "" {
		// BACKLOG #54: exact, case-sensitive code equality on the
		// indexed classic_assets.code column (classic_assets_code_idx).
		// A bare code is not unique (many issuers mint "USDC"), so it
		// only narrows to a handful of rows — but combined with issuer
		// it pins a single asset. Same chosen_assets pushdown as issuer:
		// reuses the outer-WHERE placeholder so no extra arg is bound.
		args = append(args, code)
		conds = append(conds, fmt.Sprintf("ca.code = $%d", len(args)))
		pushdownConds = append(pushdownConds, fmt.Sprintf("code = $%d", len(args)))
	}
	// The chosen_assets pushdown predicate ANDs whatever narrowing
	// filters are active (issuer, code, or both). Empty when neither
	// is set — listAssetsBaseSelectSQL then skips the CTE entirely.
	pushdownPredicate := strings.Join(pushdownConds, " AND ")
	if q != "" {
		args = append(args, "%"+q+"%")
		conds = append(conds, fmt.Sprintf(
			"(LOWER(ca.code) LIKE LOWER($%d) OR LOWER(COALESCE(ca.slug, ca.code)) LIKE LOWER($%d) OR LOWER(ca.issuer_g_strkey) LIKE LOWER($%d))",
			len(args), len(args), len(args)))
		// q-search pushdown intentionally NOT added — LIKE patterns
		// on three columns combined with the outer SELECT's
		// `ca.code LIKE` rule don't reduce as predictably as the
		// issuer case, and adding the same chosen_assets predicate
		// in WHERE would double-evaluate the same LIKE for no win.
		// If profiling later shows the q-only path is hot, add a
		// q-side chosen_assets variant. For now the SWR cache covers
		// the small filtered-LIST traffic.
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
	return listAssetsBaseSelectSQL(pushdownPredicate, order) + where + assetsOrderBy(order) + " LIMIT " + limitPlaceholder, args
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

// assetsCursorPredicate returns the WHERE clause that resumes
// pagination strictly past the supplied cursor under the active
// ordering. `argEnd` is the index of the last cursor placeholder
// (asset_id); the sort key is at argEnd-1 and the rank tier at argEnd-2.
//
// The tuple is (rank_tier ASC, sort_key, asset_id ASC) and the predicate
// mirrors it LEADING KEY FIRST — `t > ct OR (t = ct AND <rest>)`. Losing
// the leading key here is what makes a keyset cursor drop rows: with the
// tier in the ORDER BY but not the WHERE, page 2 would resume from the
// cursor's volume inside tier 0 and silently skip every tier-1/2 row
// whose volume was higher.
func assetsCursorPredicate(order AssetsOrder, argEnd int) string {
	tier := listingRankTierExpr(order)
	if order == AssetsOrderVolume24hUSDDesc {
		// Mixed-direction tuple compare: rank_tier ASC, adjusted-volume
		// DESC, asset_id ASC. The volume sort key is the §4-B
		// concentration-adjusted volume (adjustedVolume24hExpr), so the
		// keyset cursor encodes the SAME value the ORDER BY ranks on (the
		// listing emits it as sort_vol_usd). The expression is
		// COALESCE-to-zero on the raw volume, so a NULL-volume asset sorts
		// last as 0 exactly as before.
		return fmt.Sprintf(
			"(%s > $%d::int OR (%s = $%d::int AND "+
				"(%s < $%d::numeric OR (%s = $%d::numeric AND ca.asset_id > $%d))))",
			tier, argEnd-2, tier, argEnd-2,
			adjustedVolume24hExpr, argEnd-1, adjustedVolume24hExpr, argEnd-1, argEnd)
	}
	// Mixed-direction tuple compare, spelled out for the same reason as
	// the volume arm above: observation_count is ranked DESC but asset_id
	// ASC, and SQL's row-constructor compare is SAME-direction on every
	// element. `(observation_count, asset_id) < ($n, $m)` therefore reads
	// as "…AND asset_id < $m" on a tie, selecting rows the walk has
	// ALREADY served while skipping the ones it has not. On any tie in
	// observation_count — and ties are the norm in the long tail, where
	// most assets share a small observation_count — a plain
	// `GET /v1/assets` walk served some rows twice, never served others,
	// and then reported has_more=false as if it were complete
	// (wave-D KP-1 / RD-01, reproduced against real Postgres).
	return fmt.Sprintf(
		"(%s > $%d::int OR (%s = $%d::int AND "+
			"(ca.observation_count < $%d OR "+
			"(ca.observation_count = $%d AND ca.asset_id > $%d))))",
		tier, argEnd-2, tier, argEnd-2, argEnd-1, argEnd-1, argEnd)
}

func assetsOrderBy(order AssetsOrder) string {
	// rank_tier leads every order (#356): a directory-flagged issuer's
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

// splitAssetsCursor splits a listing cursor into its rank tier, sort-key
// prefix and asset_id. Two shapes are accepted:
//
//	"<tier>:<sort_key>:<asset_id>" — current (#356): the tier is the
//	                                 LEADING ORDER BY key, so it leads
//	                                 the cursor too.
//	"<sort_key>:<asset_id>"        — legacy, pre-#356. Read as tier 0.
//	                                 Every row a legacy cursor could have
//	                                 been cut from is in tier 0 or later,
//	                                 so an in-flight cursor resumes (it may
//	                                 re-serve a demoted row the client
//	                                 already saw, once) instead of 400ing.
//
// asset_id carries no ':' on this listing — classic ids are CODE-Gxxxx,
// Soroban ids are bare C-strkeys, native is "native" — so counting
// separators is unambiguous. ok=false means "no ':' at all".
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

// AssetPricePoint is one hourly USD-price sample in a price-history
// series. `T` is the bucket end (RFC 3339); `P` is the USD price
// rounded to 10 dp via the same direct-or-XLM-triangulated path
// as price_usd. Pointer P so an hour with no trades comes back
// as null rather than zero.
type AssetPricePoint struct {
	T string
	P *string
}

// GetAssetPriceHistory24h returns up to 24 hourly USD price samples
// for the asset, ordered by bucket ASC (oldest first). Each
// sample uses the same direct-then-XLM-triangulated path as
// price_usd, but bucketed to the 1-hour grain. Powers a sparkline
// column on /assets and a price chart preview on the detail page.
//
// Buckets with no underlying trades produce a null P. Callers can
// either render a gap or interpolate; we leave that to the UI.
func (s *Store) GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	rows, err := s.db.QueryContext(ctx, getAssetPriceHistory24hSQL, assetAliasArray(assetID))
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

// getAssetPriceHistory24hSQL is GetAssetPriceHistory24h's query,
// hoisted to a package constant so the function body stays under the
// funlen threshold (same treatment as getNativeAssetSQL).
const getAssetPriceHistory24hSQL = `
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
		             ORDER BY array_position($1::text[], base_asset), bucket DESC
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
		             ORDER BY inverted, prio, bucket DESC
		           ) AS rn
		      FROM (
		        SELECT date_trunc('hour', bucket) AS h, vwap::numeric AS vwap,
		               array_position($1::text[], base_asset) AS prio,
		               bucket, 0 AS inverted
		          FROM prices_1m
		         WHERE base_asset = ANY($1)
		           AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		           AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		           AND vwap IS NOT NULL
		        UNION ALL
		        SELECT date_trunc('hour', bucket), 1::numeric / vwap,
		               array_position($1::text[], quote_asset),
		               bucket, 1
		          FROM prices_1m
		         WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		           AND quote_asset = ANY($1)
		           AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		           AND vwap > 0
		      ) u
		  ) z WHERE rn = 1
		),
		xlm_usd_per_hour AS (
		  -- Same stablecoin-proxy fallback as the listing query —
		  -- prices_1m doesn't carry (native, fiat:USD) rows.
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		)
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
		    ), 10)::text AS p
		  FROM hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY hours.bucket ASC
`

// GetAssetPriceHistory7d returns up to 7 daily USD price samples
// for the asset, ordered by bucket ASC (oldest first). Same
// direct-then-XLM-triangulated path as GetAssetPriceHistory24h, but
// bucketed to the 1-day grain over the last 7 days.
//
// Buckets with no underlying trades produce a null P.
func (s *Store) GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	rows, err := s.db.QueryContext(ctx, getAssetPriceHistory7dSQL, assetAliasArray(assetID))
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

// getAssetPriceHistory7dSQL is GetAssetPriceHistory7d's query, hoisted
// to a package constant so TestProxyQuoteLists_Lockstep can pin its XLM
// arms alongside getAssetPriceHistory24hSQL.
const getAssetPriceHistory7dSQL = `
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
		             ORDER BY array_position($1::text[], base_asset), bucket DESC
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
		             ORDER BY inverted, prio, bucket DESC
		           ) AS rn
		      FROM (
		        SELECT date_trunc('day', bucket) AS d, vwap::numeric AS vwap,
		               array_position($1::text[], base_asset) AS prio,
		               bucket, 0 AS inverted
		          FROM prices_1m
		         WHERE base_asset = ANY($1)
		           AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		           AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		           AND vwap IS NOT NULL
		        UNION ALL
		        SELECT date_trunc('day', bucket), 1::numeric / vwap,
		               array_position($1::text[], quote_asset),
		               bucket, 1
		          FROM prices_1m
		         WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		           AND quote_asset = ANY($1)
		           AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		           AND vwap > 0
		      ) u
		  ) z WHERE rn = 1
		),
		xlm_usd_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		)
		SELECT
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN 'native' = ANY($1) THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM days
		  LEFT JOIN direct_per_day      d  ON d.d  = days.bucket
		  LEFT JOIN asset_xlm_per_day   x  ON x.d  = days.bucket
		  LEFT JOIN xlm_usd_per_day     xu ON xu.d = days.bucket
		 ORDER BY days.bucket ASC
`

// AssetATH is the asset's all-time-high USD price plus the day
// it was observed. Computed across every USD-quoted day-bucket
// in `prices_1d` (direct USD-stablecoin pairs and `fiat:USD`).
// Triangulated paths (asset/XLM × XLM/USD) are intentionally
// excluded — they introduce two layers of price-discovery
// noise and a single bad XLM/USD reading on a thin day could
// fabricate an ATH.
//
// The metric is "highest day-VWAP" rather than "highest single
// tick" — the day-bucket VWAP is volume-weighted and naturally
// rejects sub-stroop dust prints. The earlier `max(quote/base)`
// definition (R-008 in `docs/review-2026-05-10.md`) put XLM's
// ATH at $1.03 because a single 1-stroop ↔ 1-stroop SDEX dust
// trade pegged the day's max. CoinGecko / CMC use single-tick
// highs across hour buckets that are themselves smoothed; we
// don't have that smoothing layer pre-launch, so day-VWAP is
// the closest dust-resistant approximation.
//
// USD-quote allowlist note (R-008 follow-up, 2026-06-12): the
// `USDT-GCQTGZQQ…` issuer was REMOVED from every USD allowlist —
// there is no Tether on Stellar (the verified catalogue lists no
// stellar network for USDT); that asset trades unpegged (~\-e.09),
// which fabricated an XLM "ATH" of \.78 on thin Jan-2025 days
// (volume_usd=0 dust). USD proxies are the verified USDC issuer +
// fiat:USD only; new proxies require a verified-catalogue entry.
type AssetATH struct {
	USD string // numeric, fixed-point string (preserves precision)
	At  string // RFC-3339 day-bucket the high was set
}

// GetAssetATH returns the asset's all-time-high USD price.
//
// Sources `prices_1d` filtered to USD-denominated quotes — i.e.
// the canonical USDC issuer, plus the synthetic `fiat:USD`
// quote used by off-chain CEX feeds. Returns the (vwap, bucket_day)
// tuple where vwap is maximal.
//
// For native XLM the asset is on the BASE side of every USD pair,
// so the same query works without a special case. Returns
// (nil, nil) cleanly when the asset has never had a USD-quoted
// day with non-null vwap (very thin assets).
func (s *Store) GetAssetATH(ctx context.Context, assetID string) (*AssetATH, error) {
	// Alias-complete: the ATH is the max USD day-VWAP across EVERY
	// canonical form of the asset. Day-VWAP is volume-weighted so a thin
	// SAC pool's dust cannot fabricate a high (see AssetATH docs).
	const q = `
		SELECT
		    vwap::text,
		    to_char(bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d
		 WHERE base_asset = ANY($1)
		   AND quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'fiat:USD'
		   )
		   AND vwap IS NOT NULL
		 ORDER BY vwap DESC
		 LIMIT 1
	`
	var ath AssetATH
	switch err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID)).Scan(&ath.USD, &ath.At); {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("timescale: GetAssetATH: %w", err)
	}
	return &ath, nil
}

// GetAssetsATHBatch returns ATH USD price + day for each asset_id
// in a single round trip. DISTINCT ON picks the (vwap-max,
// bucket) tuple per base_asset; the same USD-quote allowlist as
// the per-asset GetAssetATH and the same dust-resistance rationale
// (see AssetATH docs).
//
// Empty input returns an empty map cleanly. Asset_ids with no
// USD-quoted history are simply absent from the result map.
//
// Powers `?include=ath` on /v1/coins so /assets can show "% from
// ATH" without N+1 round trips.
func (s *Store) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]AssetATH, error) {
	out := make(map[string]AssetATH, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	const q = `
		SELECT DISTINCT ON (base_asset)
		    base_asset,
		    vwap::text,
		    to_char(bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d
		 WHERE base_asset = ANY($1)
		   AND quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'fiat:USD'
		   )
		   AND vwap IS NOT NULL
		 ORDER BY base_asset, vwap DESC
	`
	rows, err := s.db.QueryContext(ctx, q, assetIDs)
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

// GetAssetMarketsCount returns the count of distinct (base, quote)
// pairs the asset participated in over the trailing 24h with a
// non-null prices_1m bucket. Cheaper than GetAssetTopMarkets — no
// volume aggregation, no limit, no ordering. Powers the asset
// detail page's "Markets: 12" header chip.
//
// Returns 0 cleanly when the asset has no rows in the window.
func (s *Store) GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error) {
	const q = `
		SELECT COUNT(*) FROM (
		  SELECT DISTINCT base_asset, quote_asset
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		) t
	`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID)).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: GetAssetMarketsCount: %w", err)
	}
	return n, nil
}

// GetAssetTradeCount24h returns the count of trades the given asset
// participated in (as base OR quote) over the trailing 24 hours.
// Reads the `trades` hypertable directly — accurate down to the
// individual trade rather than the prices_1m bucket aggregation
// used by GetAssetMarketsCount.
//
// Powers the asset detail page header — `observation_count` shows
// the all-time figure, this gives the "what's happening right now"
// counterpart so a user can tell at a glance whether the asset is
// active or a long tail entry.
//
// Returns 0 cleanly when the asset has no trades in the window.
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

// GetAssetTopMarkets returns up to `limit` markets the given asset
// participates in (as base OR quote), ordered by trailing-24h USD
// volume desc. Used by the explorer asset-detail page to show a
// "Top markets" preview without a separate /v1/markets call.
//
// limit clamps to [1, 20]; default 5.
func (s *Store) GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]AssetTopMarket, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	// Alias-complete: a market where ANY canonical form of the asset is
	// base or quote counts, and the counterparty/side CASE tests
	// membership so a pair keyed by the asset's crypto:XLM or SAC form is
	// labelled as the asset's own market (side base/quote) rather than
	// being mislabelled as a counterparty.
	const q = `
		WITH per_pair_24h AS (
		  SELECT base_asset, quote_asset,
		         SUM(volume_usd)::text AS vol_usd
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND volume_usd IS NOT NULL
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		   GROUP BY base_asset, quote_asset
		),
		per_pair_count AS (
		  SELECT base_asset, quote_asset,
		         COUNT(*) FILTER (WHERE ts > now() - INTERVAL '24 hours') AS n
		    FROM trades
		   WHERE ts >= now() - INTERVAL '24 hours'
		     AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		   GROUP BY base_asset, quote_asset
		)
		SELECT
		    CASE WHEN p.base_asset = ANY($1) THEN p.quote_asset ELSE p.base_asset END AS counterparty,
		    CASE WHEN p.base_asset = ANY($1) THEN 'base' ELSE 'quote' END             AS side,
		    p.vol_usd                                                                 AS vol_24h_usd,
		    COALESCE(c.n, 0)                                                          AS n_24h
		  FROM per_pair_24h p
		  LEFT JOIN per_pair_count c
		    ON c.base_asset = p.base_asset AND c.quote_asset = p.quote_asset
		 ORDER BY p.vol_usd::numeric DESC NULLS LAST
		 LIMIT $2
	`
	rows, err := s.db.QueryContext(ctx, q, assetAliasArray(assetID), limit)
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

// GetAssetBySlug returns one row matching the given slug. Returns
// sql.ErrNoRows when the slug doesn't match a known classic asset or
// a traded Soroban-native contract (the same discovered_assets arm the
// listing spine admits).
//
// Mirrors ListAssets's per-row metric shape (price/volume/market cap/
// supply) so the explorer can render an asset detail page from a
// single endpoint without scanning the top-N listing first.
// getAssetBySlugSQL is the SQL behind GetAssetBySlug. Hoisted out
// of the function body to keep GetAssetBySlug under the funlen
// threshold; the helpers above already document the chosen-CTE
// pattern that keeps the volume sum and price triangulation on
// the same canonical row.
const getAssetBySlugSQL = `
		WITH chosen AS (
		  -- Three input shapes accepted (in tiebreak preference):
		  --   1. friendly slug (USDC, AQUA, EURC)        — slug column
		  --   2. raw code without issuer (USDC, AQUA)    — code column
		  --   3. canonical asset_id (USDC-GA5Z…)         — asset_id column
		  -- The OR-WHERE catches all three; the ORDER BY tiebreaks in
		  -- preference order so a slug input wins over a code-only
		  -- collision (the disambiguation guard from #45 / scam-token
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
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = (SELECT asset_id FROM chosen)
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		    ) t
		),
		direct_usd AS (
		  -- Quote set = fiat:USD OR USDC — classic G-issuer AND
		  -- its SAC contract (CCW67T…) — per the stablecoin-proxy
		  -- policy (see the listing query's direct_usd CTE + the
		  -- xlm_usd CTEs below): a USDC-quoted vwap is taken AS
		  -- the USD price (~0.1% peg error accepted). ORDER BY
		  -- bucket DESC keeps the freshest row across all quotes.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_1h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_24h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_7d AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		asset_vs_xlm AS (
		  -- XLM leg in BOTH identity forms — 'native' AND its SAC
		  -- (CAS3J7… = canonical.XLMSacContractID) — and in BOTH
		  -- stored directions, base-side preferred; see the listing
		  -- query's asset_vs_xlm CTE for the rationale.
		  SELECT vwap FROM (
		    SELECT vwap, bucket, 0 AS inverted FROM prices_1m
		     WHERE base_asset  = (SELECT asset_id FROM chosen)
		       AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket >= now() - INTERVAL '7 days'
		       AND vwap IS NOT NULL
		    UNION ALL
		    SELECT 1::numeric / vwap, bucket, 1 FROM prices_1m
		     WHERE quote_asset = (SELECT asset_id FROM chosen)
		       AND base_asset  IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket >= now() - INTERVAL '7 days'
		       AND vwap > 0
		  ) u
		   ORDER BY inverted, bucket DESC LIMIT 1
		),
		asset_vs_xlm_1h AS (
		  SELECT vwap FROM (
		    SELECT vwap, bucket, 0 AS inverted FROM prices_1m
		     WHERE base_asset  = (SELECT asset_id FROM chosen)
		       AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		       AND vwap IS NOT NULL
		    UNION ALL
		    SELECT 1::numeric / vwap, bucket, 1 FROM prices_1m
		     WHERE quote_asset = (SELECT asset_id FROM chosen)
		       AND base_asset  IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		       AND vwap > 0
		  ) u
		   ORDER BY inverted, bucket DESC LIMIT 1
		),
		asset_vs_xlm_24h AS (
		  SELECT vwap FROM (
		    SELECT vwap, bucket, 0 AS inverted FROM prices_1m
		     WHERE base_asset  = (SELECT asset_id FROM chosen)
		       AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		       AND vwap IS NOT NULL
		    UNION ALL
		    SELECT 1::numeric / vwap, bucket, 1 FROM prices_1m
		     WHERE quote_asset = (SELECT asset_id FROM chosen)
		       AND base_asset  IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		       AND vwap > 0
		  ) u
		   ORDER BY inverted, bucket DESC LIMIT 1
		),
		asset_vs_xlm_7d AS (
		  SELECT vwap FROM (
		    SELECT vwap, bucket, 0 AS inverted FROM prices_1m
		     WHERE base_asset  = (SELECT asset_id FROM chosen)
		       AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		       AND vwap IS NOT NULL
		    UNION ALL
		    SELECT 1::numeric / vwap, bucket, 1 FROM prices_1m
		     WHERE quote_asset = (SELECT asset_id FROM chosen)
		       AND base_asset  IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		       AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		       AND vwap > 0
		  ) u
		   ORDER BY inverted, bucket DESC LIMIT 1
		),
		xlm_usd AS (
		  -- Same stablecoin-proxy policy as the listing query:
		  -- prices_1m doesn't carry (native, fiat:USD) rows; use
		  -- on-chain XLM/USDC as the USD-equivalent.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_1h AS (
		  -- 1h-ago XLM/USD for change_1h_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_24h AS (
		  -- 24h-ago XLM/USD for change_24h_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_7d AS (
		  -- 7d-ago XLM/USD for change_7d_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
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
		    ca.asset_id, ca.code, ca.issuer_g_strkey,
		    ca.first_seen_ledger, ca.last_seen_ledger, ca.observation_count,
		    -- XLM (asset_id='native') has no rows in direct_usd or
		    -- asset_vs_xlm — its base_asset is 'native' but neither
		    -- (native, fiat:USD) nor (native, native) exists in
		    -- prices_1m. Use the xlm_usd CTE directly.
		    ROUND(COALESCE(
		      CASE WHEN ca.asset_id = 'native'
		           THEN (SELECT vwap FROM xlm_usd)
		           ELSE NULL
		      END,
		      (SELECT vwap FROM direct_usd),
		      (SELECT vwap FROM asset_vs_xlm) * (SELECT vwap FROM xlm_usd)
		    ), 10)::text                          AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_1h) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_1h) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)    * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_1h) * (SELECT vwap FROM xlm_usd_1h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_1h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_24h) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_24h) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)     * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_24h) * (SELECT vwap FROM xlm_usd_24h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_24h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_7d) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_7d) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)    * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_7d) * (SELECT vwap FROM xlm_usd_7d))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_7d_pct,
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
		    NULL::int                             AS rank_tier
		  FROM chosen ca
		  LEFT JOIN per_asset_24h_vol vol ON true
`

// GetAssetBySlug looks up by friendly slug (USDC, AQUA, EURC),
// canonical asset_id (USDC-GA5Z…), OR raw code with case-insensitive
// retry — see the SQL's WHERE clause + the handler's case-fallback
// for the full input-shape table.
func (s *Store) GetAssetBySlug(ctx context.Context, slug string) (AssetRow, error) {
	r, err := scanAssetRow(s.db.QueryRowContext(ctx, getAssetBySlugSQL, slug))
	if err != nil {
		// Surface sql.ErrNoRows unwrapped so handler errors.Is checks
		// keep matching; scanAssetRow wraps with %w which preserves it.
		return AssetRow{}, err
	}
	return r, nil
}

// GetAssetByAssetID is a thin wrapper kept for clarity at the
// handler layer — the underlying SQL accepts canonical asset_id
// alongside friendly slug since 2026-05-10, so this just calls
// GetAssetBySlug. (Pre-fix the canonical form 404'd; see the
// alerts-catalog row "Asset canonical asset_id 404" for context.)
func (s *Store) GetAssetByAssetID(ctx context.Context, assetID string) (AssetRow, error) {
	return s.GetAssetBySlug(ctx, assetID)
}

// GetNativeAssetRow returns the synthetic AssetRow for native XLM.
//
// Native XLM has no row in classic_assets — that table only tracks
// issued classic assets, by definition. Without a special-case path
// the public lookup `/v1/coins/XLM` either 404s (no slug match) or
// returns whichever issued token's code happens to be "XLM" wins
// the disambiguation tiebreak (today: a scam token issued by
// GAE5PQNUIP5E…).
//
// Population:
//   - Slug / Code: hardcoded "XLM"
//   - AssetID: "native" (the canonical pair-side identifier)
//   - IssuerGStrkey: "" (native has no issuer)
//   - First/Last seen ledger: the trades hypertable's min/max for
//     base_asset='native' OR quote_asset='native'
//   - ObservationCount: total trades touching native in the
//     hypertable, capped at int64
//   - PriceUSD + Change*Pct: same xlm_usd / xlm_usd_{1h,24h,7d}
//     stablecoin-proxy chain used by GetAssetBySlug + the listing
//     query for non-native assets
//   - Volume24hUSD: SUM(volume_usd) where the asset is base or quote
//     in the trailing 24h
//   - MarketCapUSD / CirculatingSupply: NULL — supply pipeline
//     doesn't yet emit a row for native (algorithm 1 work)
//
// Always returns a populated row (no sql.ErrNoRows path) — the
// underlying CTEs LEFT JOIN out to NULL when there's no data.
func (s *Store) GetNativeAssetRow(ctx context.Context) (AssetRow, error) {
	return scanAssetRow(s.db.QueryRowContext(ctx, getNativeAssetSQL))
}

// getNativeAssetSQL is GetNativeAssetRow's query, hoisted to a
// package constant so the function body stays under the funlen
// threshold. Returns the same column shape as listAssetsBaseSelect
// + getAssetBySlugSQL — the shared scanAssetRow projector handles
// it identically.
const getNativeAssetSQL = `
		WITH per_asset_24h_vol AS (
		  SELECT SUM(volume_usd) AS vol_usd
		    FROM (
		      SELECT volume_usd FROM prices_1m
		       WHERE base_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		    ) t
		),
		xlm_usd AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_1h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_24h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_7d AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		ledger_bounds AS (
		  -- Always return one row with placeholder zeros — the
		  -- previous version scanned the trades hypertable for
		  -- 7 days OF native-touching rows, which is millions of
		  -- rows on a busy ledger and timed out under lock-table
		  -- pressure (53200). Native XLM is well-known; the
		  -- explorer doesn't need an accurate first_seen_ledger
		  -- for it. observation_count uses prices_1m row count
		  -- as a cheap proxy.
		  SELECT
		    0::bigint AS first_ledger,
		    0::bigint AS last_ledger,
		    COALESCE(
		      (SELECT COUNT(*)::bigint
		         FROM prices_1m
		        WHERE bucket >= now() - INTERVAL '24 hours'
		          AND (base_asset = 'native' OR quote_asset = 'native')),
		      0
		    ) AS obs_count
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
		    NULL::int                              AS rank_tier
		  FROM ledger_bounds lb
		  LEFT JOIN per_asset_24h_vol vol ON true
`

// LatestAssetStats returns per-asset 24h volume + supply stats
// for /v1/assets/{id}. Volume sums prices_1m.volume_usd across
// pairs where the asset is base or quote (mirrors
// Volume24hUSDForAsset). Supply is null for now — the intended
// source table classic_asset_stats_5m never got a writer and was
// dropped by migration 0152 (#358).
//
// Always returns nil error for a row that simply has no stats;
// the LEFT JOINs evaluate to NULL.
func (s *Store) LatestAssetStats(ctx context.Context, assetID string) (AssetRow, error) {
	// Alias-complete: sum the asset's volume across all its canonical
	// forms (mirrors Volume24hUSDForAsset), so the /v1/assets/{id} volume
	// figure includes the crypto:XLM (CEX) and SAC (Soroban) legs.
	const q = `
		SELECT COALESCE(SUM(volume_usd), 0)::text
		  FROM (
		    SELECT volume_usd FROM prices_1m
		     WHERE base_asset = ANY($1)
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket  <  now()
		    UNION ALL
		    SELECT volume_usd FROM prices_1m
		     WHERE quote_asset = ANY($1)
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket  <  now()
		  ) t
	`
	var vol string
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetID)).Scan(&vol); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: LatestAssetStats: %w", err)
	}
	out := AssetRow{AssetID: assetID}
	if vol != "" && vol != "0" {
		out.Volume24hUSD = &vol
	}
	return out, nil
}

// ValidateAssetsCursor returns an error if `cursor` is non-empty
// but doesn't match the encoded shape the listing emits for the
// active order. Empty cursor is always valid (resume from the
// first page). Callers should reject invalid cursors at the
// handler boundary with a 400 — falling through silently
// truncates the keyset predicate to "(0, \"\")" / "(0, \"\")"
// which matches no rows under the default order, returning an
// empty page that looks like end-of-pagination.
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
	// Rank tier — the leading ORDER BY key (#356). Synthesised as "0" for
	// a legacy two-field cursor, so this only rejects a genuinely
	// malformed three-field one.
	if !isDigitString(tier) {
		return fmt.Errorf("non-numeric rank-tier prefix")
	}
	// The tier is bound to a `$n::int` placeholder, so it must fit int32.
	// isDigitString alone accepted "2147483648", which then went through
	// atoiOrZero (plain int, 64-bit here) into an int4 bind and errored at
	// the database instead of at the boundary (wave-D KP-2).
	//
	// Bounded by the type rather than by a hard `> 2` reject: the tier
	// cardinality is order-dependent (listingRankTierExpr emits a
	// different set per order), so pinning a literal maximum here would
	// add a fourth site to the three-call-site lockstep contract.
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
	// Must fit the int64 the keyset predicate binds. An over-int64 run of
	// digits passed this check, then parseAssetCursor's ParseInt failed
	// and degenerated the whole cursor to (0, 0, "") — which matches no
	// rows under the default order, so the client received a silent 200
	// with an empty page that is indistinguishable from
	// end-of-pagination (wave-D KP-2). A malformed cursor must 400, not
	// quietly claim the walk is over.
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

// isNumericPrefix returns true for a digit string with at most one '.'
// separator. Negative volumes don't exist in our data, so we don't
// accept a leading '-'.
//
// At least ONE digit is required. Without that check the loop accepted
// "." — a lone dot sets dot=true, the loop ends, and it returned true —
// so a cursor whose volume prefix is "." passed validation and reached
// the keyset predicate as a $n::numeric bind, which Postgres rejects
// (wave-D KP-2). The empty prefix is still valid and is handled by the
// caller: it encodes "the last row had a NULL vol_usd".
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

// parseAssetCursor decodes a `<tier>:<obs_count>:<asset_id>` cursor.
// Empty cursor → (0, 0, "") which means "no cursor". Malformed
// cursors fall through to the same; the handler is responsible
// for rejecting them via ValidateAssetsCursor before this is
// reached.
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

// EncodeAssetsCursor renders the keyset cursor for the LAST row of a
// page under `order`: `<rank_tier>:<sort_key>:<asset_id>` — the full
// ORDER BY tuple, leading key first, exactly the values the query ranked
// this row on. Pairs with [parseAssetCursor] / [parseVolumeCursor].
//
// It lives here rather than in each handler's fmt.Sprintf so the encode
// side cannot drift from assetsOrderBy + assetsCursorPredicate; the two
// /v1/assets paths (the observation-count listing and the unified
// listing's classic phase) both call it.
//
// RankTier nil (a row that did not come from the listing query) encodes
// as tier 0 — the same tier a legacy cursor resumes in.
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

// GetAssetsPriceHistory24hBatch returns 24h hourly USD-price series
// for many assets in one query. Result is keyed by asset_id; the
// per-asset slice has up to 24 ordered points (oldest → newest).
// Assets with no trade history in the window get an empty slice
// (callers can render that as "no chart").
//
// Same direct-then-XLM-triangulated path as the single-asset
// GetAssetPriceHistory24h; just a single CTE pass over all
// requested assets at once.
func (s *Store) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	rows, err := s.db.QueryContext(ctx, getAssetsPriceHistory24hBatchSQL, assetIDs)
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
const getAssetsPriceHistory24hBatchSQL = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		direct_per_hour AS (
		  -- Quote set = fiat:USD OR USDC (classic or its SAC form)
		  -- per the stablecoin-proxy policy (see the listing
		  -- query's direct_usd CTE).
		  SELECT base_asset AS asset_id,
		         date_trunc('hour', bucket) AS h,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, h
		),
		asset_xlm_per_hour AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and BOTH
		  -- stored directions, base-side preferred per (asset, hour);
		  -- see GetAssetPriceHistory24h's asset_xlm_per_hour. The
		  -- latest-bucket pick per arm is what last(vwap, bucket) was.
		  SELECT DISTINCT ON (asset_id, h) asset_id, h, vwap
		    FROM (
		      SELECT base_asset AS asset_id,
		             date_trunc('hour', bucket) AS h,
		             vwap::numeric AS vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset,
		             date_trunc('hour', bucket),
		             1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND quote_asset = ANY($1)
		         AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, h, inverted, bucket DESC
		),
		xlm_usd_per_hour AS (
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		),
		want AS (
		  SELECT unnest($1::text[]) AS asset_id
		)
		SELECT
		    w.asset_id,
		    to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.asset_id = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM want w
		  CROSS JOIN hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY w.asset_id, hours.bucket ASC
`

// GetAssetsPriceHistory7dBatch returns 7d daily USD-price series for
// many assets in one query. 7-bucket-deep daily grain, mirroring
// the per-asset GetAssetPriceHistory7d. Same direct-then-XLM-
// triangulated path; one query for many asset_ids.
func (s *Store) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	rows, err := s.db.QueryContext(ctx, getAssetsPriceHistory7dBatchSQL, assetIDs)
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
const getAssetsPriceHistory7dBatchSQL = `
		WITH days AS (
		  SELECT generate_series(
		    date_trunc('day', now() - INTERVAL '6 days'),
		    date_trunc('day', now()),
		    INTERVAL '1 day'
		  ) AS bucket
		),
		direct_per_day AS (
		  -- Quote set = fiat:USD OR USDC (classic or its SAC form)
		  -- per the stablecoin-proxy policy (see the listing
		  -- query's direct_usd CTE).
		  SELECT base_asset AS asset_id,
		         date_trunc('day', bucket) AS d,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, d
		),
		asset_xlm_per_day AS (
		  -- XLM leg in BOTH identity forms ('native' + SAC) and BOTH
		  -- stored directions, base-side preferred per (asset, day);
		  -- see GetAssetPriceHistory24h's asset_xlm_per_hour. The
		  -- latest-bucket pick per arm is what last(vwap, bucket) was.
		  SELECT DISTINCT ON (asset_id, d) asset_id, d, vwap
		    FROM (
		      SELECT base_asset AS asset_id,
		             date_trunc('day', bucket) AS d,
		             vwap::numeric AS vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE base_asset = ANY($1)
		         AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset,
		             date_trunc('day', bucket),
		             1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND quote_asset = ANY($1)
		         AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, d, inverted, bucket DESC
		),
		xlm_usd_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		),
		want AS (
		  SELECT unnest($1::text[]) AS asset_id
		)
		SELECT
		    w.asset_id,
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.asset_id = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM want w
		  CROSS JOIN days
		  LEFT JOIN direct_per_day     d  ON d.d  = days.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_day  x  ON x.d  = days.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_day    xu ON xu.d = days.bucket
		 ORDER BY w.asset_id, days.bucket ASC
`
