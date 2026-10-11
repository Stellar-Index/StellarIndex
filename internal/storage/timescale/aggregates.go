package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// HistoryPoint is a single bucket's worth of aggregated price +
// volume, returned by [Store.HistoryPoints]. Wire shape mirrors the
// API's `{t, p, v_usd}` triple — the API binary's adapter is a
// pure passthrough.
type HistoryPoint struct {
	Bucket    time.Time
	VWAP      string   // NUMERIC text from Postgres
	VolumeUSD *string  // NULL when no usd_volume column entries — e.g. early classic-only ledgers
	Sources   []string // sorted union of the bucket's `sources`
}

// HistoryGranularity is the CAGG selector for [Store.HistoryPoints].
// Stable string values matching the API's `granularity` query
// parameter and the migration's table names (prices_<granularity>).
type HistoryGranularity string

const (
	Granularity1m  HistoryGranularity = "1m"
	Granularity15m HistoryGranularity = "15m"
	Granularity1h  HistoryGranularity = "1h"
	Granularity4h  HistoryGranularity = "4h"
	Granularity1d  HistoryGranularity = "1d"
	Granularity1w  HistoryGranularity = "1w"
	Granularity1mo HistoryGranularity = "1mo"
)

// AllHistoryGranularities is the ONE declaration of the served set, finest first.
// [HistoryGranularity.Validate] and its rejection message ([HistoryGranularityList]) read it, as
// do the API's 400 bodies, so the accepted set and the enumeration shown to callers cannot drift.
//
// Adding a rung is adding a rung the backfill must materialise:
// TestCAGGsLiveForeverCoversEveryServedGranularity fails until [CAGGsLiveForever] gains the
// matching prices_<g> view. Otherwise backfilled ranges keep a permanent hole at the new
// resolution, since continuous-aggregate policies only roll forward.
var AllHistoryGranularities = []HistoryGranularity{
	Granularity1m, Granularity15m, Granularity1h, Granularity4h,
	Granularity1d, Granularity1w, Granularity1mo,
}

// historyGranularitySep separates the entries [HistoryGranularityList]
// renders. Named rather than inlined so the guard test can split on
// the same value: a test that hard-coded ", " would fail on a change
// of punctuation and report it as a missing granularity.
const historyGranularitySep = ", "

// HistoryGranularityList renders [AllHistoryGranularities] as the enumeration that Validate's
// error and the API's 400 bodies carry, so a new granularity reaches every message automatically.
func HistoryGranularityList() string {
	parts := make([]string, len(AllHistoryGranularities))
	for i, g := range AllHistoryGranularities {
		parts[i] = string(g)
	}
	return strings.Join(parts, historyGranularitySep)
}

// Validate reports whether g is one of the supported granularities
// ([AllHistoryGranularities]). Caller surfaces unknown granularities
// as 400.
func (g HistoryGranularity) Validate() error {
	for _, known := range AllHistoryGranularities {
		if g == known {
			return nil
		}
	}
	return fmt.Errorf("unknown granularity %q (want one of: %s)", g, HistoryGranularityList())
}

// closedBucketInterval is the Postgres INTERVAL the closed-bucket guard subtracts from now()
// (ADR-0015); it equals the bucket size. The calendar spellings ('1 day', '1 month') step on the
// UTC bucket grid only because [sessionConnConfig] pins every pool's session TimeZone to UTC.
func (g HistoryGranularity) closedBucketInterval() string {
	switch g {
	case Granularity1m:
		return "1 minute"
	case Granularity15m:
		return "15 minutes"
	case Granularity1h:
		return "1 hour"
	case Granularity4h:
		return "4 hours"
	case Granularity1d:
		return "1 day"
	case Granularity1w:
		return "1 week"
	case Granularity1mo:
		return "1 month"
	}
	return ""
}

// BucketDuration is the wall-clock width of one bucket: the value price surfaces stamp into
// `window_seconds`, and what [Store.ClosedVWAPAtOrBefore] adds to a bucket START to get its close
// instant. 1mo is a nominal 30 days (the CAGG is calendar-exact), close enough for staleness math
// and the resolution ladder. Zero for an unknown granularity.
func (g HistoryGranularity) BucketDuration() time.Duration {
	switch g {
	case Granularity1m:
		return time.Minute
	case Granularity15m:
		return 15 * time.Minute
	case Granularity1h:
		return time.Hour
	case Granularity4h:
		return 4 * time.Hour
	case Granularity1d:
		return 24 * time.Hour
	case Granularity1w:
		return 7 * 24 * time.Hour
	case Granularity1mo:
		return 30 * 24 * time.Hour
	}
	return 0
}

// Seconds is [BucketDuration] in whole seconds — the integer a price
// snapshot carries in `window_seconds`.
func (g HistoryGranularity) Seconds() int {
	return int(g.BucketDuration() / time.Second)
}

// dirVWAP is ONE stored market direction's contribution to a single CAGG bucket, as scanned from
// prices_<granularity>. The decoder keeps each trade in the venue's observed base/quote ordering
// (see [canonical.Trade] / [canonical.Orient]), so one market lands as BOTH (A,B) and (B,A) rows
// and every serving read must fold the two itself.
//
//   - vwapText and volumeText are the row's `vwap` and `volume_priced` NUMERIC columns as text
//     (no float round-trip). Both cover priceable rows only (both legs > 0): vwap = Σquote /
//     Σbase and volume_priced = Σbase, in THAT ROW's own orientation and raw (smallest-unit)
//     terms. `volume` also counts zero-leg rows, so it cannot weight vwap.
//   - flipped marks a row stored (quote, base) relative to the orientation the caller asked for.
type dirVWAP struct {
	vwapText   string
	volumeText string
	flipped    bool
}

// combinedVWAPSigDigits is how many SIGNIFICANT digits a combined VWAP renders with. Prices span
// roughly 1e2 down to 1e-12 and below (an 18-decimals token quoted in XLM), so precision must be
// RELATIVE (see [rateScaleFor]). 25 digits is past the ~16-20 fractional digits of the CAGG's own
// NUMERIC division, so rendering never drops precision the inputs carried.
const combinedVWAPSigDigits = 25

// formatCombinedVWAP renders an exact combined VWAP as a decimal string with trailing zeros
// trimmed (mirrors `formatCrossRate` on the API side). It is the ONLY lossy step in the combine;
// everything upstream is exact rational arithmetic.
func formatCombinedVWAP(r *big.Rat) string {
	s := r.FloatString(rateScaleFor(r, combinedVWAPSigDigits))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// combineDirVWAP folds the stored market directions of ONE bucket into the VWAP of their union in
// the requested (base, quote) orientation, as a NUMERIC-shaped decimal string.
//
// VWAP is Σ(quote leg) / Σ(base leg), so each row's legs are re-expressed in the REQUESTED
// orientation's units before the sums are divided:
//
//	requested base  leg = volume_priced          (flipped row: vwap × volume_priced)
//	requested quote leg = vwap × volume_priced   (flipped row: volume_priced)
//
// A trade-count-weighted mean of {vwap, 1/vwap_flipped} is not a VWAP: it weights a hundred dust
// trades above one whale. A single unflipped row is returned VERBATIM so served bytes match a
// single-direction read.
//
// Arithmetic is exact (math/big), never float64 and never a SQL `1.0 / vwap`, which would round
// the flipped leg before weighting. ok=false when no row carries a parseable, positive price and
// volume; callers treat that as "no data for this bucket".
func combineDirVWAP(rows []dirVWAP) (string, bool) {
	if len(rows) == 1 && !rows[0].flipped {
		if v, ok := new(big.Rat).SetString(rows[0].vwapText); !ok || v.Sign() <= 0 {
			return "", false
		}
		return rows[0].vwapText, true
	}
	baseLeg, quoteLeg := new(big.Rat), new(big.Rat)
	for _, r := range rows {
		vwap, ok := new(big.Rat).SetString(r.vwapText)
		if !ok || vwap.Sign() <= 0 {
			continue
		}
		vol, ok := new(big.Rat).SetString(r.volumeText)
		if !ok || vol.Sign() <= 0 {
			continue
		}
		// The row's OTHER leg, via Σquote = vwap · Σbase.
		other := new(big.Rat).Mul(vwap, vol)
		if r.flipped {
			baseLeg.Add(baseLeg, other)
			quoteLeg.Add(quoteLeg, vol)
			continue
		}
		baseLeg.Add(baseLeg, vol)
		quoteLeg.Add(quoteLeg, other)
	}
	if baseLeg.Sign() <= 0 || quoteLeg.Sign() <= 0 {
		return "", false
	}
	return formatCombinedVWAP(new(big.Rat).Quo(quoteLeg, baseLeg)), true
}

// dirTWAP is ONE stored market direction's contribution to a single TWAP CAGG bucket
// (twap_1h / twap_1d), as scanned from twap_<granularity>. Mirrors [dirVWAP], but the merge weight
// is the direction's minute COVERAGE (`sample_count`), not trade count (see [combineDirTWAP]).
//
//   - twapText is the row's `twap` NUMERIC column as text (no float round-trip), in THAT ROW's own
//     orientation: avg(prices_1m.twap) over the window's minute buckets.
//   - sampleCount is the number of those minute buckets: the exact denominator of the row's own
//     avg, and the weight the union merge needs.
//   - notionalCount is how many of those minutes cleared the $0.01 notional floor; zero means the
//     row's twap is the unfloored fallback.
//   - flipped marks a row stored (quote, base) relative to the orientation the caller asked for.
type dirTWAP struct {
	twapText      string
	sampleCount   int64
	notionalCount int64
	flipped       bool
}

// flooredDirTWAP applies migration 0166's notional-floor fallback across
// directions: when any direction cleared the floor, a direction whose twap
// is the unfloored fallback (dust or unpriced only) is dropped from the merge.
func flooredDirTWAP(rows []dirTWAP) []dirTWAP {
	floored := 0
	for _, r := range rows {
		if r.notionalCount > 0 {
			floored++
		}
	}
	if floored == 0 || floored == len(rows) {
		return rows
	}
	out := make([]dirTWAP, 0, floored)
	for _, r := range rows {
		if r.notionalCount > 0 {
			out = append(out, r)
		}
	}
	return out
}

// combineDirTWAP folds the stored market directions of ONE TWAP bucket into the requested
// (base, quote) orientation, as a NUMERIC-shaped decimal string.
//
// twap_1h / twap_1d are avg(prices_1m.twap): one equal observation per elapsed MINUTE, not per
// trade. The only correct direction weight is each direction's minute COVERAGE (`sample_count`);
// trade-count weighting is wrong by an unbounded factor.
//
//	combined = Σ(oriented_twap · sample_count) / Σ(sample_count)
//
// A flipped row is oriented by an EXACT rational 1/twap, never SQL `1.0 / twap`, which would
// round before weighting; a flipped leg thus contributes the harmonic mean of its minute prices,
// a second-order gap for 1h/1d. A single unflipped row is returned VERBATIM. Rows that fell back
// past the notional floor are dropped first whenever another direction cleared it
// ([flooredDirTWAP]). ok=false when no row carries a parseable, positive twap with a positive
// sample_count.
func combineDirTWAP(rows []dirTWAP) (string, bool) {
	rows = flooredDirTWAP(rows)
	if len(rows) == 1 && !rows[0].flipped {
		if v, ok := new(big.Rat).SetString(rows[0].twapText); !ok || v.Sign() <= 0 {
			return "", false
		}
		return rows[0].twapText, true
	}
	num, den := new(big.Rat), new(big.Rat)
	for _, r := range rows {
		twap, ok := new(big.Rat).SetString(r.twapText)
		if !ok || twap.Sign() <= 0 || r.sampleCount <= 0 {
			continue
		}
		oriented := twap
		if r.flipped {
			// Exact rational inverse — 1/twap, not a rounded SQL division.
			oriented = new(big.Rat).Inv(twap)
		}
		w := new(big.Rat).SetInt64(r.sampleCount)
		num.Add(num, new(big.Rat).Mul(oriented, w))
		den.Add(den, w)
	}
	if num.Sign() <= 0 || den.Sign() <= 0 {
		return "", false
	}
	return formatCombinedVWAP(new(big.Rat).Quo(num, den)), true
}

// appendSources appends src's entries to dst, skipping any already present: a served row's
// contributor list is the union of its per-direction `sources` arrays. A linear scan avoids a map
// allocation per bucket on the price hot path; ordering is settled by [normalizeVwapSources].
func appendSources(dst, src []string) []string {
	for _, s := range src {
		dup := false
		for _, have := range dst {
			if have == s {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, s)
		}
	}
	return dst
}

// bucketRowCap converts a caller's BUCKET limit into the SQL row limit a both-directions read
// needs. A bucket holds at most two rows (one per direction), so 2n+1 rows always contain the
// first n buckets COMPLETE; the +1 row can only start an (n+1)-th bucket, which the caller trims.
// Capping in SQL (not via a DISTINCT-buckets CTE) keeps the index-ordered plans untouched.
//
// Returns 0 for a non-positive limit, meaning "no LIMIT clause".
func bucketRowCap(limit int) int {
	if limit <= 0 {
		return 0
	}
	return 2*limit + 1
}

// HistoryPoints returns every CLOSED bucket for the pair from the CAGG matching `granularity`,
// ordered ASC, for /v1/history/since-inception. The in-progress bucket is excluded by a sargable
// `bucket <= now() - <granularity>` guard (ADR-0015). `limit` clamps the BUCKET count; 0 returns
// all.
//
// BOTH stored directions are read and folded into the requested ($1, $2) orientation by
// [combineDirVWAP], as [Store.LatestClosedVWAP1mForPair] and [Store.OHLCSeries] do; one
// orientation alone drops trades recorded the other way round (and empties a flipped-only pair).
//
// Returns an empty slice + nil error when the pair has no closed buckets yet.
func (s *Store) HistoryPoints(ctx context.Context, p canonical.Pair, granularity HistoryGranularity, limit int) ([]HistoryPoint, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	// `table` and `interval` come from a closed enum after Validate(), so the fmt.Sprintf is safe;
	// the pair binds via $1/$2.
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// The two orientations are a UNION ALL of single-direction branches, NOT one
	// `(A AND B) OR (B AND A)`: the planner cannot drive prices_*_pair_bucket_idx from an OR of two
	// different (base, quote) equality pairs (see prices_1m_direction_union_test.go). This read is
	// unauthenticated with no lower time bound.
	//
	// `LIMIT $3` repeats on each branch: the first `2n+1` merged rows are contained in the union
	// of each branch's first `2n+1`, and the per-branch cap stops a branch materialising in full.
	//
	// The closed-bucket guard stays SARGABLE: a function on the indexed column forfeits index
	// access and chunk pruning.
	//
	// `base_asset` is a sort tiebreaker: `bucket ASC` is not a total order once a bucket holds both
	// directions, and byte-identical cross-region serving should not rest on planner order.
	// [combineDirVWAP] is commutative, so the value is unchanged. With limit=0 and custom plans
	// the sort streams as Merge Append + Incremental Sort; history_points_test.go pins it.
	args := []any{p.Base.String(), p.Quote.String()}
	limitClause := ""
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		args = append(args, rowCap)
		limitClause = fmt.Sprintf("\n              LIMIT $%d", len(args))
	}
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate above.
	q := fmt.Sprintf(`
		SELECT * FROM (
		    (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text, volume_usd::text, sources
		       FROM %[1]s
		      WHERE base_asset = $1 AND quote_asset = $2
		        AND bucket <= now() - INTERVAL '%[2]s'
		      ORDER BY bucket ASC%[3]s)
		    UNION ALL
		    (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text, volume_usd::text, sources
		       FROM %[1]s
		      WHERE base_asset = $2 AND quote_asset = $1
		        AND bucket <= now() - INTERVAL '%[2]s'
		      ORDER BY bucket ASC%[3]s)
		) AS both_directions
		 ORDER BY bucket ASC, base_asset%[3]s
	`, table, interval, limitClause)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPoints[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanHistoryPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("HistoryPoints[%s]", granularity))
}

// scanHistoryPoints folds the both-directions rows of a prices_<granularity> read into one
// [HistoryPoint] per bucket, in query order. Rows of a bucket are adjacent (callers order by
// bucket), so one pass with no map suffices.
//
// `base` is the requested pair's base asset id; a row with a different base_asset is the flipped
// storage of the same market. `limit` is the caller's BUCKET limit (0 = unbounded): the query
// fetched [bucketRowCap] rows, so the first `limit` buckets are complete and the rest are trimmed.
// `what` labels wrapped errors.
func scanHistoryPoints(rows *sql.Rows, base string, limit int, what string) ([]HistoryPoint, error) {
	out := make([]HistoryPoint, 0, 1024)
	var (
		curBucket time.Time
		curDirs   []dirVWAP
		curUSD    []sql.NullString
		curSrcs   []string
		open      bool
	)
	flush := func() {
		if !open {
			return
		}
		open = false
		vwap, ok := combineDirVWAP(curDirs)
		if !ok {
			return
		}
		sort.Strings(curSrcs)
		out = append(out, HistoryPoint{
			Bucket:    curBucket,
			VWAP:      vwap,
			VolumeUSD: sumUSDVolume(curUSD),
			Sources:   curSrcs,
		})
	}
	for rows.Next() {
		var (
			bucket  time.Time
			rowBase string
			vwap    string
			volume  string
			vusd    sql.NullString
			srcs    stringArray
		)
		if err := rows.Scan(&bucket, &rowBase, &vwap, &volume, &vusd, &srcs); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		if !open || !bucket.Equal(curBucket) {
			flush()
			curBucket, curDirs, curUSD, curSrcs, open = bucket, curDirs[:0], curUSD[:0], nil, true
		}
		curSrcs = appendSources(curSrcs, srcs)
		curDirs = append(curDirs, dirVWAP{
			vwapText:   vwap,
			volumeText: volume,
			flipped:    rowBase != base,
		})
		curUSD = append(curUSD, vusd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s rows: %w", what, err)
	}
	flush()
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sumUSDVolume adds the per-direction volume_usd columns of one bucket. The two directions hold
// DISJOINT trades (each trade is filed once, in the venue's orientation), so USD volumes add with
// no double counting and, being USD already, need no orientation conversion.
//
// Returns nil when every column is NULL (the "no usd_volume entries" case [HistoryPoint]
// documents), and the single non-NULL value VERBATIM when only one direction contributes, keeping
// served bytes identical to a single-direction read. Sums render at the widest input scale, as a
// Postgres `SUM(numeric)` would.
func sumUSDVolume(vals []sql.NullString) *string {
	var (
		sum   *big.Rat
		only  string
		scale int
		n     int
	)
	for _, v := range vals {
		if !v.Valid {
			continue
		}
		r, ok := new(big.Rat).SetString(v.String)
		if !ok {
			continue
		}
		n++
		only = v.String
		if s := decimalScale(v.String); s > scale {
			scale = s
		}
		if sum == nil {
			sum = r
			continue
		}
		sum.Add(sum, r)
	}
	switch n {
	case 0:
		return nil
	case 1:
		return &only
	}
	s := sum.FloatString(scale)
	return &s
}

// decimalScale counts the fractional digits in a Postgres NUMERIC text
// value. Postgres never renders NUMERIC in exponent form, so a single
// '.' scan is exact.
func decimalScale(s string) int {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// HistoryPointsInRange is [HistoryPoints] with an explicit [from, to) bound on the bucket column,
// for /v1/chart's rolling window; the same closed-bucket guard applies. A zero `from` or `to`
// disables that bound and `limit` 0 returns all buckets.
//
// Both stored directions are combined into the requested orientation; see [Store.HistoryPoints]
// for why a one-direction read silently drops liquidity.
//
// Returns an empty slice + nil error when the pair has no closed buckets in the window.
func (s *Store) HistoryPointsInRange(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	limit int,
) ([]HistoryPoint, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// Args grow incrementally so the placeholder count matches the optional from/to/limit clauses.
	args := []any{p.Base.String(), p.Quote.String()}
	// Neither UNION branch inherits a predicate from the other, so every bound is spelled into each
	// (same discipline as [recentClosedVWAP1mCombinedTemplate]). The closed-bucket guard stays
	// sargable, with the interval on the RHS.
	bounds := "\n        AND bucket <= now() - INTERVAL '" + interval + "'"
	if !from.IsZero() {
		args = append(args, from.UTC())
		bounds += fmt.Sprintf("\n        AND bucket >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		bounds += fmt.Sprintf("\n        AND bucket < $%d", len(args))
	}
	limitClause := ""
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		args = append(args, rowCap)
		limitClause = fmt.Sprintf("\n              LIMIT $%d", len(args))
	}
	// UNION ALL of index-drivable branches, with the base_asset sort tiebreaker; see
	// [Store.HistoryPoints]. `from` is OPTIONAL here, so the read cannot rely on its range bound.
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate.
	q := fmt.Sprintf(`
		SELECT * FROM (
		    (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text, volume_usd::text, sources
		       FROM %[1]s
		      WHERE base_asset = $1 AND quote_asset = $2%[2]s
		      ORDER BY bucket ASC%[3]s)
		    UNION ALL
		    (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text, volume_usd::text, sources
		       FROM %[1]s
		      WHERE base_asset = $2 AND quote_asset = $1%[2]s
		      ORDER BY bucket ASC%[3]s)
		) AS both_directions
		 ORDER BY bucket ASC, base_asset%[3]s
	`, table, bounds, limitClause)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPointsInRange[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanHistoryPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("HistoryPointsInRange[%s]", granularity))
}

// twapGranularities is the set of granularities backed by a TWAP continuous aggregate: 1h for
// sub-daily timeframes, 1d for daily and up, so the TWAP CAGG surface stays at two views over
// prices_1m instead of mirroring all seven prices_* grains.
var twapGranularities = map[HistoryGranularity]struct{}{
	Granularity1h: {},
	Granularity1d: {},
}

// TWAPGranularitySupported reports whether granularity has a TWAP CAGG
// (twap_1h / twap_1d). Callers snap an arbitrary requested granularity
// onto one of these before calling [Store.TWAPPointsInRange].
func TWAPGranularitySupported(g HistoryGranularity) bool {
	_, ok := twapGranularities[g]
	return ok
}

// TWAPPointsInRange returns CLOSED time-weighted-average buckets for the pair from the
// twap_<granularity> CAGG, ordered ASC. It is the TWAP sibling of [Store.HistoryPointsInRange]
// (same [from, to) semantics, closed-bucket guard and `[]HistoryPoint` shape, the VWAP field
// carrying the TWAP value). Only 1h and 1d have a TWAP CAGG; anything else returns an error the
// API surfaces as a 400.
//
// The SDEX decoder records XLM/USDC and USDC/XLM as separate rows, so both stored directions are
// combined into the requested ($1, $2) orientation by [combineDirTWAP]: exact 1/twap inversion,
// weighted by minute COVERAGE (`sample_count`), not trade count. See canonical.Orient.
//
// Both directions are selected RAW and folded per bucket by [scanTWAPPoints], so `limit` is a
// BUCKET limit ([bucketRowCap] fetches 2n+1 rows to deliver n buckets). The closed-bucket guard
// is SARGABLE (ADR-0015), so range bounds plus this predicate prune chunks at plan time. Returns
// an empty slice + nil error when no closed TWAP buckets are in the window.
func (s *Store) TWAPPointsInRange(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	limit int,
) ([]HistoryPoint, error) {
	if !TWAPGranularitySupported(granularity) {
		return nil, fmt.Errorf("timescale: TWAPPointsInRange: unsupported granularity %q (twap CAGG exists for 1h, 1d)", granularity)
	}
	table := "twap_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// Both directions are selected RAW; [combineDirTWAP] folds them in Go (exact big.Rat, no SQL
	// `1.0/twap` rounding). A bucket has at most two rows, so this is a plain index-ordered scan.
	args := []any{p.Base.String(), p.Quote.String()}
	// Bounds are spelled into BOTH branches (neither inherits a predicate from the other); the
	// closed-bucket guard stays sargable.
	bounds := "\n        AND bucket <= now() - INTERVAL '" + interval + "'"
	if !from.IsZero() {
		args = append(args, from.UTC())
		bounds += fmt.Sprintf("\n        AND bucket >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		bounds += fmt.Sprintf("\n        AND bucket < $%d", len(args))
	}
	limitClause := ""
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		args = append(args, rowCap)
		limitClause = fmt.Sprintf("\n              LIMIT $%d", len(args))
	}
	// UNION ALL, not an OR: twap_* has the same (base_asset, quote_asset, bucket) index shape as
	// prices_*, and `from`/`to` are optional here too (see [Store.HistoryPoints]).
	// #nosec G201 — table + interval derive from the validated
	// twapGranularities set, not user input. See TWAPGranularitySupported.
	q := fmt.Sprintf(`
		SELECT * FROM (
		    (SELECT bucket, base_asset, twap::text, COALESCE(sample_count, 0), COALESCE(notional_sample_count, 0), volume_usd::text
		       FROM %[1]s
		      WHERE base_asset = $1 AND quote_asset = $2%[2]s
		      ORDER BY bucket ASC%[3]s)
		    UNION ALL
		    (SELECT bucket, base_asset, twap::text, COALESCE(sample_count, 0), COALESCE(notional_sample_count, 0), volume_usd::text
		       FROM %[1]s
		      WHERE base_asset = $2 AND quote_asset = $1%[2]s
		      ORDER BY bucket ASC%[3]s)
		) AS both_directions
		 ORDER BY bucket ASC, base_asset%[3]s
	`, table, bounds, limitClause)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: TWAPPointsInRange[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanTWAPPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("TWAPPointsInRange[%s]", granularity))
}

// scanTWAPPoints folds the both-directions rows of a twap_<granularity> read into one
// [HistoryPoint] per bucket (the VWAP field carries the TWAP value), in query order. It has the
// [scanHistoryPoints] shape with [combineDirTWAP]'s coverage-weighted fold and `sample_count` in
// place of `volume`.
//
// `base` is the requested pair's base asset id; a row with a different base_asset is the flipped
// storage of the same market. `limit` is the caller's BUCKET limit (0 = unbounded): the query
// fetched [bucketRowCap] rows, so the first `limit` buckets are complete and the rest are trimmed.
// `what` labels wrapped errors.
func scanTWAPPoints(rows *sql.Rows, base string, limit int, what string) ([]HistoryPoint, error) {
	out := make([]HistoryPoint, 0, 1024)
	var (
		curBucket time.Time
		curDirs   []dirTWAP
		curUSD    []sql.NullString
		open      bool
	)
	flush := func() {
		if !open {
			return
		}
		open = false
		twap, ok := combineDirTWAP(curDirs)
		if !ok {
			return
		}
		out = append(out, HistoryPoint{
			Bucket:    curBucket,
			VWAP:      twap,
			VolumeUSD: sumUSDVolume(curUSD),
		})
	}
	for rows.Next() {
		var (
			bucket  time.Time
			rowBase string
			twap    string
			sc      int64
			nsc     int64
			vusd    sql.NullString
		)
		if err := rows.Scan(&bucket, &rowBase, &twap, &sc, &nsc, &vusd); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		if !open || !bucket.Equal(curBucket) {
			flush()
			curBucket, curDirs, curUSD, open = bucket, curDirs[:0], curUSD[:0], true
		}
		curDirs = append(curDirs, dirTWAP{
			twapText:      twap,
			sampleCount:   sc,
			notionalCount: nsc,
			flipped:       rowBase != base,
		})
		curUSD = append(curUSD, vusd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s rows: %w", what, err)
	}
	flush()
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Vwap1mRow is one row from the prices_1m continuous aggregate (see
// migrations/0002_create_price_aggregates.up.sql). Bucket is the START of the 1-minute window.
type Vwap1mRow struct {
	Bucket     time.Time
	BaseAsset  string
	QuoteAsset string
	// VWAP is a decimal string exactly as Postgres serialises NUMERIC, avoiding a float
	// round-trip; handlers that need a number parse with big.Rat.
	VWAP       string
	TradeCount int64
	Sources    []string
}

// recentClosedVWAP1mForPairQuery is the newest-first closed-bucket read behind the SEP-40
// prices() passthrough.
//
// It reads BOTH stored orientations (the decoder keeps each trade in the venue's observed
// ordering, [dirVWAP]), since one orientation drops every minute that traded only the flipped way.
// They are a UNION ALL of index-drivable branches, not an OR; see [closedVWAP1mAtOrBeforeQuery].
// `LIMIT $3` is a ROW cap ([bucketRowCap]); [scanCombinedVwap1mRows] trims any tail.
//
// The guard is `bucket <= now() - INTERVAL '1 minute'`, NOT `bucket + INTERVAL '1 minute' <=
// now()`: a function on the indexed column blocks the bucket index and chunk pruning. It is a
// const so the sargability tests in closed_vwap_at_test.go can scan it.
//
// Deliberately NOT given the `bucket >=` lower bound or the 14-day existence gate
// [RecentClosedVWAP1mCombined] carries: both would change what this public endpoint SERVES (a
// dormant asset's last N closed buckets becoming empty), an owner decision, not a query-shape fix.
const recentClosedVWAP1mForPairQuery = `
        SELECT * FROM (
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket <= now() - INTERVAL '1 minute'
              ORDER BY bucket DESC
              LIMIT $3)
            UNION ALL
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket <= now() - INTERVAL '1 minute'
              ORDER BY bucket DESC
              LIMIT $3)
        ) AS both_directions
         ORDER BY bucket DESC, base_asset
         LIMIT $3
    `

// RecentClosedVWAP1mForPair returns up to `limit` most-recent CLOSED 1-minute buckets from
// prices_1m for the pair, newest first, each COMBINED across both stored directions so every row
// is the price of Base in Quote. Same closed-bucket guard as [LatestClosedVWAP1mForPair]
// (ADR-0015) and the same [combineDirVWAP] math.
//
// Returns an empty slice + nil error when the pair has no closed buckets in scope; the SEP-40
// endpoint tells "no observations" from "asset unknown" with an asset-existence check. The caller
// clamps `limit`.
func (s *Store) RecentClosedVWAP1mForPair(ctx context.Context, p canonical.Pair, limit int) ([]Vwap1mRow, error) {
	rows, err := s.db.QueryContext(ctx, recentClosedVWAP1mForPairQuery,
		p.Base.String(), p.Quote.String(), bucketRowCap(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCombinedVwap1mRows(rows, p, limit, "RecentClosedVWAP1mForPair")
}

// recentClosedVWAP1mCombinedTemplate is the newest-first, both-directions, LIMIT-N variant of
// [latestClosedVWAP1mTemplate]. `%[1]s` carries the literal `bucket >=` lower-bound clause for
// plan-time chunk pruning.
//
// It selects the RAW per-direction rows and leaves the combine to [combineDirVWAP] in Go: exact
// rational money math, not a NUMERIC expression whose `1.0 / vwap` would round the flipped leg
// before weighting. `LIMIT $3` is a ROW cap ([bucketRowCap]) that bounds the walk to the requested
// buckets while keeping a plain index-ordered scan (no CTE, GROUP BY or source unnest).
// `%[1]s` goes into BOTH branches because each must prune chunks independently. Both directions
// are a UNION ALL, not an OR; see [closedVWAP1mAtOrBeforeQuery].
const recentClosedVWAP1mCombinedTemplate = `
        SELECT * FROM (
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket <= now() - INTERVAL '1 minute'
                %[1]s
              ORDER BY bucket DESC
              LIMIT $3)
            UNION ALL
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket <= now() - INTERVAL '1 minute'
                %[1]s
              ORDER BY bucket DESC
              LIMIT $3)
        ) AS both_directions
         ORDER BY bucket DESC, base_asset
         LIMIT $3
    `

// RecentClosedVWAP1mCombined returns up to `limit` most-recent CLOSED 1-minute buckets from
// prices_1m for the pair, newest-first, each COMBINED across both stored directions with
// [combineDirVWAP] (the math [LatestClosedVWAP1mForPair] serves) so every row is the price of Base
// in Quote.
//
// It is the trailing-baseline source for the /v1/price serving-sanity guard
// (aggregate.GuardServedVWAP). The baseline MUST use the same combine as the served candidate, or
// the two disagree on every two-sided market and the guard false-positives.
//
// It is called ONLY after [LatestClosedVWAP1mForPair] has confirmed the pair is populated, so this
// LIMIT-N read never pays the empty-pair cold walk; it still carries the literal `bucket >=` lower
// bound for plan-time chunk pruning. Returns an empty slice + nil error when nothing is in scope.
func (s *Store) RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]Vwap1mRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-latestVWAPWindow)
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", cutoff.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — only `lower` is interpolated, built from our own time.Time in a fixed layout;
	// pair strings + limit bind as $1/$2/$3.
	q := fmt.Sprintf(recentClosedVWAP1mCombinedTemplate, lower) //nolint:gosec // G201: see note above
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String(), bucketRowCap(limit))
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mCombined: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCombinedVwap1mRows(rows, p, limit, "RecentClosedVWAP1mCombined")
}

// closedVWAP1mCombinedBeforeTemplate is [recentClosedVWAP1mCombinedTemplate] anchored at an
// instant instead of now(): only buckets STRICTLY older than $4 qualify. `%[1]s` is the literal
// `bucket >=` lower bound, injected into both branches for plan-time chunk pruning.
const closedVWAP1mCombinedBeforeTemplate = `
        SELECT * FROM (
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket < $4::timestamptz
                %[1]s
              ORDER BY bucket DESC
              LIMIT $3)
            UNION ALL
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket < $4::timestamptz
                %[1]s
              ORDER BY bucket DESC
              LIMIT $3)
        ) AS both_directions
         ORDER BY bucket DESC, base_asset
         LIMIT $3
    `

// ClosedVWAP1mCombinedBefore is [RecentClosedVWAP1mCombined] anchored at `before`: up to `limit`
// closed prices_1m buckets whose START is strictly older than `before`, newest-first, combined
// with the same [combineDirVWAP] the served candidate uses.
//
// It is the trailing baseline for a HISTORICAL candidate bucket (the point-in-time serving
// guard): the now-anchored read returns only the newest buckets, none of which precede an older
// candidate, so the guard would pass it unjudged. The lower bound is [latestVWAPWindow] behind
// `before`, mirroring the latest-bucket readers. Returns an empty slice + nil error when no closed
// bucket precedes `before`.
func (s *Store) ClosedVWAP1mCombinedBefore(ctx context.Context, p canonical.Pair, before time.Time, limit int) ([]Vwap1mRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	cutoff := before.UTC().Add(-latestVWAPWindow)
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", cutoff.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — only `lower` is interpolated, built from our own time.Time in a fixed layout;
	// pair strings, limit and the anchor bind as $1..$4.
	q := fmt.Sprintf(closedVWAP1mCombinedBeforeTemplate, lower) //nolint:gosec // G201: see note above
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String(), bucketRowCap(limit), before.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: ClosedVWAP1mCombinedBefore: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCombinedVwap1mRows(rows, p, limit, "ClosedVWAP1mCombinedBefore")
}

// scanCombinedVwap1mRows folds the both-directions rows of a prices_1m read into one [Vwap1mRow]
// per bucket, in query order. Rows of a bucket are adjacent (callers order by bucket), so one pass
// with no map suffices.
//
// Per bucket the VWAP is the volume-weighted union of the directions ([combineDirVWAP]), the trade
// count their sum, and the sources their de-duplicated union. `limit` is the caller's BUCKET limit
// (0 = unbounded): the query fetched [bucketRowCap] rows, so the first `limit` buckets are
// complete and a partial tail is trimmed. `what` labels wrapped errors.
func scanCombinedVwap1mRows(rows *sql.Rows, p canonical.Pair, limit int, what string) ([]Vwap1mRow, error) {
	base := p.Base.String()
	capHint := limit
	if capHint <= 0 {
		capHint = 16
	}
	out := make([]Vwap1mRow, 0, capHint)
	var (
		cur  Vwap1mRow
		dirs []dirVWAP
		open bool
	)
	flush := func() {
		if !open {
			return
		}
		open = false
		vwap, ok := combineDirVWAP(dirs)
		if !ok {
			return
		}
		cur.VWAP = vwap
		normalizeVwapSources(&cur)
		out = append(out, cur)
	}
	for rows.Next() {
		var (
			bucket  time.Time
			rowBase string
			vwap    string
			volume  string
			tc      int64
			src     stringArray
		)
		if err := rows.Scan(&bucket, &rowBase, &vwap, &volume, &tc, &src); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		if !open || !bucket.Equal(cur.Bucket) {
			flush()
			cur = Vwap1mRow{Bucket: bucket, BaseAsset: base, QuoteAsset: p.Quote.String()}
			dirs, open = dirs[:0], true
		}
		dirs = append(dirs, dirVWAP{
			vwapText:   vwap,
			volumeText: volume,
			flipped:    rowBase != base,
		})
		cur.TradeCount += tc
		cur.Sources = appendSources(cur.Sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s rows: %w", what, err)
	}
	flush()
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// closedVWAP1mAtOrBeforeQuery is the both-directions anchor read behind /v1/assets/{id}'s
// change_24h_pct.
//
// It reads BOTH stored orientations: one alone would return a single leg's VWAP for a
// two-direction bucket (a silently wrong number) and match nothing for a flipped-only bucket.
//
// `LIMIT 2` is the row cap for ONE bucket; if the newest bucket has a single row, the second row
// belongs to an older bucket and [scanCombinedVwap1mRows]'s limit=1 trim discards it. The shape is
// a UNION ALL of two single-direction branches, NOT one `(A AND B) OR (B AND A)`. Each branch is
// an exact prefix match on prices_1m_pair_bucket_idx (base_asset, quote_asset, bucket DESC), so it
// walks newest-first and stops at its LIMIT. The OR form cannot drive that index: the planner
// falls back to the plain bucket index with the pair as a filter, and for a pair with NO rows the
// LIMIT never fills, so it walks every chunk. Guarded by TestBothDirectionReadersUseUnionNotOr.
const closedVWAP1mAtOrBeforeQuery = `
        SELECT * FROM (
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket <= $3::timestamptz - INTERVAL '1 minute'
              ORDER BY bucket DESC
              LIMIT 2)
            UNION ALL
            (SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
                    COALESCE(trade_count, 0), sources
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket <= $3::timestamptz - INTERVAL '1 minute'
              ORDER BY bucket DESC
              LIMIT 2)
        ) AS both_directions
         ORDER BY bucket DESC, base_asset
         LIMIT 2
    `

// ClosedVWAP1mAtOrBefore returns the most-recent CLOSED 1-minute prices_1m bucket for the pair
// whose end (`bucket + 1 minute`) is at or before t, COMBINED across both stored directions so the
// VWAP is the price of Base in Quote. It anchors change_24h_pct's 24-hours-ago price.
//
// Same closed-bucket guard as [LatestClosedVWAP1mForPair] (ADR-0015). Returns [sql.ErrNoRows] when
// no closed bucket exists at-or-before t (pair first traded < 24h ago, or the 90 d prices_1m
// retention elided the row) and when the VWAP will not parse as a positive rational, so an
// unusable anchor reads as absent rather than flowing into a percentage.
func (s *Store) ClosedVWAP1mAtOrBefore(ctx context.Context, p canonical.Pair, t time.Time) (Vwap1mRow, error) {
	rows, err := s.db.QueryContext(ctx, closedVWAP1mAtOrBeforeQuery,
		p.Base.String(), p.Quote.String(), t,
	)
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: ClosedVWAP1mAtOrBefore: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, err := scanCombinedVwap1mRows(rows, p, 1, "ClosedVWAP1mAtOrBefore")
	if err != nil {
		return Vwap1mRow{}, err
	}
	if len(out) == 0 {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	return out[0], nil
}

// VWAPAtRow is the result of [Store.ClosedVWAPAtOrBefore]: the VWAP of one closed bucket
// at-or-before a historical instant, plus which CAGG resolution served it. VWAP is a NUMERIC
// decimal string (no float round-trip). Bucket is the window START; the END is
// `Bucket + Resolution.BucketDuration()`.
type VWAPAtRow struct {
	VWAP       string
	Bucket     time.Time
	Resolution HistoryGranularity
}

// PriceAtMinuteRungMaxAge is how far behind now a point-in-time lookup may be and still be
// answered from a raw prices_1m bucket, the first boundary of [priceAtResolutionLadder]. Exported
// because the thin-market gate (pricingguard.SubstanceGate.AllowedAt) holds a minute-served
// instant to the minute floor and an hour/day-served one to the hour floor, so the two boundaries
// must be one number.
const PriceAtMinuteRungMaxAge = 48 * time.Hour

// priceAtResolutionLadder returns the CAGG resolutions to probe for a point-in-time lookup `age`
// behind now, FINEST first. It encodes the served-tier reality (ADR-0034): prices_1m holds the
// recent working set (~2 days), prices_1h reaches back weeks, prices_1d spans to 2015. Coarser
// rungs are fallbacks so an old instant still resolves when finer CAGGs never held or have aged
// out that row. Pure, so it is unit-tested without a DB.
func priceAtResolutionLadder(age time.Duration) []HistoryGranularity {
	switch {
	case age <= PriceAtMinuteRungMaxAge:
		return []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}
	case age <= 45*24*time.Hour:
		return []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}
	default:
		return []HistoryGranularity{Granularity1d}
	}
}

// ClosedVWAPAtOrBefore returns the closed VWAP bucket at-or-before `ts` for the pair from the
// FINEST CAGG resolution whose nearest bucket closes within `maxStaleness` of ts. It is the shared
// point-in-time engine behind /v1/price/at and /v1/price/changes.
//
// It walks [priceAtResolutionLadder] finest-first and accepts the first rung whose nearest closed
// bucket closes (bucket + resolution) within maxStaleness. A dead-market gap in prices_1m falls
// through to a fresher coarser bar rather than fabricating continuity, and the accepted rung is
// reported in Resolution so callers label `window_seconds` honestly.
//
// Both stored directions are combined, flipped rows inverted, so the answer is always the pair's
// base in its quote, as [Store.LatestClosedVWAP1mForPair] and [Store.OHLCSeries] do.
//
// Returns [sql.ErrNoRows] when no rung has an in-window bucket (pair younger than ts, ts predates
// history, or the nearest observation is staler than maxStaleness on every rung).
func (s *Store) ClosedVWAPAtOrBefore(ctx context.Context, p canonical.Pair, ts time.Time, maxStaleness time.Duration) (VWAPAtRow, error) {
	age := time.Since(ts)
	if age < 0 {
		age = 0
	}
	for _, g := range priceAtResolutionLadder(age) {
		bucket, vwap, found, err := s.closedVWAPAtOrBeforeRes(ctx, p, ts, maxStaleness, g)
		if err != nil {
			return VWAPAtRow{}, err
		}
		if !found {
			continue
		}
		if ts.Sub(bucket.Add(g.BucketDuration())) <= maxStaleness {
			return VWAPAtRow{VWAP: vwap, Bucket: bucket, Resolution: g}, nil
		}
	}
	return VWAPAtRow{}, sql.ErrNoRows
}

// closedVWAPAtOrBeforeQueryTemplate finds the nearest closed bucket at-or-before an instant for
// one CAGG resolution and returns its RAW per-direction rows; [combineDirVWAP] folds them in Go
// with exact rational arithmetic.
//
//	%[1]s - table name (prices_<granularity>, from the validated enum)
//	%[2]s - upper bound literal (ts - resolution: the closed-bucket guard
//	        `bucket + resolution <= ts` written sargably as `bucket <= ts - resolution`)
//	%[3]s - lower bound literal (plan-time chunk exclusion; set below any bucket
//	        ClosedVWAPAtOrBefore would accept)
//
// Both bounds are literal constants so the planner prunes chunks at PLAN time (a bind parameter
// would force a whole-history plan, as in [Store.LatestClosedVWAP1mForPair]). The pair strings
// stay bound parameters ($1/$2).
const closedVWAPAtOrBeforeQueryTemplate = `
    WITH latest AS (
        SELECT max(b) AS b FROM (
            SELECT max(bucket) AS b FROM %[1]s
             WHERE base_asset = $1 AND quote_asset = $2
               AND bucket <= TIMESTAMPTZ '%[2]s'
               AND bucket >= TIMESTAMPTZ '%[3]s'
            UNION ALL
            SELECT max(bucket) AS b FROM %[1]s
             WHERE base_asset = $2 AND quote_asset = $1
               AND bucket <= TIMESTAMPTZ '%[2]s'
               AND bucket >= TIMESTAMPTZ '%[3]s'
        ) u
    ),
    r AS (
        SELECT base_asset, vwap::text AS vwap, COALESCE(volume_priced, 0)::text AS volume_priced
          FROM %[1]s
         WHERE bucket = (SELECT b FROM latest)
           AND bucket >= TIMESTAMPTZ '%[3]s'
           AND base_asset = $1 AND quote_asset = $2
        UNION ALL
        SELECT base_asset, vwap::text AS vwap, COALESCE(volume_priced, 0)::text AS volume_priced
          FROM %[1]s
         WHERE bucket = (SELECT b FROM latest)
           AND bucket >= TIMESTAMPTZ '%[3]s'
           AND base_asset = $2 AND quote_asset = $1
    )
    SELECT (SELECT b FROM latest), base_asset, vwap, volume_priced
      FROM r
`

// closedVWAPAtOrBeforeRes runs [closedVWAPAtOrBeforeQueryTemplate] for one resolution. found=false
// (nil error) means no closed bucket in the pruning window, so the caller falls to a coarser rung;
// the lower bound sits below any acceptable bucket, so it never wrongly excludes one.
func (s *Store) closedVWAPAtOrBeforeRes(
	ctx context.Context, p canonical.Pair, ts time.Time, maxStaleness time.Duration, g HistoryGranularity,
) (time.Time, string, bool, error) {
	if err := g.Validate(); err != nil {
		return time.Time{}, "", false, err
	}
	res := g.BucketDuration()
	// Closed-bucket guard, sargable form: bucket + res <= ts  ⇒  bucket <= ts − res.
	upper := ts.Add(-res)
	// Lower bound strictly below the oldest acceptable bucket START (ts - maxStaleness - res); the
	// extra res of slack keeps a borderline bucket in the window so the Go-side staleness check,
	// not chunk pruning, rejects it.
	lower := ts.Add(-(maxStaleness + 2*res))
	const layout = "2006-01-02 15:04:05-07"
	table := "prices_" + string(g)
	// #nosec G201 — only `table` (from the validated enum) and two of our own time.Time bounds in
	// a fixed layout are interpolated. Pair strings bind as $1/$2.
	q := fmt.Sprintf(closedVWAPAtOrBeforeQueryTemplate,
		table, upper.UTC().Format(layout), lower.UTC().Format(layout))

	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String())
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("timescale: ClosedVWAPAtOrBefore[%s]: %w", g, err)
	}
	defer func() { _ = rows.Close() }()

	base := p.Base.String()
	var bucket time.Time
	dirs := make([]dirVWAP, 0, 2)
	for rows.Next() {
		var rowBase, vwap, volume string
		if err := rows.Scan(&bucket, &rowBase, &vwap, &volume); err != nil {
			return time.Time{}, "", false, fmt.Errorf("timescale: ClosedVWAPAtOrBefore[%s] scan: %w", g, err)
		}
		dirs = append(dirs, dirVWAP{vwapText: vwap, volumeText: volume, flipped: rowBase != base})
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, "", false, fmt.Errorf("timescale: ClosedVWAPAtOrBefore[%s] rows: %w", g, err)
	}
	vwap, ok := combineDirVWAP(dirs)
	if !ok {
		// No bucket on this rung, or one with no usable price/volume: no data, fall to a coarser rung.
		return time.Time{}, "", false, nil
	}
	return bucket, vwap, true, nil
}

// LatestClosedVWAP1mForPair returns the most-recent CLOSED 1-minute prices_1m bucket for the
// pair. The API serves only closed buckets (ADR-0015), so the in-progress bucket is excluded
// explicitly even though the CAGG refresh policy already drops it.
//
// Returns [sql.ErrNoRows] when the pair has no closed bucket yet; callers translate that to the
// price-not-found problem or fall back to the latest-trade path.
func (s *Store) LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (Vwap1mRow, error) {
	// Combine BOTH stored directions: the SDEX decoder records XLM/USDC and USDC/XLM, so one
	// direction halves the liquidity. Flipped rows are inverted and weighted within the latest
	// closed bucket.
	//
	// PERF (both layers required):
	//  1. The closed-bucket predicate MUST be `bucket <= now() - 1min`, not `bucket + 1min <=
	//     now()`; a function on the indexed column makes max() scan every chunk (446ms -> 26ms).
	//  2. `now()` is only known at run time, so the planner still enumerates all ~374 chunks
	//     (~280ms). A LITERAL lower bound computed in Go excludes old chunks at plan time (~2ms).
	//
	// No unbounded fallback: the handler probes the zero-row native/fiat:USD pair on every XLM
	// query, so a fallback would make every miss an all-chunk scan; ErrNoRows sends it to its
	// triangulation chain instead. Proving a pair EMPTY touches every chunk in the ~400-day window
	// (minutes COLD), so a cheap existence probe over latestVWAPGateWindow (~2 weeks of hot chunks)
	// runs first; a gate hit returns the same bucket.
	gateSince := time.Now().UTC().Add(-latestVWAPGateWindow)
	exists, err := s.recentClosedVWAP1mExists(ctx, p, gateSince)
	if err != nil {
		return Vwap1mRow{}, err
	}
	if !exists {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	cutoff := time.Now().UTC().Add(-latestVWAPWindow)
	return s.latestClosedVWAP1m(ctx, p, cutoff)
}

// latestVWAPWindow bounds the lookback for [LatestClosedVWAP1mForPair]. A LITERAL cutoff this far
// back is interpolated so the planner prunes old chunks at PLAN time (~6ms vs ~280ms unbounded).
// 400 days covers every recently-traded pair while excluding most of prices_1m's decade of chunks.
const latestVWAPWindow = 400 * 24 * time.Hour

// latestVWAPGateWindow bounds the cheap recent-existence probe [LatestClosedVWAP1mForPair] runs
// BEFORE its value walk. It is the price surface's freshness horizon: a pair with no closed
// 1-minute bucket in two weeks is "not currently priced", so the read returns [sql.ErrNoRows] and
// /v1/price uses its triangulation / last-trade fallback. Two weeks is generous for a minutes-fresh
// contract, yet PROVING a pair empty touches only recent (hot, mostly-uncompressed) chunks instead
// of the ~400-day span. It MUST stay < [latestVWAPWindow] so that on a gate HIT the value walk
// always finds the just-confirmed bucket.
const latestVWAPGateWindow = 14 * 24 * time.Hour

// recentClosedVWAP1mExistsTemplate is the recent-existence gate query. `%[1]s` carries the literal
// lower-bound clause for plan-time chunk pruning. It probes BOTH stored directions: a
// one-direction probe could miss a flipped-only bucket and wrongly gate a live pair to ErrNoRows.
// LIMIT 1 lets a populated pair short-circuit at its first row instead of scanning the window.
const recentClosedVWAP1mExistsTemplate = `
        SELECT 1 FROM (
            (SELECT 1 FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket <= now() - INTERVAL '1 minute'
                %[1]s
              LIMIT 1)
            UNION ALL
            (SELECT 1 FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket <= now() - INTERVAL '1 minute'
                %[1]s
              LIMIT 1)
        ) u
         LIMIT 1
    `

// recentClosedVWAP1mExists reports whether the pair has at least one CLOSED 1-minute bucket (in
// either stored direction) at or after `since`: the cheap gate in front of
// [LatestClosedVWAP1mForPair]'s value walk. A miss returns (false, nil) so the caller returns
// [sql.ErrNoRows]. The sargable closed-bucket guard and the literal lower bound both prune chunks,
// so proving a pair empty touches only recent chunks.
func (s *Store) recentClosedVWAP1mExists(ctx context.Context, p canonical.Pair, since time.Time) (bool, error) {
	// A literal lower bound (our own timestamp) gives PLAN-time chunk exclusion; a $N bind
	// parameter would force a whole-history plan.
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", since.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — the only interpolated value is `lower`, built from our
	// own time.Time in a fixed layout; the pair strings bind as $1/$2.
	q := fmt.Sprintf(recentClosedVWAP1mExistsTemplate, lower) //nolint:gosec // G201: see note above
	var one int
	err := s.db.QueryRowContext(ctx, q, p.Base.String(), p.Quote.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: recentClosedVWAP1mExists: %w", err)
	}
	return true, nil
}

// RecentClosedVWAP1mExists reports whether the pair has at least one CLOSED 1-minute VWAP bucket
// (in either stored direction) within [latestVWAPGateWindow] (two weeks). It exports the gate
// [LatestClosedVWAP1mForPair] runs internally for the /v1/price stablecoin-proxy fallback.
//
// The proxy needs it because `tryStablecoinFiatProxy` calls `LatestPrice(asset, <peg>)` for each
// USD-pegged classic. A peg's quote is a classic asset, so on a VWAP miss `LatestPrice` skips the
// synthetic-fiat fast path and falls to [Store.LatestTradesForPair], an UNBOUNDED last-trade scan.
// For an empty proxy pair (a pure-Soroban token that trades only vs XLM, so <token>/USDC has zero
// rows) that is a cold full-history walk, repeated for EVERY peg. Gating each peg on this bounded
// probe skips empty pairs first. A hit means a live pair; a miss means skip the peg.
func (s *Store) RecentClosedVWAP1mExists(ctx context.Context, p canonical.Pair) (bool, error) {
	since := time.Now().UTC().Add(-latestVWAPGateWindow)
	return s.recentClosedVWAP1mExists(ctx, p, since)
}

// latestClosedVWAP1m runs the combined-direction latest-closed-bucket query with a LITERAL
// `bucket >= since` lower bound on every prices_1m scan, so the planner prunes old chunks at PLAN
// time (see [LatestClosedVWAP1mForPair]). Returns [sql.ErrNoRows] when no closed bucket exists in
// the window.
//
// latestClosedVWAP1mTemplate has a single `%[1]s` slot for the literal lower-bound clause,
// repeated on all three prices_1m scans. The final SELECT returns the latest bucket's RAW
// per-direction rows (at most two); [combineDirVWAP] folds them in Go with exact rational
// arithmetic rather than a NUMERIC expression that would round `1.0 / vwap` before weighting.
const latestClosedVWAP1mTemplate = `
        WITH latest AS (
            SELECT max(b) AS b FROM (
                SELECT max(bucket) AS b FROM prices_1m
                 WHERE base_asset = $1 AND quote_asset = $2
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
                UNION ALL
                SELECT max(bucket) AS b FROM prices_1m
                 WHERE base_asset = $2 AND quote_asset = $1
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
            ) u
        ),
        r AS (
            SELECT base_asset, vwap::text AS vwap, COALESCE(volume_priced, 0)::text AS volume_priced,
                   COALESCE(trade_count, 0) AS tc, sources
              FROM prices_1m
             WHERE bucket = (SELECT b FROM latest)
               %[1]s
               AND base_asset = $1 AND quote_asset = $2
            UNION ALL
            SELECT base_asset, vwap::text AS vwap, COALESCE(volume_priced, 0)::text AS volume_priced,
                   COALESCE(trade_count, 0) AS tc, sources
              FROM prices_1m
             WHERE bucket = (SELECT b FROM latest)
               %[1]s
               AND base_asset = $2 AND quote_asset = $1
        )
        SELECT (SELECT b FROM latest), base_asset, vwap, volume_priced, tc, sources
          FROM r
    `

func (s *Store) latestClosedVWAP1m(ctx context.Context, p canonical.Pair, since time.Time) (Vwap1mRow, error) {
	// The literal lower bound goes on both max() arms AND the point-read so all three prices_1m
	// scans get plan-time chunk exclusion.
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", since.Format("2006-01-02 15:04:05-07"))
	// G201 is suppressed below: only `lower` is interpolated, from our own time.Time in a fixed
	// layout. The literal is REQUIRED: TimescaleDB only does plan-time chunk exclusion for a
	// constant, not a bind parameter.
	q := fmt.Sprintf(latestClosedVWAP1mTemplate, lower) //nolint:gosec // G201: see note above
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String())
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: LatestClosedVWAP1mForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// One bucket, so at most two rows. An empty result (no closed bucket, or none with a usable
	// price/volume) stays [sql.ErrNoRows], which the price handler resolves via its fallback chain.
	out, err := scanCombinedVwap1mRows(rows, p, 1, "LatestClosedVWAP1mForPair")
	if err != nil {
		return Vwap1mRow{}, err
	}
	if len(out) == 0 {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	return out[0], nil
}

// TimedVWAPsForPair1m returns (vwap, bucket_end) pairs from prices_1m for the half-open window
// [from, to), oldest first, for the multi-window baseline refresher, which needs the timestamp to
// apply [baseline.SplitByLookback] for its 1d / 7d / 30d sub-windows. The bucket end is the stored
// start + 1 minute, as the API surface uses. Returns an empty slice + nil error when the window
// has no closed buckets.
func (s *Store) TimedVWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]domain.BaselineTimedVWAP, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions into the requested ($1, $2) orientation, as
	// LatestClosedVWAP1mForPair does: reading one feeds the anomaly baseline half the liquidity
	// and no rows for minutes that traded only the flipped way. See canonical.Orient.
	//
	// The combine is the VOLUME-weighted union, Σ(requested quote leg) / Σ(requested base leg),
	// with legs re-expressed via Σquote = vwap·volume_priced (the [combineDirVWAP] identity).
	// Trade-count weighting is not a VWAP and biases every two-sided baseline. The division stays
	// in NUMERIC and only the final value casts to float8, since this series feeds statistical
	// baseline math with the float contract documented on [Store.VWAPsForPair1m].
	//
	// Each minute carries its USD notional (volume_usd, NULL when unpriced) so the refresher can
	// build USD-volume bars; that policy lives there, not in this read.
	const q = `
        SELECT (SUM(CASE WHEN base_asset = $1 THEN vwap * COALESCE(volume_priced, 0)
                         ELSE COALESCE(volume_priced, 0) END)
                  / NULLIF(SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume_priced, 0)
                                    ELSE vwap * COALESCE(volume_priced, 0) END), 0))::float8 AS vwap,
               bucket + INTERVAL '1 minute',
               NULLIF(SUM(COALESCE(volume_usd, 0)), 0)::text
          FROM (
            (SELECT bucket, base_asset, vwap, volume_priced, volume_usd
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket >= $3::timestamptz
                AND bucket <  $4::timestamptz)
            UNION ALL
            (SELECT bucket, base_asset, vwap, volume_priced, volume_usd
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket >= $3::timestamptz
                AND bucket <  $4::timestamptz)
          ) AS both_directions
         GROUP BY bucket
        HAVING SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume_priced, 0)
                        ELSE vwap * COALESCE(volume_priced, 0) END) > 0
         ORDER BY bucket ASC
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]domain.BaselineTimedVWAP, 0, 256)
	for rows.Next() {
		var (
			t   domain.BaselineTimedVWAP
			usd sql.NullString
		)
		if err := rows.Scan(&t.VWAP, &t.BucketEnd, &usd); err != nil {
			return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m scan: %w", err)
		}
		if usd.Valid {
			v, ok := new(big.Rat).SetString(usd.String)
			if !ok {
				return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: volume_usd %q is not a decimal", usd.String)
			}
			t.USDVolume = v
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m rows: %w", err)
	}
	return out, nil
}

// VWAPsForPair1m returns oldest-first VWAP values from prices_1m for [from, to), without
// timestamps. The baseline refresher reads [Store.TimedVWAPsForPair1m] instead, since empty
// minutes have no row and `baseline.ReturnsFromVWAPs` needs each bucket's end to scale a return
// across a gap. Returns an empty slice + nil error when the window has no closed buckets.
//
// VWAP goes through a float8 cast: the baseline math is float64 anyway, and the precision loss on
// one bucket's VWAP is immaterial to statistics over hundreds of buckets.
func (s *Store) VWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]float64, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions into the requested ($1, $2) orientation as a VOLUME-weighted
	// union, so the baseline window reflects the full market. See TimedVWAPsForPair1m.
	const q = `
        SELECT (SUM(CASE WHEN base_asset = $1 THEN vwap * COALESCE(volume_priced, 0)
                         ELSE COALESCE(volume_priced, 0) END)
                  / NULLIF(SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume_priced, 0)
                                    ELSE vwap * COALESCE(volume_priced, 0) END), 0))::float8 AS vwap
          FROM (
            (SELECT bucket, base_asset, vwap, volume_priced
               FROM prices_1m
              WHERE base_asset = $1 AND quote_asset = $2
                AND bucket >= $3
                AND bucket <  $4)
            UNION ALL
            (SELECT bucket, base_asset, vwap, volume_priced
               FROM prices_1m
              WHERE base_asset = $2 AND quote_asset = $1
                AND bucket >= $3
                AND bucket <  $4)
          ) AS both_directions
         GROUP BY bucket
        HAVING SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume_priced, 0)
                        ELSE vwap * COALESCE(volume_priced, 0) END) > 0
         ORDER BY bucket ASC
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]float64, 0, 256)
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("timescale: VWAPsForPair1m scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m rows: %w", err)
	}
	return out, nil
}

// stringArray is a [sql.Scanner] for Postgres TEXT[] columns such as prices_1m `sources`. It
// parses only the plain `{a,b,c}` form; quoted entries with embedded commas are unsupported,
// which is fine because source names are identifier-shaped.
type stringArray []string

// normalizeVwapSources sorts the CAGG `sources` array so contributor ordering is deterministic.
// The CAGG's `array_agg(DISTINCT source)` has no ORDER BY, and sorting here avoids a CAGG rebuild.
func normalizeVwapSources(row *Vwap1mRow) {
	if len(row.Sources) < 2 {
		return
	}
	sort.Strings(row.Sources)
}

// Scan implements [sql.Scanner].
func (a *stringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("stringArray: unsupported scan type %T", src)
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("stringArray: malformed Postgres array literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*a = []string{}
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(inner); i++ {
		if i == len(inner) || inner[i] == ',' {
			elt := inner[start:i]
			// NULL elements come through as the literal "NULL"
			// (case-sensitive); array_agg(DISTINCT source) over a
			// non-null source column never produces these, but
			// guard anyway.
			if elt != "NULL" {
				out = append(out, elt)
			}
			start = i + 1
		}
	}
	*a = out
	return nil
}

// Volume24hUSDForAsset returns the trailing 24h USD trade volume for an asset across every pair
// it is in (as base OR quote), read from the prices_1m CAGG rather than the trades hypertable.
// The string carries the NUMERIC sum exactly; "0" means existed with no trades, distinct from null
// ("not tracked"). assetKey is the canonical asset string as stored in base_asset / quote_asset.
//
// The CAGG sums `coalesce(usd_volume, 0)`, where `usd_volume` is the trade-time valuation
// `tradeUSDVolume` (trades.go) stamps at insert. A trade it could not value adds 0, so lowerBound
// reports whether any alias form's asset_volume_24h row counted such a trade.
func (s *Store) Volume24hUSDForAsset(ctx context.Context, assetKey string) (usd string, lowerBound bool, err error) {
	// Sum across ALL canonical forms (XLM's native / crypto:XLM / SAC, plus any classic-SAC
	// wrapper), not the caller's one spelling; a single form silently omits the CEX and Soroban legs.
	const q = `
        SELECT COALESCE(sum(volume_usd), 0)::text, ` + aliasVolumeLowerBoundSQL + `
          FROM prices_1m
         WHERE (base_asset = ANY($1) OR quote_asset = ANY($1))
           AND bucket >= now() - INTERVAL '24 hours'
           AND bucket <= now() - INTERVAL '1 minute'
    `
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetKey)).Scan(&usd, &lowerBound); err != nil {
		return "", false, fmt.Errorf("timescale: Volume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return usd, lowerBound, nil
}

// OHLCBar is one OHLC + volume + trade-count bucket from [Store.OHLCSeries] /
// [Store.OHLCSeriesReBucketed]. Bucket is the window START. Prices are NUMERIC decimal strings
// (no float round-trip) and volumes are integer-string smallest-unit sums.
type OHLCBar struct {
	Bucket      time.Time
	Open        string
	High        string
	Low         string
	Close       string
	BaseVolume  string
	QuoteVolume string
	TradeCount  int64

	// Sources is the lexically-sorted set of venues that contributed trades to this bucket, unioned
	// across BOTH stored directions (the CAGG's `array_agg(DISTINCT source)`).
	//
	// BaseVolume and QuoteVolume sum SMALLEST-UNIT amounts, and the scale is per-SOURCE (on-chain
	// DEX 7 decimals, CEX 8, FX 6). A caller summing bars across markets (internal/api/v1's fiat
	// combine) must lift them to a common scale first or it adds incommensurable integers. Empty
	// when the bucket could not be attributed; never default a scale, since guessing one is the
	// defect this column removes.
	Sources []string
}

// OHLCSeries returns oldest-first OHLC bars from the CAGG matching `granularity` for [from, to),
// for /v1/ohlc's multi-bar mode. Native bucket size IS the interval, so rows map 1:1 to bars;
// other intervals route through [Store.OHLCSeriesReBucketed].
//
// The in-progress bucket is excluded by the sargable `bucket <= now() - <interval>` guard
// (ADR-0015). `limit` clamps row count (0 = unbounded). Returns empty + nil when no closed buckets
// exist.
//
// A capped read orders DESC and takes the LIMIT so the cap keeps the NEWEST buckets, then reverses
// (mirrors [Store.TradesInRange]); `ORDER BY bucket ASC LIMIT` would keep the OLDEST and never
// reach `to` for a wide window.
//
// Σ(quote) (and a flipped row's base leg) is the stored `volume_quote`, exact at any size and
// counting zero-leg trades like `volume`; `vwap * volume` would price a zero-quote trade's base at
// the bucket price and drop a zero-base trade's quote.
func (s *Store) OHLCSeries(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	limit int,
) ([]OHLCBar, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: OHLCSeries: to %v <= from %v", to, from)
	}
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// Combine BOTH stored directions into the requested ($1, $2) orientation as a UNION ALL of two
	// index-drivable branches, not an OR (see [Store.HistoryPoints]); a window bound does not
	// rescue the OR since /v1/ohlc's window is caller-chosen and can span all history.
	//
	// The `norm` CTE re-expresses each row in the requested orientation: flipped rows invert every
	// price, which SWAPS high<->low, and swap base<->quote volume. Per bucket: high = max, low =
	// min; open/close prefer the requested-direction row and fall back to the inverted flipped row
	// (intra-bucket ordering across directions is unknowable from the CAGG); volumes + trade_count
	// sum. See canonical.Orient / canonOrientSQL.
	//
	// `sources` is carried through so a caller combining bars across markets can resolve each
	// bar's scale (see [OHLCBar.Sources]). Each branch admits one (base_asset, quote_asset), so
	// max() over a bucket's single row per direction is that row; inline keeps one grouping pass
	// (a joined-back CTE costs 1.34x, this 1.005x EXPLAIN plan cost).
	// #nosec G201 — table + interval come from the validated HistoryGranularity enum.
	q := fmt.Sprintf(`
		WITH norm AS (
		    SELECT
		        bucket,
		        (base_asset = $1) AS req,
		        CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		        CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		        CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		        CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		        CASE WHEN base_asset = $1 THEN volume       ELSE volume_quote END AS base_vol,
		        CASE WHEN base_asset = $1 THEN volume_quote ELSE volume       END AS quote_vol,
		        trade_count AS tc,
		        sources     AS srcs
		      FROM (
		        (SELECT bucket, base_asset, first_price, last_price, high_price, low_price,
		                volume, volume_quote, trade_count, sources
		           FROM %[1]s
		          WHERE base_asset = $1 AND quote_asset = $2
		            AND bucket >= $3
		            AND bucket <  $4
		            AND bucket <= now() - INTERVAL '%[2]s')
		        UNION ALL
		        (SELECT bucket, base_asset, first_price, last_price, high_price, low_price,
		                volume, volume_quote, trade_count, sources
		           FROM %[1]s
		          WHERE base_asset = $2 AND quote_asset = $1
		            AND bucket >= $3
		            AND bucket <  $4
		            AND bucket <= now() - INTERVAL '%[2]s')
		      ) AS both_directions
		)
		SELECT
		    bucket,
		    COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1])::text AS open,
		    max(hi)::text                                                           AS high,
		    min(lo)::text                                                           AS low,
		    COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1])::text AS close,
		    sum(base_vol)::text                                                     AS base_volume,
		    sum(quote_vol)::text                                                    AS quote_volume,
		    sum(tc)::bigint                                                         AS trade_count,
		    (SELECT COALESCE(array_agg(DISTINCT s ORDER BY s), ARRAY[]::text[])
		       FROM unnest(COALESCE(max(srcs) FILTER (WHERE req),     ARRAY[]::text[])
		                || COALESCE(max(srcs) FILTER (WHERE NOT req), ARRAY[]::text[])) AS s)
		                                                                            AS sources
		  FROM norm
		 GROUP BY bucket
	`, table, interval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		// DESC + LIMIT keeps the NEWEST `limit` buckets when the window
		// holds more than that many; reversed to ascending below. See the
		// doc comment above.
		args = append(args, limit)
		q += fmt.Sprintf(" ORDER BY bucket DESC LIMIT $%d", len(args))
	} else {
		q += " ORDER BY bucket ASC"
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		var srcs stringArray
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
			&srcs,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeries[%s] scan: %w", granularity, err)
		}
		bar.Sources = srcs
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s] rows: %w", granularity, err)
	}
	if limit > 0 {
		// Scanned newest-first (DESC above); reverse to the ascending
		// order this method's contract promises its callers.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// ohlcReBucketedQuery builds [Store.OHLCSeriesReBucketed]'s statement.
// Extracted from its caller so the reader stays inside the package length
// budget; `table` comes from the validated granularity enum and
// `outInterval` from the caller's allow-list, so neither is user input.
func ohlcReBucketedQuery(table, outInterval string) string {
	// #nosec G201 — see the call site: both arguments are enum/allow-list
	// derived, never user-passed.
	return fmt.Sprintf(`
		WITH norm AS (
		    SELECT bucket,
		           COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1]) AS open,
		           max(hi) AS high,
		           min(lo) AS low,
		           COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1]) AS close,
		           sum(base_vol)  AS base_vol,
		           sum(quote_vol) AS quote_vol,
		           sum(tc)        AS tc,
		           COALESCE(max(srcs) FILTER (WHERE req),     ARRAY[]::text[])
		        || COALESCE(max(srcs) FILTER (WHERE NOT req), ARRAY[]::text[]) AS srcs
		      FROM (
		        SELECT bucket, (base_asset = $1) AS req,
		               CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		               CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		               CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		               CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		               CASE WHEN base_asset = $1 THEN volume       ELSE volume_quote END AS base_vol,
		               CASE WHEN base_asset = $1 THEN volume_quote ELSE volume       END AS quote_vol,
		               trade_count AS tc,
		               sources     AS srcs
		          FROM (
		            (SELECT bucket, base_asset, first_price, last_price, high_price, low_price,
		                    volume, volume_quote, trade_count, sources
		               FROM %[1]s
		              WHERE base_asset = $1 AND quote_asset = $2
		                AND bucket >= $3
		                AND bucket <  $4)
		            UNION ALL
		            (SELECT bucket, base_asset, first_price, last_price, high_price, low_price,
		                    volume, volume_quote, trade_count, sources
		               FROM %[1]s
		              WHERE base_asset = $2 AND quote_asset = $1
		                AND bucket >= $3
		                AND bucket <  $4)
		          ) AS both_directions
		      ) raw
		     GROUP BY bucket
		),
		src AS (
		    SELECT time_bucket(INTERVAL '%[2]s', bucket) AS ob,
		           array_agg(DISTINCT s ORDER BY s)      AS sources
		      FROM norm, unnest(norm.srcs) AS s
		     GROUP BY 1
		)
		SELECT
		    time_bucket(INTERVAL '%[2]s', n.bucket)                     AS out_bucket,
		    (array_agg(n.open  ORDER BY n.bucket ASC))[1]::text         AS open,
		    max(n.high)::text                                           AS high,
		    min(n.low)::text                                            AS low,
		    (array_agg(n.close ORDER BY n.bucket DESC))[1]::text        AS close,
		    sum(n.base_vol)::text                                       AS base_volume,
		    sum(n.quote_vol)::text                                      AS quote_volume,
		    sum(n.tc)::bigint                                           AS trade_count,
		    COALESCE(sc.sources, ARRAY[]::text[])                       AS sources
		  FROM norm n
		  LEFT JOIN src sc ON sc.ob = time_bucket(INTERVAL '%[2]s', n.bucket)
		 GROUP BY time_bucket(INTERVAL '%[2]s', n.bucket), sc.sources
		 HAVING time_bucket(INTERVAL '%[2]s', n.bucket) <= now() - INTERVAL '%[2]s'
	`, table, outInterval)
}

// OHLCSeriesReBucketed is [Store.OHLCSeries] re-bucketed into a coarser `outInterval` via
// `time_bucket`, for intervals with no native CAGG (the folded rows of [OHLCRoutes]). Open is the
// first input bar's open and close the last bar's close (ordered by bucket); high/low are max/min;
// volumes and trade_count sum (quote volume per [Store.OHLCSeries]).
//
// `outInterval` MUST be an integer multiple of the source CAGG's native bucket size
// (TestOHLCRoutesFoldIsAMultipleOfItsSource pins every declared pairing). time_bucket snaps to the
// first Monday of 2000 UTC, so 2w lands on the Mondays prices_1w starts on. `outInterval` is
// composed into the SQL only after the [OHLCRoutes] allow-list check. The closed-bucket guard and
// `limit` DESC-then-reverse ordering are [Store.OHLCSeries]'s.
func (s *Store) OHLCSeriesReBucketed(
	ctx context.Context,
	p canonical.Pair,
	sourceGranularity HistoryGranularity,
	outInterval string,
	from, to time.Time,
	limit int,
) ([]OHLCBar, error) {
	if err := sourceGranularity.Validate(); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed: to %v <= from %v", to, from)
	}
	// Allow-list: outInterval composes directly into the SQL, so it must not be untrusted input.
	// The accepted pairings are the folded rows of [OHLCRoutes], the table the API routes from; a
	// hand-kept copy would drift and 500 on any width the router learns but the copy does not.
	if !ohlcFoldDeclared(sourceGranularity, outInterval) {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed: fold %s→%q is not declared in OHLCRoutes", sourceGranularity, outInterval)
	}
	table := "prices_" + string(sourceGranularity)
	// Combine BOTH stored directions before re-bucketing: the `norm` CTE collapses each SOURCE
	// bucket's two directions into one bar in the requested ($1, $2) orientation (see OHLCSeries /
	// canonical.Orient), and the outer query folds those into the coarser out_bucket.
	//
	// `sources` needs two folds, and only the first can use OHLCSeries's trick. A SOURCE bucket has
	// at most one row per direction (the CAGG groups on (bucket, base_asset, quote_asset)), so norm
	// concatenates them. Across source buckets the fold is a real N-way union, so it gets its own
	// grouping over the unnested arrays, joined back on out_bucket (1.25x plan cost); unioning at
	// the pre-fold grain forces the row-level CTE to materialise (2.08x).
	// #nosec G201 — table comes from the validated enum, outInterval from the allow-list above.
	q := ohlcReBucketedQuery(table, outInterval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		// DESC + LIMIT keeps the NEWEST `limit` out-buckets when the window
		// holds more than that many; reversed to ascending below. Same
		// shape as [Store.OHLCSeries] — an `ORDER BY out_bucket ASC LIMIT`
		// would keep the OLDEST `limit` buckets.
		args = append(args, limit)
		q += fmt.Sprintf(" ORDER BY out_bucket DESC LIMIT $%d", len(args))
	} else {
		q += " ORDER BY out_bucket ASC"
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s]: %w", sourceGranularity, outInterval, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		var srcs stringArray
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
			&srcs,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] scan: %w", sourceGranularity, outInterval, err)
		}
		bar.Sources = srcs
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] rows: %w", sourceGranularity, outInterval, err)
	}
	if limit > 0 {
		// Scanned newest-first (DESC above); reverse to the ascending
		// order this method's contract promises its callers.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// MarketSubstance summarises a pair's trailing closed-bucket activity across BOTH stored
// directions, measured on prices_1m. It feeds the thin-market gate
// (internal/pricingguard.SubstanceGate).
//
// VolumeUSD is the exact NUMERIC sum of volume_usd as a decimal string (ADR-0003: the floor
// comparison is exact-rational). Buckets counts DISTINCT closed 1-minute buckets with a trade and
// SpanSeconds is max(bucket)-min(bucket); together they are the persistence signal, since a burst
// of N trades in one minute gives Buckets=1 / SpanSeconds=0, separating dust from continuous
// trading.
//
// Rows whose usd_volume could not be valued contribute 0, so an unvaluable market fails a positive
// floor BY CONSTRUCTION (fail-closed). ValuedBuckets counts buckets with positive summed
// volume_usd so the gate can tell that failure from a valued thin market.
type MarketSubstance struct {
	VolumeUSD     string
	Buckets       int64
	SpanSeconds   int64
	ValuedBuckets int64
}

// substanceLegs renders the two spelling sets a substance read binds as
// $1/$2. An empty set would measure nothing and read as "no market".
func substanceLegs(method string, bases, quotes []canonical.Asset) (baseKeys, quoteKeys []string, err error) {
	if len(bases) == 0 || len(quotes) == 0 {
		return nil, nil, fmt.Errorf("timescale: %s: empty spelling set (bases %d, quotes %d)", method, len(bases), len(quotes))
	}
	keys := func(set []canonical.Asset) []string {
		out := make([]string, len(set))
		for i, a := range set {
			out[i] = a.String()
		}
		return out
	}
	return keys(bases), keys(quotes), nil
}

// PairMarketSubstance measures [MarketSubstance] over the trailing `window` (closed buckets only,
// ADR-0015) for the ONE market formed by every spelling in `bases` against every spelling in
// `quotes`, the alias union the substance gate holds to its floor. A minute active under two
// spellings is one bucket, so the union is taken over bucket timestamps, never by adding
// per-spelling counts. Like the other prices_1m readers, the lower bound is a LITERAL timestamptz
// for plan-time chunk pruning and the closed-bucket predicate is the sargable
// `bucket <= now() - 1min`.
//
// An empty market returns {VolumeUSD: "0"} with a nil error: absence of market is a measurement.
func (s *Store) PairMarketSubstance(ctx context.Context, bases, quotes []canonical.Asset, window time.Duration) (MarketSubstance, error) {
	if window <= 0 {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstance: non-positive window %v", window)
	}
	baseKeys, quoteKeys, err := substanceLegs("PairMarketSubstance", bases, quotes)
	if err != nil {
		return MarketSubstance{}, err
	}
	cutoff := time.Now().UTC().Add(-window)
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", cutoff.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — only `lower` (our own time.Time, fixed layout) is interpolated; the spelling
	// sets bind as $1/$2. The second arm's NOT stops a row both arms select (possible only when
	// the two sets share a spelling) from counting twice.
	q := fmt.Sprintf(`
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
    `, lower) //nolint:gosec // G201: see note above
	var sub MarketSubstance
	if err := s.db.QueryRowContext(ctx, q, baseKeys, quoteKeys).Scan(
		&sub.VolumeUSD, &sub.Buckets, &sub.SpanSeconds, &sub.ValuedBuckets,
	); err != nil {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstance: %w", err)
	}
	return sub, nil
}

// PairMarketSubstanceAt measures [MarketSubstance] over the `window` ENDING AT `asOf`, the
// point-in-time twin of [Store.PairMarketSubstance]: a pair thick today may have been
// attacker-seeded dust at `ts`.
//
// `g` is the grain the legs are counted at; only two are accepted:
//   - [Granularity1m]: the live gate's grain. Its reach back is a deployment setting (see
//     [Store.DailyMarketDays], "Why prices_1h"), so use it only for instants recent enough that
//     retention still holds the window.
//   - [Granularity1h]: indefinite by design, the grain a historical instant is held to.
//
// The window is the buckets already CLOSED at asOf (ADR-0015): `bucket <= asOf - g` (sargable)
// and `bucket >= asOf - window`, both LITERAL timestamptz values for plan-time chunk pruning. The
// `now()` guard stays so an asOf at or past the present never admits the in-progress bucket. An
// empty window returns {VolumeUSD: "0"} with a nil error.
func (s *Store) PairMarketSubstanceAt(
	ctx context.Context, bases, quotes []canonical.Asset, asOf time.Time, window time.Duration, g HistoryGranularity,
) (MarketSubstance, error) {
	if window <= 0 {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstanceAt: non-positive window %v", window)
	}
	if g != Granularity1m && g != Granularity1h {
		return MarketSubstance{}, fmt.Errorf(
			"timescale: PairMarketSubstanceAt: unsupported grain %q (want %s or %s)", g, Granularity1m, Granularity1h)
	}
	baseKeys, quoteKeys, err := substanceLegs("PairMarketSubstanceAt", bases, quotes)
	if err != nil {
		return MarketSubstance{}, err
	}
	const layout = "2006-01-02 15:04:05-07"
	upper := asOf.UTC().Add(-g.BucketDuration())
	lower := asOf.UTC().Add(-window)
	// #nosec G201 — the table suffix and closed-bucket interval come from the two-member grain
	// allowlist above, and the bounds are our own time.Time in a fixed layout; the spelling sets
	// bind as $1/$2. The union and the second arm's NOT are [Store.PairMarketSubstance]'s.
	q := fmt.Sprintf(`
        SELECT COALESCE(sum(bucket_usd), 0)::text,
               count(*),
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0),
               count(*) FILTER (WHERE bucket_usd > 0)
          FROM (
            SELECT bucket, sum(volume_usd) AS bucket_usd
              FROM (
                SELECT bucket, volume_usd FROM prices_%[1]s
                 WHERE base_asset = ANY($1) AND quote_asset = ANY($2)
                   AND bucket <= now() - INTERVAL '%[2]s'
                   AND bucket <= TIMESTAMPTZ '%[3]s'
                   AND bucket >= TIMESTAMPTZ '%[4]s'
                UNION ALL
                SELECT bucket, volume_usd FROM prices_%[1]s
                 WHERE base_asset = ANY($2) AND quote_asset = ANY($1)
                   AND bucket <= now() - INTERVAL '%[2]s'
                   AND bucket <= TIMESTAMPTZ '%[3]s'
                   AND bucket >= TIMESTAMPTZ '%[4]s'
                   AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))
              ) d
             GROUP BY bucket
          ) b
    `, string(g), g.closedBucketInterval(), upper.Format(layout), lower.Format(layout)) //nolint:gosec // G201: see note above
	var sub MarketSubstance
	if err := s.db.QueryRowContext(ctx, q, baseKeys, quoteKeys).Scan(
		&sub.VolumeUSD, &sub.Buckets, &sub.SpanSeconds, &sub.ValuedBuckets,
	); err != nil {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstanceAt: %w", err)
	}
	return sub, nil
}
