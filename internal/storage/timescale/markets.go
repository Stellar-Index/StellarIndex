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

// Market is one distinct (base, quote) pair summary with activity
// statistics. Returned by [Store.DistinctPairs].
//
// Volume24hUSD is the trailing-24h USD volume summed from the
// prices_1m hypertable (which has per-bucket volume_usd
// computed by the aggregator) — or, for a source-filtered
// listing, from pools_per_source_1h, so the figure is that
// venue's own contribution and not the pair's cross-venue
// total (see [Store.SourceMarkets]). Pointer + nil-when-zero so a
// pair with no USD-equivalent trades emits JSON null rather
// than "0" — important for downstream filtering.
//
// Field semantics:
//
//   - LastTradeAt — minute-precise start of the most recent
//     prices_1m bucket that observed a trade in this pair, within
//     the 24h window. Falls back to BucketCloseAt for pairs that
//     traded 14d-active-but-24h-idle (no minute-precise signal in
//     the 24h scan window).
//   - BucketCloseAt — start-of-day UTC of the day bucket the pair
//     was last active in. Always populated. Aligns to UTC
//     midnight by construction (`time_bucket('1 day', ts)`), so it
//     is not a freshness signal; LastTradeAt is.
type Market struct {
	Pair          canonical.Pair
	LastTradeAt   time.Time
	BucketCloseAt time.Time
	TradeCount24h int64
	Volume24hUSD  *string
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time
	// usd_volume. Computed only for the per-source listing.
	VolumeLowerBound bool
	// LastPrice is the last quote-per-base price observed for this
	// pair: the newest prices_1m bucket close within the trailing 24h,
	// or — for a pair that has been idle longer than that — the newest
	// materialized prices_1d close inside the recency window. Under a
	// source filter it is that VENUE's own last price, from
	// pools_per_source_1h. nil when no bucket in range has a non-null
	// `last_price` (cold pair, freshly-ingested fixture, etc.).
	// Numeric-stringified for precision parity.
	LastPrice *string
}

// Pool is one (source, base, quote) tuple — same shape as Market
// but with the source dimension preserved. Returned by [Store.AllPools]
// to back the /v1/pools listing where the same pair traded on
// multiple venues becomes multiple rows.
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

// MarketsRecencyWindow bounds the trades scanned by DistinctPairs.
// Pairs that haven't traded inside the window are excluded from the
// `/v1/markets` listing — the public contract is "active markets",
// not "every pair ever observed". The window is exposed as a var so
// tests can override it without changing the public function signature.
//
// A concurrent backfill evicts the recent chunks from the buffer cache and
// pushes the cold call to ~7 s.
//
// 30-day: ~9 s with JIT, ~3 s without — too slow for a hot path.
// 90-day: ~16-19 s — exceeded the 30s client deadline.
//
// 14 days passes the "active markets" intuition (a market that
// hasn't traded in two weeks isn't really active) and the
// COMPRESSED chunks at the boundary fit in postgres's shared
// buffers. A future materialised market_catalogue would let us
// drop the recency bound entirely.
var MarketsRecencyWindow = 14 * 24 * time.Hour

// MarketsOrder controls the sort + cursor scheme used by
// DistinctPairs. Default is alphabetic by `<base>|<quote>` (stable
// pagination through the entire 14-day-active set). Volume24hDesc
// is used by the explorer's `/markets` page so the most active
// USD-volume pairs surface in the first page rather than being
// alphabetically deep behind ~5K dust pairs.
type MarketsOrder int

const (
	// MarketsOrderPair = ORDER BY (base|quote) ASC. Cursor is the
	// (base|quote) string. Stable; iterates the full set.
	MarketsOrderPair MarketsOrder = iota
	// MarketsOrderVolume24hDesc = ORDER BY volume_24h_usd DESC NULLS
	// LAST, (base|quote) ASC. Cursor is `<vol_or_blank>:<base|quote>`.
	// Pairs with USD volume surface first; the long-tail dust comes
	// last. Useful for "what are the active markets" queries.
	MarketsOrderVolume24hDesc
)

// DistinctPairs returns one page of recently-active (base, quote)
// pairs from the trades hypertable, each with its most-recent trade
// timestamp, 24h trade count, and 24h USD volume.
//
// Pagination is cursor-based; the cursor format depends on
// `order`. Empty cursor starts from the beginning.
//
// limit clamps to [1, 500] — matching DistinctAssets for consistency.
//
// Recency window: the query scans only chunks within the last
// MarketsRecencyWindow (default 14d) so chunk pruning bounds I/O on
// a hypertable with hundreds of millions of trades.
func (s *Store) DistinctPairs(ctx context.Context, cursor string, limit int) ([]Market, string, error) {
	return s.DistinctPairsExt(ctx, cursor, limit, MarketsOrderPair)
}

// DistinctPairsExt is DistinctPairs with explicit ordering control.
// DistinctPairs is preserved as the legacy 3-arg form so existing
// callers compile unchanged.
func (s *Store) DistinctPairsExt(ctx context.Context, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.distinctPairsCommon(ctx, "", "", cursor, limit, order)
}

// SourceMarkets returns one page of (base, quote) pairs the given
// source observed in the trailing MarketsRecencyWindow — a per-DEX
// pool list whose 24h volume, 24h trade count and last price are THAT
// venue's own, computed from the per-source CAGG /v1/pools reads. Same
// wire shape as DistinctPairsExt.
//
// It deliberately does NOT route through distinctPairsCommon: the
// pair-wide price CAGGs have no per-source grain to filter on, only a
// `sources` array to test membership against, so a source-filtered
// read of them returns the whole market's figures. See
// [sourceMarketsCommon].
func (s *Store) SourceMarkets(ctx context.Context, source, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.sourceMarketsCommon(ctx, source, cursor, limit, order)
}

// AssetMarkets returns one page of (base, quote) pairs where the
// given canonical asset_id appears on either side of the pair —
// `t.base_asset = $asset OR t.quote_asset = $asset`. Backs the
// `/v1/markets?asset=<asset_id>` query that the explorer's
// /assets/{slug} Markets tab uses to surface every market the
// asset participates in without paying for a 500-row global scan
// + client-side filter (the previous shape).
func (s *Store) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order MarketsOrder) ([]Market, string, error) {
	return s.distinctPairsCommon(ctx, "", asset, cursor, limit, order)
}

// PoolsFilter narrows AllPools by venue and/or pair. Zero-value
// fields mean "no filter on this dimension"; an unfiltered call
// uses PoolsFilter{}.
//
// Pair filter (Base + Quote both non-empty) is the canonical
// per-pair source-contribution query — used by the pair detail
// page to render "which venues moved this pair in the last 24h".
// It matches either stored orientation, and rows come back in the
// canonical orientation (canonical.Orient) whichever order was asked.
// Single-side filters (Base only / Quote only) are accepted but
// uncommon; they match that side of the canonical orientation. Every
// asset filter matches all alias forms (canonical.AssetAliasStrings).
//
// Asset filter is the OR-shape — base = X OR quote = X. Used by
// asset-detail surfaces ("every pool touching this asset")
// without forcing the caller to fire two parallel `?base=` +
// `?quote=` requests and merge client-side. Mutually exclusive
// with Base/Quote at the handler layer (the storage layer
// accepts the combination, but its semantics aren't
// well-defined: an asset-OR + base-AND would land on the
// intersection or the union depending on interpretation).
type PoolsFilter struct {
	Sources []string
	Base    string
	Quote   string
	Asset   string
}

// AllPools returns every (source, base, quote) tuple observed in
// the trailing MarketsRecencyWindow. Distinct from DistinctPairsExt
// which collapses across sources — same physical pair traded on
// soroswap + sdex returns ONE row from DistinctPairsExt and TWO
// rows from AllPools. Backs /v1/pools.
//
// `filter.Sources` constrains to a venue allowlist; `filter.Base` /
// `filter.Quote` constrain by canonical asset_id. Empty fields
// mean no filter.
//
// Cursor format: "<vol_or_blank>:<source>|<base>|<quote>" for
// volume-desc; "<source>|<base>|<quote>" for pair-asc. Same
// keyset-pagination shape as DistinctPairsExt with the source
// dimension prepended.
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

// perSourcePoolsCTE is the per-(source, base, quote) scan of the
// pools_per_source_1h continuous aggregate (migration 0036) that BOTH
// per-venue reads share: /v1/pools (buildPoolsQuery) and the
// source-filtered /v1/markets listing (buildSourceMarketsQuery).
//
// It is one literal, not two, because the two surfaces are required to
// agree: `/v1/markets?source=X` and `/v1/pools?source=X` describe the
// same venue's same pair and would disagree by orders of magnitude if
// the markets listing read the PAIR-WIDE prices_1m/prices_1d CAGGs and
// merely FILTERED them by `source = ANY(sources)`, which selects
// BUCKETS a venue printed in, not that venue's contribution. Sharing
// the CTE makes the agreement structural instead of a thing two query
// templates have to remember.
//
// Callers append their own filter predicates, the GROUP BY, and their
// ordering tail. $1 is the recency-window lower bound; every caller
// binds it first, and sacAliasFoldBind's arrays at $foldIdx, $foldIdx+1.
//
// Rows are alias-folded BEFORE grouping, so the filters and every group
// key see one spelling per asset.
//
// vol_24h_usd sums trade-time usd_volume only. A trade stamped without one
// is excluded, never re-marked at today's XLM/USD; vol_lower_bound says so.
func perSourcePoolsCTE(foldIdx int) string {
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
            last(p.bucket_last_price, p.bucket_last_ts)::text AS last_price
          FROM (
            SELECT r.source, r.bucket, r.bucket_last_ts, r.bucket_last_price,
                   r.trade_count, r.sum_usd_priced, r.sum_base_unpriced,
                   COALESCE(bf.canon, r.base_asset)  AS base_asset,
                   COALESCE(qf.canon, r.quote_asset) AS quote_asset
              FROM pools_per_source_1h r
              LEFT JOIN alias_fold bf ON bf.form = r.base_asset
              LEFT JOIN alias_fold qf ON qf.form = r.quote_asset
             -- A wrap/unwrap (native vs its SAC) folds to a self-pair, which is no market.
             WHERE COALESCE(bf.canon, r.base_asset) <> COALESCE(qf.canon, r.quote_asset)
          ) p
         WHERE p.bucket >= $1
    `
}

// poolsFilterSQL renders the /v1/pools filter predicates over the raw
// pools_per_source_1h rows. $4 sources and $5 base / $6 quote / $7 asset
// alias arrays are always bound (see poolsFilterArgs); an empty array
// disables its predicate, keeping the positional layout stable.
//
// The rows hold BOTH stored orientations and every alias spelling of an
// asset, and `canon` folds orientations only afterwards, so each
// predicate must be invariant under that fold or it drops the other
// direction's volume before the fold can sum it:
//   - base+quote matches the pair in either stored orientation;
//   - base or quote alone matches that side of the CANONICAL
//     orientation, the only orientation a folded row has;
//   - asset (the OR-shape) matches either leg.
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
	// Scanning the trades hypertable directly would take three passes:
	// the vol_24h CTE (24h SUM grouped by source+pair), the last_px
	// CTE (DISTINCT ON per-source latest price), and the outer
	// enumeration (14d window LEFT JOINing both CTEs). That measures
	// 8-30s on a populated 2.7B-row hypertable, so every read instead
	// collapses into a single scan of the
	// pools_per_source_1h continuous aggregate (migration 0036).
	// One row per (source, base, quote, 1h bucket) holds
	// SUM(usd_volume) split by Phase-1-priced vs needs-XLM-fallback,
	// COUNT(*), and last(quote_amount/base_amount, ts). The
	// `pools` CTE re-aggregates those hourly rows over the 14d
	// window with FILTER clauses pulling the 24h slice for
	// vol_24h_usd + count_24h. Trade-off: last_trade_at lags by up
	// to one refresh interval (5 min) — acceptable for a pools
	// discovery surface.
	//
	// XLM-fallback semantics preserved exactly: priced trades
	// contribute their stored usd_volume; unpriced trades with an
	// XLM leg contribute base_amount × XLM/USD (or quote_amount
	// × XLM/USD); pure-SEP-41/SEP-41 unpriced trades stay 0 (a
	// direct trades scan would return NULL; the handler scan collapses
	// NULL and "0" identically, so the two are equivalent).
	// $8/$9 are the alias-fold arrays, after the $1..$7 layout below.
	cte := perSourcePoolsCTE(8)
	canonBase, canonQuote, flipped := canonOrientSQL(10)
	cte += poolsFilterSQL(canonBase, canonQuote) + `
         GROUP BY p.source, p.base_asset, p.quote_asset
        )
    `
	// canon collapses flipped orientations of the same market within a
	// source (XLM/USDC + USDC/XLM → one canonical row): vol + trade
	// count sum across both directions; last_price is the latest trade's
	// price re-expressed canonically (inverted for the flipped one); either
	// direction's excluded trades make the folded volume a lower bound.
	// See canonical.Orient.
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

// assetAliasBind expands an asset filter to the text[] of its alias forms
// (native / crypto:XLM / SAC for XLM), so an `= ANY($n)` leg matches every
// spelling a venue keyed its rows under. Empty asset binds an empty,
// NON-nil array: nil would bind SQL NULL, cardinality(NULL) is NULL rather
// than 0, and the "no filter" short-circuit would then exclude every row.
// An unparseable asset binds as a one-element set.
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

// sacAliasFoldBind binds the alias_fold (form, canon) arrays: every SAC
// contract spelling mapped to its classic/native form, so a market traded
// on SDEX and on a Soroban venue groups as one row. crypto:XLM is left
// unfolded: it keys the off-chain CEX population, served as its own row.
// Both slices are non-nil for the same NULL reason as assetAliasBind.
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

// expandSACSpellings lists, per requested pair, every stored (base, quote)
// spelling the alias-folded listing summed into it, as parallel slices
// keyed by the requested "<base>|<quote>".
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

// sourceMarketsCommon is the per-venue /v1/markets listing. It reads
// the SAME per-source continuous aggregate /v1/pools reads
// (pools_per_source_1h, via [perSourcePoolsCTE]) and collapses the
// source dimension away — one row per canonical pair, carrying THAT
// venue's own 24h trade count, 24h USD volume and last price.
//
// Why not distinctPairsCommon with a source filter:
// prices_1m and prices_1d are grouped by (bucket, base, quote) with an
// `array_agg(DISTINCT source) AS sources` column, so `$source = ANY(
// p.sources)` is a BUCKET-membership test, not a contribution filter.
// A pair soroswap printed three times, in minutes SDEX printed a
// thousand times in, came back with SDEX's thousand:
// `/v1/markets?source=soroswap` reported the whole market's volume and
// trade count while `/v1/pools?source=soroswap` reported soroswap's
// own, so the two surfaces disagreed by orders of magnitude about the
// same venue's same pair. The per-source grain only exists in
// pools_per_source_1h, so that is what a per-source answer has to be
// computed from.
//
// Freshness comes along with it: pools_per_source_1h has a 5-minute
// end_offset against prices_1d's 6 hours, so the per-venue last_price
// is minutes old rather than the previous UTC day's close.
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

// buildSourceMarketsQuery composes the per-venue listing over
// [perSourcePoolsCTE]. Column list, cursor formats and ordering
// branches are byte-compatible with buildDistinctPairsQuery so
// [scanDistinctPairs] and [encodeMarketsCursor] serve both — only the
// grain underneath differs.
//
// $1 since (14d window), $2 cursor, $3 limit+1 (overfetch-by-one),
// $4 source.
//
// bucket_close_at is derived as `date_trunc('day', last_trade_at)`
// rather than read from prices_1d: the field's documented meaning is
// "start-of-day UTC of the day bucket the pair was last active in",
// and the per-source last_trade_at answers that exactly — where
// prices_1d could not, since its newest materialized bucket is the
// PREVIOUS day (6h end_offset, materialized_only) and it is pair-wide
// rather than per-venue anyway.
func buildSourceMarketsQuery(since time.Time, source, cursor string, limit int, order MarketsOrder) (string, []any) {
	canonBase, canonQuote, flipped := canonOrientSQL(7)
	forms, canons := sacAliasFoldBind() // $5, $6
	ctes := perSourcePoolsCTE(5) + `
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
	// A per-source listing is never answerable from the pair-wide price
	// CAGGs — `$source = ANY(p.sources)` selects buckets a venue printed
	// in, so every aggregate over them is the whole market's. Route it
	// to the per-source CAGG instead of computing a cross-source answer
	// under a per-source label; no caller can reach the wrong shape by
	// passing a source here.
	if source != "" {
		return s.sourceMarketsCommon(ctx, source, cursor, limit, order)
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// `since` is computed Go-side instead of `NOW() - INTERVAL` so
	// the planner sees a constant timestamp parameter and can prune
	// chunks at plan time rather than relying on stable-function
	// evaluation. count_24h uses FILTER (more readable than the
	// SUM/CASE form, identical plan).
	//
	// Overfetch by one (LIMIT $N = limit+1) to detect "more pages
	// exist". The extra row isn't returned to the caller; its only
	// purpose is to toggle whether we emit a nextCursor.
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

// scanDistinctPairs reads up to `limit+1` rows; the +1th row toggles
// hasMore. Pulled out of DistinctPairsExt so the latter stays under
// the gocognit threshold. withLowerBound reads the per-source query's
// trailing vol_lower_bound column.
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
			// Skip rows we can't parse rather than failing the whole
			// response: one malformed trades row (e.g. a manual insert
			// with a non-canonical asset code) would otherwise 500 the
			// entire /v1/markets surface. The ingest pipeline only emits
			// canonical asset strings, so reaching this branch means
			// something bypassed the normal write path; surface it as
			// a warning + counter so operators can find and remove it
			// rather than serving a 500 to every consumer.
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

// ValidateMarketsCursor returns an error if `cursor` is non-empty
// but doesn't match the encoded shape that encodeMarketsCursor
// emits for the active order. Empty cursor is always valid (start
// from the first page). Callers should reject invalid cursors at
// the handler boundary with a 400.
//
// Without this guard, a hand-crafted cursor (or a stale link from
// before a pagination format change) silently degrades:
//
//   - MarketsOrderPair: the SQL predicate
//     `(base || '|' || quote) > $cursor` falls through to a
//     lexicographic skip — collation-dependent and almost never
//     what the caller wants.
//   - MarketsOrderVolume24hDesc: the predicate casts
//     `split_part($cursor, ':', 1)::numeric`, which raises a
//     Postgres "invalid input syntax for type numeric" error and
//     a 500. Burns CPU per request.
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
		// Volume prefix may be empty (last row had a null vol_usd).
		// Otherwise: digits with at most one '.', no leading sign.
		// Shared with the assets cursor rather than re-inlined: at least one
		// digit is required, so a lone "." fails. Two copies of a predicate
		// is two places to fix it and one place to forget.
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

// encodeMarketsCursor formats the last-row cursor for the active
// ordering — pair-only for MarketsOrderPair, `<vol>:<pair>` for
// MarketsOrderVolume24hDesc.
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

// distinctPairsActivityCTEs is the static head of the /v1/markets
// listing query: `d`, the 14-day active-pair set from prices_1d, and `h`,
// the trailing-24h prices_1m scan that carries the volume, trade count,
// fresh last_price and same-day membership. It takes no interpolation —
// only the bind parameters $1 (since), $4 (source), $5 (asset alias
// set) and $6/$7 (sacAliasFoldBind) — so it lives in one literal, and
// buildDistinctPairsQuery appends the `raw` and `canon` CTEs that depend
// on the canonical-orientation expressions. Why each CTE reads the view
// it reads is argued at length in buildDistinctPairsQuery.
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

// buildDistinctPairsQuery composes the per-pair-volume CTE +
// SELECT for DistinctPairs given the limit and ordering. Pulled
// out of DistinctPairs so the latter stays under the gocognit
// threshold and the two ordering branches are readable side-by-
// side.
//
// Cursor formats:
//   - MarketsOrderPair          — `<base>|<quote>`. Strict-greater
//     comparison resumes pagination.
//   - MarketsOrderVolume24hDesc — `<vol_or_blank>:<base>|<quote>`.
//     Empty vol prefix sorts as 0 (so cursor "":<pair> resumes
//     into the null-volume tail). The compare on (vol, pair) is
//     strict-less for vol DESC, strict-greater for pair ASC —
//     the standard keyset tuple-comparison trick is adapted to
//     mixed ordering.
func buildDistinctPairsQuery(since time.Time, source, asset, cursor string, limit int, order MarketsOrder) (string, []any) {
	// /v1/markets is a directory listing, not a history view: reading
	// prices_1m × 14 days (~52k pairs × 20,160 buckets) blew the 8s
	// handler ceiling. So the 14d-active-pair set comes from prices_1d,
	// and only the 24h figures read prices_1m (chunk-pruned to the last
	// day, so exact). A rolling 24h window is not bucket-additive over a
	// coarser CAGG: prices_1h understated 24h volume ~9%. Detail
	// endpoints are untouched.
	//
	// d and h are FULL OUTER JOINed because prices_1d (6h end_offset,
	// materialized_only) has no row yet for a pair whose first trade is
	// today, and its newest bucket is yesterday's close. last_price,
	// last_trade_at and bucket_close_at prefer h's fresh prices_1m
	// values, falling back to d only for pairs idle longer than 24h.
	// count_24h is COALESCE'd to 0 for 14d-active-but-24h-idle pairs.
	//
	// $5 is a text[] of the asset's alias forms: XLM is keyed `native`,
	// `crypto:XLM` or its SAC depending on venue, so a scalar match
	// omitted the other forms' markets.
	// canon collapses flipped orientations of the same market (XLM/USDC
	// and USDC/XLM — the SDEX decoder records both) into ONE row: USD
	// volume + trade count sum across both directions, and last_price is
	// the most-recent trade's price re-expressed in the canonical
	// orientation (inverted for the flipped direction). See
	// canonOrientSQL / canonical.Orient. `folded` first maps each SAC
	// spelling onto its classic form, so a market traded on SDEX and on a
	// Soroban venue is one row; orientation is decided on the folded pair.
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
		// Cursor: "<vol_or_blank>:<base>|<quote>". Two-tuple keyset
		// against (vol_24h, base|quote). COALESCE NULL→0 so a
		// 24h-idle pair sorts last under DESC (same effect as
		// NULLS LAST; real volumes are positive). The "next page"
		// relation is (v < cv) OR (v = cv AND pair > cpair) — DESC
		// on the first key flips the comparator, encoded explicitly
		// (identical to the prior HAVING form, now a WHERE since d/h
		// are already grouped).
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

// pairMarketQuery is the single-pair activity summary behind /v1/pairs.
// $1 is the MarketsRecencyWindow lower bound, $2/$3 the requested base
// and quote.
//
// LastTradeAt is sourced from trades (exact, second-precision) rather
// than from a CAGG bucket — that is the documented difference between
// this endpoint and /v1/markets, whose directory rows round to the
// minute, and it is preserved here. BucketCloseAt is recomputed from
// the same value in Go so the wire shape still matches /v1/markets.
//
// Combine BOTH stored directions of the market (the SDEX decoder
// records XLM/USDC and USDC/XLM as separate rows) into the requested
// ($2, $3) orientation, matching /v1/markets (canonOrientSQL) and
// LatestClosedVWAP1mForPair. count_24h + vol_24h_usd sum across
// directions (USD volume is orientation-independent); last_price is the
// most-recent bucket's price re-expressed in the requested orientation
// (inverted for the flipped direction). See canonical.Orient.
//
// Shape. Every both-directions read is a UNION ALL of two single-direction
// branches, and each aggregate reads the smallest window that produces it:
//
//   - `(A AND B) OR (B AND A)` cannot drive trades_pair_ts_idx /
//     prices_1m_pair_bucket_idx, so the planner falls back to the bare time
//     index (measured in [closedVWAP1mAtOrBeforeQuery]).
//   - MAX(ts) needs one backwards index probe per direction and count_24h
//     needs 24 hours; one shared 14-day aggregate read 17.2M rows for
//     crypto:BTC/crypto:USDT and ran 95.7s cold, past the 8s handler ceiling.
//
// The `, base_asset` tiebreaker on the last_price sort makes the order total
// once a bucket holds both orientations, as in [closedVWAP1mAtOrBeforeQuery].
// Guarded by TestPairMarketQueryShape.
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

// PairMarket returns the activity summary for a single (base, quote)
// pair. The bool result is false when the pair hasn't traded inside
// MarketsRecencyWindow; callers translate that to an empty list
// (200 OK) per the /v1/pairs envelope contract — not a 404 — to
// match the "array of MarketRow" spec shape.
//
// Recency window: scoped to the last MarketsRecencyWindow so chunk
// pruning bounds I/O on a hypertable with hundreds of millions of
// trades, and so the result is consistent with DistinctPairs (a
// pair that DistinctPairs hides should also be hidden here).
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

// PairVolumePoint is one hourly USD-volume sample for a single
// (base, quote) pair, used by the per-pair sparkline endpoint.
// Hour is the bucket start (UTC); VolumeUSD is numeric-stringified
// for full precision through the JSON boundary.
type PairVolumePoint struct {
	Hour      time.Time
	VolumeUSD string
}

// pairKey is the lookup key for the batched per-pair volume map.
// Wire shape stays string-based (`<base>|<quote>`) so the v1 API
// adapter doesn't have to import canonical.Pair.
type pairKey = string

// GetPairsVolumeHistory24hBatch returns per-(base, quote) hourly
// USD-volume buckets for the trailing 24h, suitable for the
// /markets / /v1/markets sparkline column.
//
// Unlike the per-source variant, this query reads volume_usd
// directly from prices_1m — pairs aggregated across all sources
// match the wire shape /v1/markets already returns. Each requested
// key sums BOTH stored orientations, as the listing's canon fold does
// for volume_24h_usd; one UNION ALL arm per direction keeps each an
// index equality. Each key also reads the SAC spellings the listing
// folded into it, so the bars sum to the row's volume_24h_usd. Holes are
// zero-filled so each per-pair series always has 24 entries oldest → newest.
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

// FirstTradeBatch returns, for each requested (base, quote) pair,
// the open time of the pair's FIRST daily bucket in prices_1d — the
// queryable "since inception = first recorded trade" anchor (RFP). Day precision is deliberate: prices_1d is indefinite
// (back to each pair's first trade) and the per-pair MIN is
// index-assisted, so this stays cheap enough for the ?include=
// opt-in path on /v1/markets. Both orientations of each pair are
// consulted and the earlier one wins (mirror-listed pairs), as are the
// SAC spellings the listing folded into the pair.
// Missing pairs are absent from the map.
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
         GROUP BY base_asset, quote_asset`
	rows, err := s.db.QueryContext(ctx, q, bases, quotes)
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
