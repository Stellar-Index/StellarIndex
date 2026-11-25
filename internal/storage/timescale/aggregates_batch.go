package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Set forms of the per-key price readers a /v1/price/batch prefetch makes. Each runs the single reader's statement text
// verbatim inside one LATERAL over every key, with only its $1/$2 legs bound to the key, and folds each key's rows
// with the single reader's fold, so a key's answer is the single call's. A failed statement fails the whole set: no
// key is ever reported absent because the set read broke.

// KeyResult is one key's answer from a set read: Value and Err exactly as the single-key reader returns them.
type KeyResult[T any] struct {
	Value T
	Err   error
}

// bucketLowerBound is the literal `bucket >=` clause; a literal, not a bind, gives TimescaleDB plan-time chunk exclusion.
func bucketLowerBound(since time.Time) string {
	return fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", since.UTC().Format("2006-01-02 15:04:05-07"))
}

// The per-key statement renderers, shared by each single reader and its set form. Only bucketLowerBound, our own
// time.Time in a fixed layout, is interpolated; everything else binds.

func recentClosedVWAP1mExistsSQL(since time.Time) string {
	return fmt.Sprintf(recentClosedVWAP1mExistsTemplate, bucketLowerBound(since)) //nolint:gosec // G201: see above
}

func latestClosedVWAP1mSQL(since time.Time) string {
	return fmt.Sprintf(latestClosedVWAP1mTemplate, bucketLowerBound(since)) //nolint:gosec // G201: see above
}

func recentClosedVWAP1mCombinedSQL(now time.Time) string {
	lower := bucketLowerBound(now.Add(-LatestVWAPWindow))
	return fmt.Sprintf(recentClosedVWAP1mCombinedTemplate, lower) //nolint:gosec // G201: see above
}

func closedVWAP1mCombinedBeforeSQL(before time.Time) string {
	lower := bucketLowerBound(before.Add(-LatestVWAPWindow))
	return fmt.Sprintf(closedVWAP1mCombinedBeforeTemplate, lower) //nolint:gosec // G201: see above
}

// pairMarketSubstanceSQL binds the spelling sets as $1/$2. The second arm's NOT stops a row both arms select (possible
// only when the two sets share a spelling) from counting twice.
func pairMarketSubstanceSQL(now time.Time, window time.Duration) string {
	return fmt.Sprintf(`
        SELECT COALESCE(sum(bucket_usd), 0)::text,
               count(*),
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0),
               count(*) FILTER (WHERE bucket_usd > 0)
          FROM (
            SELECT bucket, sum(volume_usd) AS bucket_usd
              FROM (
                SELECT bucket, volume_usd FROM prices_1m
                 WHERE base_asset = ANY($1) AND quote_asset = ANY($2)
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
                UNION ALL
                SELECT bucket, volume_usd FROM prices_1m
                 WHERE base_asset = ANY($2) AND quote_asset = ANY($1)
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
                   AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))
              ) d
             GROUP BY bucket
          ) b
    `, bucketLowerBound(now.Add(-window))) //nolint:gosec // G201: see above
}

// keyLegs rebinds a per-key statement's $1/$2 to the current key's columns; none of these statements reaches $10.
var (
	pairKeyLegs      = strings.NewReplacer("$1", "k.base", "$2", "k.quote")
	spellingsKeyLegs = strings.NewReplacer("$1", "k.bases", "$2", "k.quotes")
)

// vwapRowCols names a both-direction prices_1m statement's six output columns for the outer ORDER BY.
const vwapRowCols = "x(bucket, base_asset, vwap, volume_priced, trade_count, sources)"

// pairSetSQL runs single once per key: $1/$2 become text[] of bases and quotes, unnested WITH ORDINALITY so every row
// carries its key's 1-based index first. Rows come back key by key in the single statement's own order.
func pairSetSQL(single, cols, order string) string {
	return "SELECT k.ord, x.* FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS k(base, quote, ord)\n" +
		"CROSS JOIN LATERAL (" + pairKeyLegs.Replace(single) + ") AS " + cols + "\nORDER BY k.ord" + order
}

func vwapRowsSetSQL(single string) string {
	return pairSetSQL(single, vwapRowCols, ", x.bucket DESC, x.base_asset")
}

func existsSetSQL(single string) string { return pairSetSQL(single, "x(one)", "") }

// substanceSetSQL runs a spelling-set statement once per key: $1..$3 are flat (key ordinal, leg, spelling) arrays,
// regrouped into each key's base and quote sets.
func substanceSetSQL(single string) string {
	return `WITH k AS (
            SELECT ord, array_agg(spelling) FILTER (WHERE leg = 'b') AS bases,
                   array_agg(spelling) FILTER (WHERE leg = 'q') AS quotes
              FROM unnest($1::bigint[], $2::text[], $3::text[]) AS s(ord, leg, spelling)
             GROUP BY ord)
        SELECT k.ord, x.* FROM k CROSS JOIN LATERAL (` + spellingsKeyLegs.Replace(single) +
		`) AS x(volume_usd, buckets, span_seconds, valued_buckets)
        ORDER BY k.ord`
}

func pairLegArrays(pairs []canonical.Pair) (bases, quotes []string) {
	bases, quotes = make([]string, len(pairs)), make([]string, len(pairs))
	for i, p := range pairs {
		bases[i], quotes[i] = p.Base.String(), p.Quote.String()
	}
	return bases, quotes
}

// keyIndex turns a 1-based ordinality into an index of n keys; anything else is a broken statement, never a miss.
func keyIndex(ord int64, n int, what string) (int, error) {
	if ord < 1 || ord > int64(n) {
		return 0, fmt.Errorf("timescale: %s: key ordinal %d outside 1..%d", what, ord, n)
	}
	return int(ord - 1), nil
}

// vwapDirRow is one stored direction's prices_1m row as the both-direction statements return it.
type vwapDirRow struct {
	bucket       time.Time
	base         string
	vwap, volume string
	tc           int64
	sources      stringArray
}

// vwapBucketFold folds one pair's adjacent both-direction rows into one [Vwap1mRow] per bucket, in row order: VWAP via
// [combineDirVWAP], trade counts summed, sources unioned; limit (0 = unbounded) trims the partial tail of a
// [bucketRowCap] fetch.
type vwapBucketFold struct {
	base, quote string
	limit       int
	out         []Vwap1mRow
	cur         Vwap1mRow
	dirs        []dirVWAP
	open        bool
}

func newVwapBucketFold(p canonical.Pair, limit int) *vwapBucketFold {
	capHint := limit
	if capHint <= 0 {
		capHint = 16
	}
	return &vwapBucketFold{base: p.Base.String(), quote: p.Quote.String(), limit: limit, out: make([]Vwap1mRow, 0, capHint)}
}

func (f *vwapBucketFold) flush() {
	if !f.open {
		return
	}
	f.open = false
	vwap, ok := combineDirVWAP(f.dirs)
	if !ok {
		return
	}
	f.cur.VWAP = vwap
	normalizeVwapSources(&f.cur)
	f.out = append(f.out, f.cur)
}

func (f *vwapBucketFold) add(r vwapDirRow) {
	if !f.open || !r.bucket.Equal(f.cur.Bucket) {
		f.flush()
		f.cur = Vwap1mRow{Bucket: r.bucket, BaseAsset: f.base, QuoteAsset: f.quote}
		f.dirs, f.open = f.dirs[:0], true
	}
	f.dirs = append(f.dirs, dirVWAP{vwapText: r.vwap, volumeText: r.volume, flipped: r.base != f.base})
	f.cur.TradeCount += r.tc
	f.cur.Sources = appendSources(f.cur.Sources, r.sources)
}

func (f *vwapBucketFold) result() []Vwap1mRow {
	f.flush()
	if f.limit > 0 && len(f.out) > f.limit {
		f.out = f.out[:f.limit]
	}
	return f.out
}

// vwapRowsSet runs a both-direction per-key statement over every pair and folds each pair's rows as
// [scanCombinedVwap1mRows] does; args bind from $3 on, shared by every key.
func (s *Store) vwapRowsSet(ctx context.Context, single string, pairs []canonical.Pair, limit int, what string, args ...any) ([][]Vwap1mRow, error) {
	out := make([][]Vwap1mRow, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	bases, quotes := pairLegArrays(pairs)
	rows, err := s.db.QueryContext(ctx, vwapRowsSetSQL(single), append([]any{bases, quotes}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("timescale: %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	folds := make([]*vwapBucketFold, len(pairs))
	for i, p := range pairs {
		folds[i] = newVwapBucketFold(p, limit)
	}
	for rows.Next() {
		var (
			ord int64
			r   vwapDirRow
		)
		if err := rows.Scan(&ord, &r.bucket, &r.base, &r.vwap, &r.volume, &r.tc, &r.sources); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		i, err := keyIndex(ord, len(pairs), what)
		if err != nil {
			return nil, err
		}
		folds[i].add(r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s rows: %w", what, err)
	}
	for i, f := range folds {
		out[i] = f.result()
	}
	return out, nil
}

// firstRowPerKey is a single-row reader's per-key answer: the first folded bucket, or [sql.ErrNoRows] for none.
func firstRowPerKey(rows [][]Vwap1mRow) []KeyResult[Vwap1mRow] {
	out := make([]KeyResult[Vwap1mRow], len(rows))
	for i, r := range rows {
		if len(r) == 0 {
			out[i].Err = sql.ErrNoRows
			continue
		}
		out[i].Value = r[0]
	}
	return out
}

func (s *Store) recentClosedVWAP1mExistsSet(ctx context.Context, pairs []canonical.Pair, since time.Time) ([]bool, error) {
	out := make([]bool, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	bases, quotes := pairLegArrays(pairs)
	rows, err := s.db.QueryContext(ctx, existsSetSQL(recentClosedVWAP1mExistsSQL(since)), bases, quotes)
	if err != nil {
		return nil, fmt.Errorf("timescale: recentClosedVWAP1mExistsSet: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ord, one int64
		if err := rows.Scan(&ord, &one); err != nil {
			return nil, fmt.Errorf("timescale: recentClosedVWAP1mExistsSet scan: %w", err)
		}
		i, err := keyIndex(ord, len(pairs), "recentClosedVWAP1mExistsSet")
		if err != nil {
			return nil, err
		}
		out[i] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: recentClosedVWAP1mExistsSet rows: %w", err)
	}
	return out, nil
}

// RecentClosedVWAP1mExistsSet is [Store.RecentClosedVWAP1mExists] for every pair, aligned with pairs.
func (s *Store) RecentClosedVWAP1mExistsSet(ctx context.Context, pairs []canonical.Pair) ([]bool, error) {
	return s.recentClosedVWAP1mExistsSet(ctx, pairs, time.Now().UTC().Add(-latestVWAPGateWindow))
}

// LatestClosedVWAP1mForPairSet is [Store.LatestClosedVWAP1mForPair] for every pair, aligned with pairs: the same
// existence gate, then the latest-bucket read for the pairs that pass it.
func (s *Store) LatestClosedVWAP1mForPairSet(ctx context.Context, pairs []canonical.Pair) ([]KeyResult[Vwap1mRow], error) {
	exists, err := s.RecentClosedVWAP1mExistsSet(ctx, pairs)
	if err != nil {
		return nil, err
	}
	out := make([]KeyResult[Vwap1mRow], len(pairs))
	var (
		live []canonical.Pair
		at   []int
	)
	for i, ok := range exists {
		if !ok {
			out[i].Err = sql.ErrNoRows
			continue
		}
		live, at = append(live, pairs[i]), append(at, i)
	}
	cutoff := time.Now().UTC().Add(-LatestVWAPWindow)
	rows, err := s.vwapRowsSet(ctx, latestClosedVWAP1mSQL(cutoff), live, 1, "LatestClosedVWAP1mForPairSet")
	if err != nil {
		return nil, err
	}
	for j, r := range firstRowPerKey(rows) {
		out[at[j]] = r
	}
	return out, nil
}

// ClosedVWAP1mAtOrBeforeSet is [Store.ClosedVWAP1mAtOrBefore] at one instant for every pair, aligned with pairs.
func (s *Store) ClosedVWAP1mAtOrBeforeSet(ctx context.Context, pairs []canonical.Pair, t time.Time) ([]KeyResult[Vwap1mRow], error) {
	rows, err := s.vwapRowsSet(ctx, closedVWAP1mAtOrBeforeQuery, pairs, 1, "ClosedVWAP1mAtOrBeforeSet", t)
	if err != nil {
		return nil, err
	}
	return firstRowPerKey(rows), nil
}

// RecentClosedVWAP1mCombinedSet is [Store.RecentClosedVWAP1mCombined] for every pair, aligned with pairs.
func (s *Store) RecentClosedVWAP1mCombinedSet(ctx context.Context, pairs []canonical.Pair, limit int) ([][]Vwap1mRow, error) {
	if limit <= 0 {
		return make([][]Vwap1mRow, len(pairs)), nil
	}
	return s.vwapRowsSet(ctx, recentClosedVWAP1mCombinedSQL(time.Now()), pairs, limit, "RecentClosedVWAP1mCombinedSet",
		bucketRowCap(limit))
}

// ClosedVWAP1mCombinedBeforeSet is [Store.ClosedVWAP1mCombinedBefore] at one anchor for every pair, aligned with pairs.
func (s *Store) ClosedVWAP1mCombinedBeforeSet(
	ctx context.Context, pairs []canonical.Pair, before time.Time, limit int,
) ([][]Vwap1mRow, error) {
	if limit <= 0 {
		return make([][]Vwap1mRow, len(pairs)), nil
	}
	return s.vwapRowsSet(ctx, closedVWAP1mCombinedBeforeSQL(before), pairs, limit, "ClosedVWAP1mCombinedBeforeSet",
		bucketRowCap(limit), before.UTC())
}

// SubstanceRead is one [Store.PairMarketSubstance] key: the base and quote spelling sets.
type SubstanceRead struct {
	Bases, Quotes []canonical.Asset
}

// PairMarketSubstanceSet is [Store.PairMarketSubstance] over one window for every read, aligned with reads. A read
// the single call would reject (an empty spelling set) gets that error and is not queried. The spelling sets travel
// as flat (key, leg, spelling) arrays regrouped per key, since Postgres cannot unnest an array of arrays per row.
func (s *Store) PairMarketSubstanceSet(ctx context.Context, reads []SubstanceRead, window time.Duration) ([]KeyResult[MarketSubstance], error) {
	if window <= 0 {
		return nil, fmt.Errorf("timescale: PairMarketSubstanceSet: non-positive window %v", window)
	}
	out := make([]KeyResult[MarketSubstance], len(reads))
	var (
		ords          []int64
		legs, spellgs []string
	)
	for i, r := range reads {
		baseKeys, quoteKeys, err := substanceLegs("PairMarketSubstance", r.Bases, r.Quotes)
		if err != nil {
			out[i].Err = err
			continue
		}
		for _, k := range baseKeys {
			ords, legs, spellgs = append(ords, int64(i+1)), append(legs, "b"), append(spellgs, k)
		}
		for _, k := range quoteKeys {
			ords, legs, spellgs = append(ords, int64(i+1)), append(legs, "q"), append(spellgs, k)
		}
	}
	if len(ords) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, substanceSetSQL(pairMarketSubstanceSQL(time.Now(), window)), ords, legs, spellgs)
	if err != nil {
		return nil, fmt.Errorf("timescale: PairMarketSubstanceSet: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var (
			ord int64
			sub MarketSubstance
		)
		if err := rows.Scan(&ord, &sub.VolumeUSD, &sub.Buckets, &sub.SpanSeconds, &sub.ValuedBuckets); err != nil {
			return nil, fmt.Errorf("timescale: PairMarketSubstanceSet scan: %w", err)
		}
		i, err := keyIndex(ord, len(reads), "PairMarketSubstanceSet")
		if err != nil {
			return nil, err
		}
		out[i].Value = sub
		seen++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PairMarketSubstanceSet rows: %w", err)
	}
	// The aggregate answers every key exactly once; a key without its row would read as an empty market.
	if want := countValid(out); seen != want {
		return nil, fmt.Errorf("timescale: PairMarketSubstanceSet: %d rows for %d keys", seen, want)
	}
	return out, nil
}

func countValid(out []KeyResult[MarketSubstance]) int {
	n := 0
	for _, r := range out {
		if r.Err == nil {
			n++
		}
	}
	return n
}
