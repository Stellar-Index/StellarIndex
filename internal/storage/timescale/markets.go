package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Market is one distinct (base, quote) pair summary returned by [Store.DistinctPairs]; Volume24hUSD is from prices_1m, or from pools_per_source_1h for a source-filtered listing so it is that venue's own contribution.
type Market struct {
	Pair          canonical.Pair
	LastTradeAt   time.Time
	BucketCloseAt time.Time
	TradeCount24h int64
	Volume24hUSD  *string
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time
	// usd_volume. Computed only for the per-source listing.
	VolumeLowerBound bool
	// LastPrice is the last quote-per-base price: the newest prices_1m close within 24h, else the
	// newest prices_1d close in the recency window. Under a source filter it is that VENUE's own
	// price, from pools_per_source_1h. nil when no bucket has a non-null `last_price`. It is a
	// numeric string for precision.
	LastPrice *string
}

// Pool is one (source, base, quote) tuple — same shape as Market but with the source dimension preserved.
type Pool struct {
	Source        string
	Pair          canonical.Pair
	LastTradeAt   time.Time
	TradeCount24h int64
	Volume24hUSD  *string
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time usd_volume.
	VolumeLowerBound bool
	// LastPrice is the last quote-per-base price for this
	// (source, base, quote) tuple — same wire shape as
	// Market.LastPrice but per-pool.
	LastPrice *string
}

// MarketsRecencyWindow bounds the trades scanned by DistinctPairs, so idle pairs drop out of `/v1/markets` ("active markets"); a var so tests can override it.
var MarketsRecencyWindow = 14 * 24 * time.Hour

// MarketsOrder controls the sort and cursor scheme used by DistinctPairs.
type MarketsOrder int

const (
	// MarketsOrderPair orders by (base|quote) ASC; the cursor is that string.
	MarketsOrderPair MarketsOrder = iota
	// MarketsOrderVolume24hDesc orders by volume_24h_usd DESC NULLS LAST, then (base|quote) ASC;
	// the cursor is `<vol_or_blank>:<base|quote>`.
	MarketsOrderVolume24hDesc
)

// DistinctPairs returns one page of recently-active (base, quote) pairs with last trade time, 24h count and 24h USD volume; limit clamps to [1, 500] and only chunks inside MarketsRecencyWindow are scanned.
func (s *Store) DistinctPairs(ctx context.Context, cursor string, limit int) ([]Market, string, error) {
	return s.DistinctPairsExt(ctx, cursor, limit, MarketsOrderPair)
}

// DistinctPairsExt is DistinctPairs with explicit ordering control.
func (s *Store) DistinctPairsExt(ctx context.Context, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.distinctPairsCommon(ctx, "", "", cursor, limit, order)
}

// SourceMarkets returns one page of pairs the source observed in the trailing MarketsRecencyWindow with that venue's own 24h figures from the per-source CAGG, bypassing distinctPairsCommon since the pair-wide CAGGs lack a source grain.
func (s *Store) SourceMarkets(ctx context.Context, source, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.sourceMarketsCommon(ctx, source, cursor, limit, order)
}

// AssetMarkets returns one page of pairs with the canonical asset_id on either side, backing `/v1/markets?asset=` without a global scan.
func (s *Store) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.distinctPairsCommon(ctx, "", asset, cursor, limit, order)
}

// PoolsFilter narrows AllPools by venue and/or pair; zero-value fields mean no filter.
type PoolsFilter struct {
	Sources []string
	Base    string
	Quote   string
	Asset   string
}

// AllPools returns every (source, base, quote) tuple observed in the trailing MarketsRecencyWindow for /v1/pools, not collapsed across sources; the cursor shape depends on the order (volume-desc vs pair-asc).
func (s *Store) AllPools(ctx context.Context, filter PoolsFilter, cursor string, limit int, order MarketsOrder) ([]Pool, string, error) { //nolint:gocognit // limit clamp + order branch + scan loop are linear; splitting would scatter the request lifecycle
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	q, args := buildPoolsQuery(since, filter, cursor, limit, order)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("timescale: AllPools: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Pool, 0, limit)
	hasMore := false
	n := 0
	for rows.Next() {
		var (
			source            string
			baseRaw, quoteRaw string
			lastAt            time.Time
			count24h          int64
			vol24hUSD         sql.NullString
			lastPrice         sql.NullString
			lowerBound        bool
		)
		if err := rows.Scan(&source, &baseRaw, &quoteRaw, &lastAt, &count24h, &vol24hUSD, &lastPrice, &lowerBound); err != nil {
			return nil, "", fmt.Errorf("timescale: AllPools scan: %w", err)
		}
		n++
		if n > limit {
			hasMore = true
			break
		}
		base, err := canonical.ParseAsset(baseRaw)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: AllPools base %q: %w", baseRaw, err)
		}
		quote, err := canonical.ParseAsset(quoteRaw)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: AllPools quote %q: %w", quoteRaw, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: AllPools pair: %w", err)
		}
		p := Pool{
			Source:           source,
			Pair:             pair,
			LastTradeAt:      lastAt.UTC(),
			TradeCount24h:    count24h,
			VolumeLowerBound: lowerBound,
		}
		if vol24hUSD.Valid && vol24hUSD.String != "" && vol24hUSD.String != "0" {
			v := vol24hUSD.String
			p.Volume24hUSD = &v
		}
		if lastPrice.Valid && lastPrice.String != "" {
			v := lastPrice.String
			p.LastPrice = &v
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("timescale: AllPools rows: %w", err)
	}
	nextCursor := ""
	if hasMore && len(out) > 0 {
		last := out[len(out)-1]
		key := last.Source + "|" + last.Pair.Base.String() + "|" + last.Pair.Quote.String()
		if order == MarketsOrderVolume24hDesc {
			vol := ""
			if last.Volume24hUSD != nil {
				vol = *last.Volume24hUSD
			}
			nextCursor = vol + ":" + key
		} else {
			nextCursor = key
		}
	}
	return out, nextCursor, nil
}

// perSourcePoolsCTE scans pools_per_source_1h per (source, base, quote), shared by /v1/pools and source-filtered /v1/markets so they
// agree; pair-wide prices_1m/1d select buckets a venue printed in, not its contribution. $1 is the recency lower bound and
// sacAliasFoldBind's arrays sit at $foldIdx, $foldIdx+1. Rows are alias-folded BEFORE grouping. vol_24h_usd sums trade-time usd_volume
// only (vol_lower_bound says so). rawFilter is ANDed onto the raw rows before the fold joins.
func perSourcePoolsCTE(foldIdx int, rawFilter string) string {
	return `
        WITH ` + aliasFoldCTE(foldIdx) + `,
        pools AS (
          SELECT
            p.source, p.base_asset, p.quote_asset,
            MAX(p.bucket_last_ts) AS last_trade_at,
            COALESCE(SUM(p.trade_count)
                     FILTER (WHERE p.bucket >= NOW() - INTERVAL '24 hours'), 0) AS count_24h,
            COALESCE(SUM(p.sum_usd_priced)
                     FILTER (WHERE p.bucket >= NOW() - INTERVAL '24 hours'), 0)::text AS vol_24h_usd,
            -- sum_base_unpriced is NULL exactly when the bucket held no unpriced trade.
            COALESCE(bool_or(p.sum_base_unpriced IS NOT NULL)
                     FILTER (WHERE p.bucket >= NOW() - INTERVAL '24 hours'), false) AS vol_lower_bound,
            -- Two spellings folded into one market can close a bucket in the same ledger, so their
            -- timestamps tie; the raw spelling breaks it, or last() would pick either per query.
            (array_agg(p.bucket_last_price ORDER BY p.bucket_last_ts DESC,
                       p.raw_base COLLATE "C", p.raw_quote COLLATE "C"))[1]::text AS last_price
          FROM (
            SELECT r.source, r.bucket, r.bucket_last_ts, r.bucket_last_price,
                   r.trade_count, r.sum_usd_priced, r.sum_base_unpriced,
                   r.base_asset AS raw_base, r.quote_asset AS raw_quote,
                   COALESCE(bf.canon, r.base_asset)  AS base_asset,
                   COALESCE(qf.canon, r.quote_asset) AS quote_asset
              FROM pools_per_source_1h r
              LEFT JOIN alias_fold bf ON bf.form = r.base_asset
              LEFT JOIN alias_fold qf ON qf.form = r.quote_asset
             -- A wrap/unwrap (native vs its SAC) folds to a self-pair, which is no market.
             WHERE COALESCE(bf.canon, r.base_asset) <> COALESCE(qf.canon, r.quote_asset)` + rawFilter + `
          ) p
         WHERE p.bucket >= $1
    `
}

// poolsFilterSQL renders the /v1/pools predicates over raw pools_per_source_1h rows; $4 sources and $5 base / $6 quote / $7 asset alias
// arrays are always bound (empty disables the predicate). `canon` folds orientation afterwards, so each predicate must be invariant under
// it: base+quote matches either stored orientation; base or quote alone matches that side of the CANONICAL one; asset matches either leg.
func poolsFilterSQL(canonBase, canonQuote string) string {
	return `
           AND (cardinality($4::text[]) = 0 OR p.source = ANY($4))
           AND (cardinality($5::text[]) = 0 OR cardinality($6::text[]) = 0
                OR (p.base_asset = ANY($5) AND p.quote_asset = ANY($6))
                OR (p.base_asset = ANY($6) AND p.quote_asset = ANY($5)))
           AND (cardinality($5::text[]) = 0 OR cardinality($6::text[]) > 0
                OR ` + canonBase + ` = ANY($5))
           AND (cardinality($6::text[]) = 0 OR cardinality($5::text[]) > 0
                OR ` + canonQuote + ` = ANY($6))
           AND (cardinality($7::text[]) = 0 OR p.base_asset = ANY($7) OR p.quote_asset = ANY($7))`
}

// poolsRawPrefilterSQL narrows the raw rows to those with a leg in each asset array, so the fold joins and sort skip the rest
// (988k of 1.25M rows for asset=XLM). Exact: an alias family is closed, so a folded leg in an array means its raw spelling is too;
// poolsFilterSQL still applies the precise predicates.
const poolsRawPrefilterSQL = `
               AND (cardinality($5::text[]) = 0 OR r.base_asset = ANY($5) OR r.quote_asset = ANY($5))
               AND (cardinality($6::text[]) = 0 OR r.base_asset = ANY($6) OR r.quote_asset = ANY($6))
               AND (cardinality($7::text[]) = 0 OR r.base_asset = ANY($7) OR r.quote_asset = ANY($7))`

// poolsFilterArgs binds $4..$7 for poolsFilterSQL.
func poolsFilterArgs(filter PoolsFilter) []any {
	sources := filter.Sources
	if sources == nil {
		// Same NULL trap as assetAliasBind: nil binds NULL and $4 would exclude every row.
		sources = []string{}
	}
	return []any{sources, assetAliasBind(filter.Base), assetAliasBind(filter.Quote), assetAliasBind(filter.Asset)}
}

func buildPoolsQuery(since time.Time, filter PoolsFilter, cursor string, limit int, order MarketsOrder) (string, []any) { //nolint:funlen // CTE + select + 2 ordering branches form one query template; splitting would scatter the SQL across helpers
	// One scan of the pools_per_source_1h CAGG replaces three passes over the trades hypertable
	// (8-30s on 2.7B rows); the `pools` CTE re-aggregates its hourly rows over the window, with
	// FILTER clauses taking the 24h slice. last_trade_at lags by up to one refresh interval
	// (5 min), acceptable for a discovery surface. $8/$9 are the alias-fold arrays, after $1..$7.
	cte := perSourcePoolsCTE(8, poolsRawPrefilterSQL)
	canonBase, canonQuote, flipped := canonOrientSQL(10)
	cte += poolsFilterSQL(canonBase, canonQuote) + `
         GROUP BY p.source, p.base_asset, p.quote_asset
        )
    `
	// canon collapses flipped orientations of a market within a source (XLM/USDC + USDC/XLM): volume
	// and count sum across directions, last_price is the latest trade re-expressed canonically,
	// and either direction's excluded trades make the volume a lower bound. See canonical.Orient.
	cte += `,
        canon AS (
          SELECT source,
                 ` + canonBase + ` AS base_asset,
                 ` + canonQuote + ` AS quote_asset,
                 MAX(last_trade_at)             AS last_trade_at,
                 SUM(count_24h)                 AS count_24h,
                 SUM(vol_24h_usd::numeric)::text AS vol_24h_usd,
                 ` + canonLastPriceSQL(flipped) + ` AS last_price,
                 bool_or(vol_lower_bound)       AS vol_lower_bound
            FROM pools
           GROUP BY source, ` + canonBase + `, ` + canonQuote + `
        )
    `
	if order == MarketsOrderVolume24hDesc {
		const tail = `
		 SELECT source, base_asset, quote_asset, last_trade_at, count_24h, vol_24h_usd, last_price, vol_lower_bound
		   FROM canon
		  WHERE $2 = ''
		     OR COALESCE(vol_24h_usd::numeric, 0)
		          <  CAST(NULLIF(split_part($2, ':', 1), '') AS numeric)
		     OR (
		          COALESCE(vol_24h_usd::numeric, 0)
		          =  CAST(COALESCE(NULLIF(split_part($2, ':', 1), ''), '0') AS numeric)
		          AND (source || '|' || base_asset || '|' || quote_asset)
		              > substring($2 from position(':' in $2) + 1)
		        )
		  ORDER BY COALESCE(vol_24h_usd::numeric, 0) DESC,
		           (source || '|' || base_asset || '|' || quote_asset) ASC
		  LIMIT $3
		`
		return cte + tail, poolsQueryArgs(since, filter, cursor, limit)
	}
	// FROM canon, NOT FROM pools, like the volume-desc tail above: the
	// pre-collapse CTE returns both orientations of a two-sided market
	// as separate rows, each with only its own direction's volume.
	const tail = `
	 SELECT source, base_asset, quote_asset, last_trade_at, count_24h, vol_24h_usd, last_price, vol_lower_bound
	   FROM canon
	  WHERE ($2 = '' OR (source || '|' || base_asset || '|' || quote_asset) > $2)
	  ORDER BY (source || '|' || base_asset || '|' || quote_asset) ASC
	  LIMIT $3
	`
	// Both tails bind poolsQueryArgs so their parameter counts cannot drift.
	return cte + tail, poolsQueryArgs(since, filter, cursor, limit)
}

// poolsQueryArgs binds $1..$10 for buildPoolsQuery; $10 is the native-XLM SAC.
func poolsQueryArgs(since time.Time, filter PoolsFilter, cursor string, limit int) []any {
	forms, canons := sacAliasFoldBind()
	args := append([]any{since, cursor, limit + 1}, poolsFilterArgs(filter)...)
	return append(args, forms, canons, canonical.NativeSACContractID())
}

// assetAliasBind expands an asset filter to its alias forms for `= ANY($n)`. Empty binds a non-nil empty array: nil is SQL NULL and
// cardinality(NULL) is NULL, which would exclude every row. An unparseable asset binds as itself.
func assetAliasBind(asset string) []string {
	if asset == "" {
		return []string{}
	}
	a, err := canonical.ParseAsset(asset)
	if err != nil {
		return []string{asset}
	}
	return canonical.AssetAliasStrings(a)
}

// sacAliasFoldBind binds the alias_fold (form, canon) arrays mapping SAC spellings to their classic/native form; crypto:XLM stays unfolded
// (CEX population). Both slices are non-nil, see assetAliasBind.
func sacAliasFoldBind() (forms, canons []string) {
	all := canonical.AllAliasForms()
	forms, canons = []string{}, []string{}
	for form := range all {
		if a, err := canonical.ParseAsset(form); err == nil && a.Type == canonical.AssetSoroban {
			forms = append(forms, form)
		}
	}
	sort.Strings(forms)
	for _, f := range forms {
		canons = append(canons, all[f])
	}
	return forms, canons
}

// sacFoldMaps returns sacAliasFoldBind as a form→canon map and its
// inverse, canon→SAC forms.
func sacFoldMaps() (fold map[string]string, spellings map[string][]string) {
	forms, canons := sacAliasFoldBind()
	fold = make(map[string]string, len(forms))
	spellings = map[string][]string{}
	for i, f := range forms {
		fold[f] = canons[i]
		spellings[canons[i]] = append(spellings[canons[i]], f)
	}
	return fold, spellings
}

func foldSpelling(fold map[string]string, asset string) string {
	if c, ok := fold[asset]; ok {
		return c
	}
	return asset
}

// expandSACSpellings lists, per requested pair, every stored (base, quote) spelling folded into it, keyed by "<base>|<quote>".
func expandSACSpellings(pairs [][2]string, spellings map[string][]string) (keys, bases, quotes []string) {
	for _, p := range pairs {
		key := p[0] + "|" + p[1]
		for _, b := range append([]string{p[0]}, spellings[p[0]]...) {
			for _, q := range append([]string{p[1]}, spellings[p[1]]...) {
				keys = append(keys, key)
				bases = append(bases, b)
				quotes = append(quotes, q)
			}
		}
	}
	return keys, bases, quotes
}

// aliasFoldCTE declares alias_fold over the two sacAliasFoldBind arrays
// bound at $formsIdx and $formsIdx+1.
func aliasFoldCTE(formsIdx int) string {
	return fmt.Sprintf(`alias_fold(form, canon) AS (
          SELECT * FROM unnest($%d::text[], $%d::text[])
        )`, formsIdx, formsIdx+1)
}

// sourceMarketsCommon is the per-venue /v1/markets listing over [perSourcePoolsCTE], one row per canonical pair with THAT venue's counts.
// It is not distinctPairsCommon plus a source filter: prices_1m/1d `source = ANY(p.sources)` tests bucket membership, not contribution.
func (s *Store) sourceMarketsCommon(ctx context.Context, source, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	q, args := buildSourceMarketsQuery(since, source, cursor, limit, order)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("timescale: SourceMarkets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, hasMore, err := scanDistinctPairs(rows, limit, true)
	if err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if hasMore && len(out) > 0 {
		nextCursor = encodeMarketsCursor(out[len(out)-1], order)
	}
	return out, nextCursor, nil
}

// buildSourceMarketsQuery composes the per-venue listing over [perSourcePoolsCTE], byte-compatible with buildDistinctPairsQuery's columns
// and cursors. $1 since, $2 cursor, $3 limit+1, $4 source. bucket_close_at is date_trunc('day', last_trade_at) because prices_1d is
// pair-wide and lags a day.
func buildSourceMarketsQuery(since time.Time, source, cursor string, limit int, order MarketsOrder) (string, []any) {
	canonBase, canonQuote, flipped := canonOrientSQL(7)
	forms, canons := sacAliasFoldBind() // $5, $6
	ctes := perSourcePoolsCTE(5, "") + `
           AND p.source = $4
         GROUP BY p.source, p.base_asset, p.quote_asset
        ),
        canon AS (
          SELECT ` + canonBase + ` AS base_asset,
                 ` + canonQuote + ` AS quote_asset,
                 MAX(last_trade_at)              AS last_trade_at,
                 SUM(count_24h)                  AS count_24h,
                 SUM(vol_24h_usd::numeric)       AS vol_24h_num,
                 ` + canonLastPriceSQL(flipped) + ` AS last_price,
                 bool_or(vol_lower_bound)        AS vol_lower_bound
            FROM pools
           GROUP BY ` + canonBase + `, ` + canonQuote + `
        )
        SELECT base_asset, quote_asset, last_trade_at,
               date_trunc('day', last_trade_at) AS bucket_close_at,
               count_24h, NULLIF(vol_24h_num, 0)::text AS vol_24h_usd, last_price,
               vol_lower_bound
          FROM canon
    `
	switch order {
	case MarketsOrderVolume24hDesc:
		const tail = `
         WHERE $2 = ''
            OR COALESCE(vol_24h_num, 0)
                 <  CAST(NULLIF(split_part($2, ':', 1), '') AS numeric)
            OR (
                 COALESCE(vol_24h_num, 0)
                 =  CAST(COALESCE(NULLIF(split_part($2, ':', 1), ''), '0') AS numeric)
                 AND (base_asset || '|' || quote_asset)
                     > substring($2 from position(':' in $2) + 1)
               )
         ORDER BY COALESCE(vol_24h_num, 0) DESC,
                  (base_asset || '|' || quote_asset) ASC
         LIMIT $3
        `
		return ctes + tail, []any{since, cursor, limit + 1, source, forms, canons, canonical.NativeSACContractID()}
	default: // MarketsOrderPair
		const tail = `
         WHERE ($2 = '' OR (base_asset || '|' || quote_asset) > $2)
         ORDER BY (base_asset || '|' || quote_asset) ASC
         LIMIT $3
        `
		return ctes + tail, []any{since, cursor, limit + 1, source, forms, canons, canonical.NativeSACContractID()}
	}
}

func (s *Store) distinctPairsCommon(ctx context.Context, source, asset, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	// A per-source listing is never answerable from the pair-wide price CAGGs (`$source =
	// ANY(p.sources)` selects buckets, so every aggregate is the whole market's); route it to the
	// per-source CAGG rather than serve a cross-source answer under a per-source label.
	if source != "" {
		return s.sourceMarketsCommon(ctx, source, cursor, limit, order)
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// `since` is computed Go-side, not `NOW() - INTERVAL`, so the planner sees a constant and
	// prunes chunks at plan time. The query overfetches by one row (LIMIT limit+1) only to decide
	// whether to emit a nextCursor.
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	q, args := buildDistinctPairsQuery(since, source, asset, cursor, limit, order)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("timescale: DistinctPairs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, hasMore, err := scanDistinctPairs(rows, limit, false)
	if err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if hasMore && len(out) > 0 {
		nextCursor = encodeMarketsCursor(out[len(out)-1], order)
	}
	return out, nextCursor, nil
}

// scanDistinctPairs reads up to `limit+1` rows; the extra row sets hasMore. withLowerBound reads
// the per-source query's trailing vol_lower_bound column.
func scanDistinctPairs(rows *sql.Rows, limit int, withLowerBound bool) ([]Market, bool, error) {
	out := make([]Market, 0, limit)
	n := 0
	hasMore := false
	for rows.Next() {
		var (
			baseRaw, quoteRaw string
			lastAt            time.Time
			bucketCloseAt     time.Time
			count24h          int64
			vol24hUSD         sql.NullString
			lastPrice         sql.NullString
			lowerBound        bool
		)
		dest := []any{&baseRaw, &quoteRaw, &lastAt, &bucketCloseAt, &count24h, &vol24hUSD, &lastPrice}
		if withLowerBound {
			dest = append(dest, &lowerBound)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, false, fmt.Errorf("timescale: DistinctPairs scan: %w", err)
		}
		n++
		if n > limit {
			hasMore = true
			break
		}
		m, err := buildMarketRow(baseRaw, quoteRaw, lastAt, bucketCloseAt, count24h, vol24hUSD)
		if err != nil {
			// Skip an unparseable row rather than 500 all of /v1/markets. Ingest only emits canonical
			// asset strings, so this means something bypassed the write path; the warning and
			// counter let operators find and remove it.
			obs.MarketsSkippedRowsTotal.Inc()
			slog.Default().Warn("markets: skipping unparseable trades row",
				"base", baseRaw, "quote", quoteRaw, "err", err)
			continue
		}
		if lastPrice.Valid && lastPrice.String != "" {
			v := lastPrice.String
			m.LastPrice = &v
		}
		m.VolumeLowerBound = lowerBound
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("timescale: DistinctPairs rows: %w", err)
	}
	return out, hasMore, nil
}

func buildMarketRow(baseRaw, quoteRaw string, lastAt, bucketCloseAt time.Time, count24h int64, vol24hUSD sql.NullString) (Market, error) {
	base, err := canonical.ParseAsset(baseRaw)
	if err != nil {
		return Market{}, fmt.Errorf("timescale: DistinctPairs base %q: %w", baseRaw, err)
	}
	quote, err := canonical.ParseAsset(quoteRaw)
	if err != nil {
		return Market{}, fmt.Errorf("timescale: DistinctPairs quote %q: %w", quoteRaw, err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return Market{}, fmt.Errorf("timescale: DistinctPairs pair: %w", err)
	}
	m := Market{
		Pair:          pair,
		LastTradeAt:   lastAt.UTC(),
		BucketCloseAt: bucketCloseAt.UTC(),
		TradeCount24h: count24h,
	}
	if vol24hUSD.Valid && vol24hUSD.String != "" && vol24hUSD.String != "0" {
		v := vol24hUSD.String
		m.Volume24hUSD = &v
	}
	return m, nil
}

// ValidateMarketsCursor errors when a non-empty cursor does not match the shape encodeMarketsCursor emits for the order, so handlers can answer 400 instead of a collation-dependent skip or a Postgres numeric-syntax error.
func ValidateMarketsCursor(cursor string, order MarketsOrder) error {
	if cursor == "" {
		return nil
	}
	pairPart := cursor
	if order == MarketsOrderVolume24hDesc {
		idx := strings.IndexByte(cursor, ':')
		if idx < 0 {
			return fmt.Errorf("missing ':' separator")
		}
		volPart := cursor[:idx]
		// The prefix may be empty (last row had a null vol_usd); otherwise digits with at most one
		// '.', no sign, at least one digit. Shared with the assets cursor.
		if volPart != "" && !isNumericPrefix(volPart) {
			return fmt.Errorf("non-numeric volume prefix")
		}
		pairPart = cursor[idx+1:]
		if pairPart == "" {
			return fmt.Errorf("missing pair suffix")
		}
	}
	pipe := strings.IndexByte(pairPart, '|')
	if pipe < 0 {
		return fmt.Errorf("missing '|' separator in pair")
	}
	if pipe == 0 || pipe == len(pairPart)-1 {
		return fmt.Errorf("missing base or quote in pair")
	}
	return nil
}

// encodeMarketsCursor formats the last-row cursor: pair-only for MarketsOrderPair, `<vol>:<pair>` for MarketsOrderVolume24hDesc.
func encodeMarketsCursor(last Market, order MarketsOrder) string {
	pairKey := last.Pair.Base.String() + "|" + last.Pair.Quote.String()
	if order == MarketsOrderVolume24hDesc {
		vol := ""
		if last.Volume24hUSD != nil {
			vol = *last.Volume24hUSD
		}
		return vol + ":" + pairKey
	}
	return pairKey
}

// distinctPairsActivityCTEs is the static head of /v1/markets: `d` (14-day active pairs, prices_1d) and `h` (trailing-24h prices_1m scan).
// It binds $1 since, $4 source, $5 asset aliases, $6/$7 sacAliasFoldBind; buildDistinctPairsQuery appends `raw` and `canon`.
const distinctPairsActivityCTEs = `
        WITH alias_fold(form, canon) AS (
          SELECT * FROM unnest($6::text[], $7::text[])
        ),
        d AS (
            SELECT p.base_asset, p.quote_asset,
                   MAX(p.bucket) AS bucket_close_at,
                   (array_agg(p.last_price ORDER BY p.bucket DESC)
                      FILTER (WHERE p.last_price IS NOT NULL))[1]::text AS last_price
              FROM prices_1d p
             WHERE p.bucket >= $1
               AND ($4 = '' OR $4 = ANY(p.sources))
               AND (cardinality($5::text[]) = 0 OR p.base_asset = ANY($5) OR p.quote_asset = ANY($5))
             GROUP BY p.base_asset, p.quote_asset
        ),
        h AS (
            SELECT p.base_asset, p.quote_asset,
                   MAX(p.bucket)       AS last_bucket_1m,
                   SUM(p.trade_count)  AS count_24h,
                   SUM(p.volume_usd)   AS vol_24h_num,
                   -- last(), not an ordered array_agg: this scan covers
                   -- every active pair's 24h of MINUTE buckets, and a
                   -- per-group ORDER BY would sort all of them to read one
                   -- value per group. last() keeps the newest bucket's
                   -- close in the same single pass the sums already make;
                   -- FILTER drops null closes so a quiet tail bucket
                   -- cannot mask the last real price. (The prices_1d CTE
                   -- keeps array_agg: ~14 buckets per pair, nothing to
                   -- gain.)
                   last(p.last_price, p.bucket)
                      FILTER (WHERE p.last_price IS NOT NULL)::text AS last_price
              FROM prices_1m p
             WHERE p.bucket > NOW() - INTERVAL '24 hours'
               AND ($4 = '' OR $4 = ANY(p.sources))
               AND (cardinality($5::text[]) = 0 OR p.base_asset = ANY($5) OR p.quote_asset = ANY($5))
             GROUP BY p.base_asset, p.quote_asset
        ),
`

// buildDistinctPairsQuery composes the per-pair-volume CTE and SELECT. Cursors: MarketsOrderPair is `<base>|<quote>` (strict-greater);
// MarketsOrderVolume24hDesc is `<vol_or_blank>:<base>|<quote>`, a blank vol sorting as 0 so ":<pair>" resumes into the null-volume tail.
func buildDistinctPairsQuery(since time.Time, source, asset, cursor string, limit int, order MarketsOrder) (string, []any) {
	// /v1/markets is a directory listing, not a history view: reading prices_1m x 14 days blows
	// the 8s handler ceiling. So the 14d active-pair set comes from prices_1d and only the 24h
	// figures read prices_1m (chunk-pruned to the last day, so exact). A rolling 24h window is not
	// bucket-additive over a coarser CAGG: prices_1h understated 24h volume ~9%.
	//
	// d and h are FULL OUTER JOINed because prices_1d (6h end_offset, materialized_only) has no
	// row yet for a pair whose first trade is today. last_price, last_trade_at and
	// bucket_close_at prefer h, falling back to d only for pairs idle over 24h (count_24h is then
	// COALESCE'd to 0).
	//
	// $5 is a text[] of the asset's alias forms: XLM is keyed `native`, `crypto:XLM` or its SAC
	// depending on venue, so a scalar match omitted markets. canon collapses flipped orientations
	// (the SDEX decoder records both XLM/USDC and USDC/XLM) into ONE row: volume and count sum
	// across directions and last_price is the latest trade in the canonical orientation (see
	// canonical.Orient). `folded` first maps each SAC spelling onto its classic form so a market
	// on SDEX and a Soroban venue is one row.
	canonBase, canonQuote, flipped := canonOrientSQL(8)
	ctes := distinctPairsActivityCTEs + `        raw AS (
            SELECT COALESCE(d.base_asset, h.base_asset)   AS base_asset,
                   COALESCE(d.quote_asset, h.quote_asset) AS quote_asset,
                   COALESCE(h.last_bucket_1m, d.bucket_close_at) AS last_trade_at,
                   GREATEST(d.bucket_close_at,
                            date_trunc('day', h.last_bucket_1m)) AS bucket_close_at,
                   COALESCE(h.count_24h, 0)  AS count_24h,
                   COALESCE(h.vol_24h_num, 0) AS vol_24h_num,
                   COALESCE(h.last_price, d.last_price) AS last_price
              FROM d
              FULL OUTER JOIN h
                ON h.base_asset = d.base_asset
               AND h.quote_asset = d.quote_asset
        ),
        folded AS (
            SELECT COALESCE(bf.canon, raw.base_asset)  AS base_asset,
                   COALESCE(qf.canon, raw.quote_asset) AS quote_asset,
                   raw.last_trade_at, raw.bucket_close_at, raw.count_24h,
                   raw.vol_24h_num, raw.last_price
              FROM raw
              LEFT JOIN alias_fold bf ON bf.form = raw.base_asset
              LEFT JOIN alias_fold qf ON qf.form = raw.quote_asset
             WHERE COALESCE(bf.canon, raw.base_asset) <> COALESCE(qf.canon, raw.quote_asset)
        ),
        canon AS (
            SELECT ` + canonBase + ` AS base_asset,
                   ` + canonQuote + ` AS quote_asset,
                   MAX(last_trade_at)   AS last_trade_at,
                   MAX(bucket_close_at) AS bucket_close_at,
                   SUM(count_24h)       AS count_24h,
                   SUM(vol_24h_num)     AS vol_24h_num,
                   ` + canonLastPriceSQL(flipped) + ` AS last_price
              FROM folded
             GROUP BY ` + canonBase + `, ` + canonQuote + `
        )
        SELECT base_asset, quote_asset, last_trade_at, bucket_close_at,
               count_24h, NULLIF(vol_24h_num, 0)::text AS vol_24h_usd, last_price
          FROM canon
    `
	assets := assetAliasBind(asset)
	forms, canons := sacAliasFoldBind() // $6, $7
	switch order {
	case MarketsOrderVolume24hDesc:
		// Cursor "<vol_or_blank>:<base>|<quote>" is a keyset on (vol_24h, base|quote). vol_24h_num is
		// already COALESCEd to 0, so a 24h-idle pair sorts last under DESC. The next page is
		// (v < cv) OR (v = cv AND pair > cpair): DESC on the first key flips its comparator.
		const tail = `
         WHERE $2 = ''
            OR vol_24h_num
                 <  CAST(NULLIF(split_part($2, ':', 1), '') AS numeric)
            OR (
                 vol_24h_num
                 =  CAST(COALESCE(NULLIF(split_part($2, ':', 1), ''), '0') AS numeric)
                 AND (base_asset || '|' || quote_asset)
                     > substring($2 from position(':' in $2) + 1)
               )
         ORDER BY vol_24h_num DESC,
                  (base_asset || '|' || quote_asset) ASC
         LIMIT $3
        `
		return ctes + tail, []any{since, cursor, limit + 1, source, assets, forms, canons, canonical.NativeSACContractID()}
	default: // MarketsOrderPair
		const tail = `
         WHERE ($2 = '' OR (base_asset || '|' || quote_asset) > $2)
         ORDER BY (base_asset || '|' || quote_asset) ASC
         LIMIT $3
        `
		return ctes + tail, []any{since, cursor, limit + 1, source, assets, forms, canons, canonical.NativeSACContractID()}
	}
}

// pairMarketQuery is the single-pair summary behind /v1/pairs; $1 is the MarketsRecencyWindow bound, $2/$3 base and quote. LastTradeAt
// comes from trades, not a CAGG bucket. Both stored directions combine into the requested orientation (canonOrientSQL; last_price via
// canonical.Orient). A UNION ALL of two single-direction branches: `(A AND B) OR (B AND A)` cannot drive trades_pair_ts_idx /
// prices_1m_pair_bucket_idx (see [closedVWAP1mAtOrBeforeQuery]) and one shared 14-day aggregate exceeds the 8s ceiling cold. `,
// base_asset` makes the last_price order total. Guarded by TestPairMarketQueryShape.
const pairMarketQuery = `
        WITH last_trade AS (
            SELECT MAX(ts) AS ts FROM (
                (SELECT t.ts FROM trades t
                  WHERE t.base_asset = $2 AND t.quote_asset = $3
                    AND t.ts >= $1
                  ORDER BY t.ts DESC LIMIT 1)
                UNION ALL
                (SELECT t.ts FROM trades t
                  WHERE t.base_asset = $3 AND t.quote_asset = $2
                    AND t.ts >= $1
                  ORDER BY t.ts DESC LIMIT 1)
            ) AS both_directions
        )
        SELECT lt.ts AS last_trade_at,
               CASE WHEN lt.ts IS NULL THEN 0 ELSE
                   (SELECT count(*) FROM trades t
                     WHERE t.base_asset = $2 AND t.quote_asset = $3
                       AND t.ts > NOW() - INTERVAL '24 hours')
                 + (SELECT count(*) FROM trades t
                     WHERE t.base_asset = $3 AND t.quote_asset = $2
                       AND t.ts > NOW() - INTERVAL '24 hours')
               END AS count_24h,
               (SELECT SUM(volume_usd)::text FROM (
                    SELECT volume_usd FROM prices_1m
                     WHERE base_asset = $2 AND quote_asset = $3
                       AND bucket >= NOW() - INTERVAL '24 hours'
                       AND volume_usd IS NOT NULL
                    UNION ALL
                    SELECT volume_usd FROM prices_1m
                     WHERE base_asset = $3 AND quote_asset = $2
                       AND bucket >= NOW() - INTERVAL '24 hours'
                       AND volume_usd IS NOT NULL
                ) AS both_directions) AS vol_24h_usd,
               (SELECT (CASE WHEN base_asset = $2 THEN last_price
                             ELSE 1.0 / NULLIF(last_price, 0) END)::text FROM (
                    (SELECT bucket, base_asset, last_price FROM prices_1m
                      WHERE base_asset = $2 AND quote_asset = $3
                        AND bucket >= NOW() - INTERVAL '24 hours'
                        AND last_price IS NOT NULL
                      ORDER BY bucket DESC LIMIT 1)
                    UNION ALL
                    (SELECT bucket, base_asset, last_price FROM prices_1m
                      WHERE base_asset = $3 AND quote_asset = $2
                        AND bucket >= NOW() - INTERVAL '24 hours'
                        AND last_price IS NOT NULL
                      ORDER BY bucket DESC LIMIT 1)
                ) AS both_directions
                 ORDER BY bucket DESC, base_asset LIMIT 1) AS last_price
          FROM last_trade lt
    `

// PairMarket returns the activity summary for one (base, quote) pair; false when it has not traded inside MarketsRecencyWindow, so callers answer an empty list (200), not a 404.
func (s *Store) PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error) {
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	var (
		lastAt    *time.Time
		count24h  *int64
		vol24hUSD sql.NullString
		lastPx    sql.NullString
	)
	if err := s.db.QueryRowContext(ctx, pairMarketQuery, since, base.String(), quote.String()).Scan(&lastAt, &count24h, &vol24hUSD, &lastPx); err != nil {
		return Market{}, false, fmt.Errorf("timescale: PairMarket: %w", err)
	}
	if lastAt == nil {
		return Market{}, false, nil
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return Market{}, false, fmt.Errorf("timescale: PairMarket pair: %w", err)
	}
	var n int64
	if count24h != nil {
		n = *count24h
	}
	lastAtUTC := lastAt.UTC()
	bucketClose := time.Date(lastAtUTC.Year(), lastAtUTC.Month(), lastAtUTC.Day(), 0, 0, 0, 0, time.UTC)
	m := Market{
		Pair:          pair,
		LastTradeAt:   lastAtUTC,
		BucketCloseAt: bucketClose,
		TradeCount24h: n,
	}
	if vol24hUSD.Valid && vol24hUSD.String != "" && vol24hUSD.String != "0" {
		v := vol24hUSD.String
		m.Volume24hUSD = &v
	}
	if lastPx.Valid && lastPx.String != "" {
		v := lastPx.String
		m.LastPrice = &v
	}
	return m, true, nil
}

// PairVolumePoint is one hourly USD-volume sample for a (base, quote) pair; Hour is the UTC bucket start and VolumeUSD is a numeric string for full precision.
type PairVolumePoint struct {
	Hour      time.Time
	VolumeUSD string
}

// pairKey is the `<base>|<quote>` lookup key for the batched volume map, string-based so the v1 adapter need not import canonical.Pair.
type pairKey = string

// GetPairsVolumeHistory24hBatch returns per-(base, quote) hourly USD-volume buckets for the trailing 24h, for the /v1/markets sparkline column.
func (s *Store) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[pairKey][]PairVolumePoint, error) {
	if len(pairs) == 0 {
		return map[pairKey][]PairVolumePoint{}, nil
	}
	_, spellings := sacFoldMaps()
	keys, bases, quotes := expandSACSpellings(pairs, spellings)
	const q = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		want AS (
		  SELECT DISTINCT pair_key, base_asset, quote_asset
		    FROM unnest($1::text[], $2::text[], $3::text[])
		         AS t(pair_key, base_asset, quote_asset)
		),
		keys AS (
		  SELECT DISTINCT pair_key FROM want
		),
		per_dir AS (
		  SELECT w.pair_key, p.bucket, p.volume_usd
		    FROM want w
		    JOIN prices_1m p
		      ON p.base_asset = w.base_asset AND p.quote_asset = w.quote_asset
		   WHERE p.bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND p.volume_usd IS NOT NULL
		  UNION ALL
		  SELECT w.pair_key, p.bucket, p.volume_usd
		    FROM want w
		    JOIN prices_1m p
		      ON p.base_asset = w.quote_asset AND p.quote_asset = w.base_asset
		   WHERE p.bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND p.volume_usd IS NOT NULL
		),
		per_hour AS (
		  SELECT pair_key,
		         date_trunc('hour', bucket) AS h,
		         SUM(volume_usd)::text      AS vol
		    FROM per_dir
		   GROUP BY pair_key, h
		)
		SELECT k.pair_key,
		       to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		       COALESCE(p.vol, '0') AS v
		  FROM keys k
		  CROSS JOIN hours
		  LEFT JOIN per_hour p ON p.pair_key = k.pair_key AND p.h = hours.bucket
		 ORDER BY k.pair_key, hours.bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, q, keys, bases, quotes)
	if err != nil {
		return nil, fmt.Errorf("timescale: GetPairsVolumeHistory24hBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[pairKey][]PairVolumePoint, len(pairs))
	for rows.Next() {
		var key, ts, vol string
		if err := rows.Scan(&key, &ts, &vol); err != nil {
			return nil, fmt.Errorf("timescale: GetPairsVolumeHistory24hBatch scan: %w", err)
		}
		hour, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		out[key] = append(out[key], PairVolumePoint{Hour: hour, VolumeUSD: vol})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetPairsVolumeHistory24hBatch rows: %w", err)
	}
	return out, nil
}

// FirstTradeBatch returns each requested pair's first daily bucket in prices_1d (the "since inception" anchor). A bucket fed
// only by non-VWAP sources is not a trade and does not anchor.
func (s *Store) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	if len(pairs) == 0 {
		return map[string]time.Time{}, nil
	}
	fold, spellings := sacFoldMaps()
	_, fwdBase, fwdQuote := expandSACSpellings(pairs, spellings)
	bases := append(append([]string{}, fwdBase...), fwdQuote...)
	quotes := append(append([]string{}, fwdQuote...), fwdBase...)
	const q = `
        SELECT base_asset, quote_asset, MIN(bucket)
          FROM prices_1d
         WHERE (base_asset, quote_asset) IN (
               SELECT UNNEST($1::text[]), UNNEST($2::text[]))
           AND NOT COALESCE(sources <@ string_to_array($3, ','), false)
         GROUP BY base_asset, quote_asset`
	rows, err := s.db.QueryContext(ctx, q, bases, quotes, nonVWAPSourcesArg())
	if err != nil {
		return nil, fmt.Errorf("timescale: FirstTradeBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	firsts := map[string]time.Time{}
	for rows.Next() {
		var b, qa string
		var t time.Time
		if err := rows.Scan(&b, &qa, &t); err != nil {
			return nil, fmt.Errorf("timescale: FirstTradeBatch scan: %w", err)
		}
		fb, fq := foldSpelling(fold, b), foldSpelling(fold, qa)
		for _, key := range []string{b + "|" + qa, qa + "|" + b, fb + "|" + fq, fq + "|" + fb} {
			if cur, ok := firsts[key]; !ok || t.Before(cur) {
				firsts[key] = t
			}
		}
	}
	return firsts, rows.Err()
}
