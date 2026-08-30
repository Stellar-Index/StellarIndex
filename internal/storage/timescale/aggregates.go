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
	VWAP      string  // NUMERIC text from Postgres
	VolumeUSD *string // NULL when no usd_volume column entries — e.g. early classic-only ledgers
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

// Validate reports whether g is one of the seven supported
// granularities. Caller surfaces unknown granularities as 400.
func (g HistoryGranularity) Validate() error {
	switch g {
	case Granularity1m, Granularity15m, Granularity1h, Granularity4h,
		Granularity1d, Granularity1w, Granularity1mo:
		return nil
	}
	return fmt.Errorf("unknown granularity %q (want one of: 1m, 15m, 1h, 4h, 1d, 1w, 1mo)", g)
}

// closedBucketInterval is the Postgres INTERVAL string that the
// closed-bucket guard subtracts from now() per ADR-0015. Equal to
// the granularity's bucket size.
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

// BucketDuration is the wall-clock width of one bucket at this
// granularity — the value the price surfaces stamp into
// `window_seconds`, and the amount [Store.ClosedVWAPAtOrBefore] adds
// to a bucket START to get its close instant. 1mo is approximated as
// 30 days (the CAGG's `time_bucket('1 month', …)` is calendar-exact,
// but this figure is only used for staleness math + the resolution
// ladder, where a 30-day nominal month is close enough). Zero for an
// unknown granularity.
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

// dirVWAP is ONE stored market direction's contribution to a single
// CAGG bucket, as scanned from prices_<granularity>. The decoder keeps
// each trade in the venue's observed base/quote ordering rather than
// re-orienting it (see [canonical.Trade] / [canonical.Orient]), so the
// same market lands in the CAGG as BOTH (A,B) and (B,A) rows — every
// serving read has to fold the two together itself.
//
//   - vwapText and volumeText are the row's NUMERIC columns as text
//     (ADR-0003 — no float round-trip). Per migration 0002,
//     vwap = Σquote_amount / Σbase_amount and volume = Σbase_amount,
//     both in THAT ROW's own orientation and raw (smallest-unit) terms.
//   - flipped marks a row stored (quote, base) relative to the
//     orientation the caller asked for.
type dirVWAP struct {
	vwapText   string
	volumeText string
	flipped    bool
}

// combinedVWAPSigDigits is how many SIGNIFICANT digits a combined VWAP
// renders with. Prices on this surface span roughly 1e2 (fiat crosses)
// down to 1e-12 and below (an 18-decimals Soroban token quoted in XLM),
// so the precision has to be RELATIVE — see [rateScaleFor], which this
// reuses. 25 digits is comfortably past the ~16–20 fractional digits
// Postgres gives the CAGG's own NUMERIC division, so rendering never
// throws away precision the inputs actually carried.
const combinedVWAPSigDigits = 25

// formatCombinedVWAP renders an exact combined VWAP as a decimal string
// with trailing zeros trimmed — the money-string house style (mirrors
// `formatCrossRate` on the API side). This rendering is the ONLY lossy
// step in the combine; everything upstream of it is exact rational
// arithmetic.
func formatCombinedVWAP(r *big.Rat) string {
	s := r.FloatString(rateScaleFor(r, combinedVWAPSigDigits))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// combineDirVWAP folds the stored market directions of ONE bucket into
// the VWAP of their union, expressed in the requested (base, quote)
// orientation, as a NUMERIC-shaped decimal string.
//
// VWAP is Σ(quote leg) / Σ(base leg), so the only correct way to merge
// two directions is to re-express each row's two legs in the REQUESTED
// orientation's units and re-divide the sums:
//
//	requested base  leg = volume          (flipped row: vwap × volume)
//	requested quote leg = vwap × volume   (flipped row: volume)
//
// A flipped row's `volume` is Σ of the requested QUOTE asset, and its
// vwap × volume reconstructs Σ of the requested BASE asset — the same
// Σquote = vwap·Σbase identity [Store.OHLCSeries] already uses to derive
// quote_amount. Both directions therefore contribute in the same raw
// units and the quotient is the true union VWAP.
//
// This replaces a TRADE-COUNT-weighted mean of {vwap, 1/vwap_flipped}.
// That is not a VWAP at all: it weights a hundred dust trades above one
// whale, and it equals the union VWAP only in the degenerate case where
// both directions happen to carry the identical average trade size. On
// a real two-sided market it is wrong by an unbounded factor.
//
// A single unflipped row is returned VERBATIM: the union of one
// direction IS that row's stored VWAP, so passing the Postgres NUMERIC
// text straight through keeps the served bytes byte-identical to a
// single-direction read.
//
// Arithmetic is exact (math/big) — no float64 anywhere near a served
// money value (ADR-0003), and no `1.0 / vwap` inversion, which would
// round the flipped leg to whatever scale Postgres picked for that
// division BEFORE it was ever weighted.
//
// ok=false when no row carries a usable (parseable, positive) price and
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

// dirTWAP is ONE stored market direction's contribution to a single TWAP
// CAGG bucket (twap_1h / twap_1d), as scanned from twap_<granularity>.
// Mirrors [dirVWAP], but the merge weight is the direction's minute
// COVERAGE — the number of prices_1m minute-buckets it contributed
// (migration 0126's `sample_count`) — not trade count (see [combineDirTWAP]).
//
//   - twapText is the row's `twap` NUMERIC column as text (ADR-0003 — no
//     float round-trip), in THAT ROW's own orientation. Per migration 0081
//     it is avg(prices_1m.twap) over the window's minute buckets.
//   - sampleCount is the number of those minute buckets (migration 0126) —
//     the exact denominator of the row's own avg, and the weight the
//     union merge needs.
//   - flipped marks a row stored (quote, base) relative to the orientation
//     the caller asked for.
type dirTWAP struct {
	twapText    string
	sampleCount int64
	flipped     bool
}

// combineDirTWAP folds the stored market directions of ONE TWAP bucket
// into the requested (base, quote) orientation, as a NUMERIC-shaped
// decimal string.
//
// The twap_1h / twap_1d CAGGs (migration 0081) define twap =
// avg(prices_1m.twap) over the minute buckets in the window — one equal
// observation per elapsed MINUTE, deliberately NOT per trade (0081's
// "time-weighted at 1-minute resolution"; a minute with 1000 dust prints
// must not outvote 999 quiet minutes). Merging two stored directions
// therefore has ONE correct weight: each direction's minute COVERAGE, i.e.
// how many prices_1m minute-buckets it contributed. That count is
// migration 0126's `sample_count`; it is why 0126 exists — it is not
// recoverable from twap/trade_count/volume.
//
//	combined = Σ(oriented_twap · sample_count) / Σ(sample_count)
//
// A flipped row (stored quote/base relative to the request) is oriented by
// an EXACT rational reciprocal 1/twap — never a SQL `1.0 / twap`, which
// would round the inverted leg to whatever scale Postgres picked for that
// division BEFORE it was ever weighted (ADR-0003).
//
// This replaces a TRADE-COUNT-weighted mean of {twap, 1/twap_flipped}.
// Trade count is the weight 0081 exists to reject: count-weighting the
// DIRECTION merge is exact only in the degenerate case where each
// direction's trade count is proportional to its minute coverage, and
// wrong by an unbounded factor otherwise. Equal-weighting the two
// directions is ALSO wrong — it regresses the healthy, well-covered case —
// so the coverage count is load-bearing.
//
// A single unflipped row is returned VERBATIM (its stored NUMERIC text),
// keeping the served bytes byte-identical to a single-direction read.
//
// ok=false when no row carries a usable (parseable, positive) twap with a
// positive sample_count; callers treat that as "no data for this bucket".
func combineDirTWAP(rows []dirTWAP) (string, bool) {
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

// appendSources appends src's entries to dst, skipping any already
// present. The per-direction rows of one bucket carry their own
// `sources` arrays; a served row's contributor list is their union.
// Linear scan — a bucket has a handful of sources, and this avoids a
// map allocation per bucket on the price hot path. Ordering is settled
// afterwards by [normalizeVwapSources].
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

// bucketRowCap converts a caller's BUCKET limit into the SQL row limit
// a both-directions read needs. A bucket holds at most two rows (one
// per stored direction), so 2n+1 rows always contain the first n
// buckets COMPLETE — the +1 row can only ever start an (n+1)-th bucket,
// which the caller trims away. Keeping the cap in SQL (rather than
// selecting DISTINCT buckets in a CTE first) leaves the existing
// index-ordered plans untouched.
//
// Returns 0 for a non-positive limit, meaning "no LIMIT clause".
func bucketRowCap(limit int) int {
	if limit <= 0 {
		return 0
	}
	return 2*limit + 1
}

// HistoryPoints returns every CLOSED bucket for the pair from the
// CAGG matching `granularity`, ordered chronologically (ASC). Used
// by /v1/history/since-inception to serve the full historical
// series at the requested granularity.
//
// Per ADR-0015 the in-progress bucket is excluded via a
// `bucket + <granularity> <= now()` guard.
//
// `limit` clamps the BUCKET count; passing 0 returns all buckets. The
// API caller passes the spec-bounded value (default unbounded for
// since-inception; clients paginate via Pagination.next when we
// add that surface).
//
// BOTH stored directions of the market are read and folded into the
// requested ($1, $2) orientation by [combineDirVWAP] — the same
// treatment [Store.LatestClosedVWAP1mForPair] and [Store.OHLCSeries]
// give. Reading only (base=$1, quote=$2), as this did, silently DROPPED
// every trade the venue happened to record the other way round, so the
// served series was a VWAP over an arbitrary subset of the market (and
// empty for a flipped-only pair).
//
// Returns an empty slice + nil error when the pair has no closed
// buckets at this granularity yet.
func (s *Store) HistoryPoints(ctx context.Context, p canonical.Pair, granularity HistoryGranularity, limit int) ([]HistoryPoint, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	// SQL injection guard: granularity goes via Validate() against
	// a fixed enum, then composes into the table name. Body of the
	// query plus params binds via $1/$2. The fmt.Sprintf format is
	// safe — both `table` and `interval` come from a closed enum
	// after Validate(), not from user input.
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate above.
	q := fmt.Sprintf(`
		SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text, volume_usd::text
		  FROM %s
		 WHERE ((base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1))
		   AND bucket + INTERVAL '%s' <= now()
		 ORDER BY bucket ASC
	`, table, interval)
	args := []any{p.Base.String(), p.Quote.String()}
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		q += " LIMIT $3"
		args = append(args, rowCap)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPoints[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanHistoryPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("HistoryPoints[%s]", granularity))
}

// scanHistoryPoints folds the raw both-directions rows of a
// prices_<granularity> read into one [HistoryPoint] per bucket,
// preserving the query's bucket ordering. Rows of the same bucket are
// adjacent (every caller orders by bucket), so this is one pass with no
// map.
//
// `base` is the requested pair's base asset id — a row whose base_asset
// differs is the flipped storage of the same market. `limit` is the
// caller's BUCKET limit (0 = unbounded); the query fetched
// [bucketRowCap] rows, so the first `limit` buckets are complete and
// anything past them is trimmed. `what` labels wrapped errors.
func scanHistoryPoints(rows *sql.Rows, base string, limit int, what string) ([]HistoryPoint, error) {
	out := make([]HistoryPoint, 0, 1024)
	var (
		curBucket time.Time
		curDirs   []dirVWAP
		curUSD    []sql.NullString
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
		out = append(out, HistoryPoint{
			Bucket:    curBucket,
			VWAP:      vwap,
			VolumeUSD: sumUSDVolume(curUSD),
		})
	}
	for rows.Next() {
		var (
			bucket  time.Time
			rowBase string
			vwap    string
			volume  string
			vusd    sql.NullString
		)
		if err := rows.Scan(&bucket, &rowBase, &vwap, &volume, &vusd); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		if !open || !bucket.Equal(curBucket) {
			flush()
			curBucket, curDirs, curUSD, open = bucket, curDirs[:0], curUSD[:0], true
		}
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

// sumUSDVolume adds the per-direction volume_usd columns of one bucket.
// The two stored directions hold DISJOINT trades (the decoder files each
// trade once, in the venue's own orientation), so their USD volumes add
// — no double counting. Both are already USD-denominated, so unlike the
// price legs no orientation conversion applies.
//
// Returns nil when every direction's column is NULL (the "no usd_volume
// entries" case [HistoryPoint] documents), and the single non-NULL value
// VERBATIM when only one direction contributes — keeping the served
// bytes identical to a single-direction read. Summing renders at the
// widest input scale, which is exactly what a Postgres `SUM(numeric)`
// would have produced.
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

// HistoryPointsInRange is [HistoryPoints] with an explicit
// [from, to) time bound on the bucket column. Used by /v1/chart
// to serve a rolling-window view (timeframe → from = now-tf, to =
// now); the same closed-bucket guard from [HistoryPoints] applies.
//
// `from` zero disables the lower bound (equivalent to since-
// inception); `to` zero disables the upper bound. `limit` 0 returns
// all buckets.
//
// Both stored directions are combined into the requested orientation —
// see [Store.HistoryPoints] for why a one-direction read silently drops
// liquidity.
//
// Empty slice + nil error when the pair has no closed buckets in
// the requested window.
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
	// Build args incrementally so the placeholder count matches the
	// optional from/to/limit clauses.
	args := []any{p.Base.String(), p.Quote.String()}
	clauses := "((base_asset = $1 AND quote_asset = $2)\n        OR (base_asset = $2 AND quote_asset = $1))" +
		"\n   AND bucket + INTERVAL '" + interval + "' <= now()"
	if !from.IsZero() {
		args = append(args, from.UTC())
		clauses += fmt.Sprintf("\n   AND bucket >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		clauses += fmt.Sprintf("\n   AND bucket < $%d", len(args))
	}
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate.
	q := fmt.Sprintf(`
		SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text, volume_usd::text
		  FROM %s
		 WHERE %s
		 ORDER BY bucket ASC
	`, table, clauses)
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		args = append(args, rowCap)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPointsInRange[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanHistoryPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("HistoryPointsInRange[%s]", granularity))
}

// twapGranularities is the set of granularities backed by a TWAP
// continuous aggregate (migration 0081). TWAP charts serve only these
// two resolutions — 1h for sub-daily timeframes, 1d for daily+ — so the
// TWAP CAGG surface stays minimal (one hierarchical view each over
// prices_1m) rather than mirroring all seven prices_* grains.
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

// TWAPPointsInRange returns CLOSED time-weighted-average buckets for the
// pair from the twap_<granularity> CAGG (migration 0081), ordered
// chronologically (ASC). It is the TWAP sibling of
// [Store.HistoryPointsInRange]: same [from, to) window semantics, same
// closed-bucket guard, same `[]HistoryPoint` wire shape (the VWAP field
// carries the TWAP value). granularity must be 1h or 1d — the only two
// grains with a TWAP CAGG; anything else returns an error the API
// surfaces as an unknown-granularity 400.
//
// TWAP methodology lives in the CAGG (migration 0081): time-weighted at
// 1-minute resolution. This read only combines the two stored market
// directions into the requested ($1, $2) orientation, exactly as the
// VWAP reads do (LatestClosedVWAP1mForPair, TimedVWAPsForPair1m,
// OHLCSeries): the SDEX decoder records XLM/USDC and USDC/XLM as
// separate rows, so reading only (base=$1, quote=$2) would use half the
// liquidity. The fold is [combineDirTWAP] in Go — exact rational money
// math (ADR-0003): flipped rows are inverted as an EXACT 1/twap (not a
// rounded SQL `1.0/twap`) and every direction is weighted by its minute
// COVERAGE (`sample_count`, migration 0126), NOT trade count. Migration
// 0081's twap is equal-per-minute, so the direction merge must weight by
// minutes covered; the earlier trade-count weight let a burst of dust
// prints on one side outvote the other. See canonical.Orient.
//
// Both stored directions are selected RAW (bucket, base_asset, twap,
// sample_count, volume_usd) and folded per bucket by [scanTWAPPoints] —
// the [scanHistoryPoints] shape, so `limit` is a BUCKET limit
// ([bucketRowCap] fetches 2n+1 rows to deliver n complete buckets).
//
// Closed-bucket (ADR-0015): `bucket <= now() - <interval>` — the
// SARGABLE form (a constant on the right, no function on the indexed
// `bucket` column), so the from/to range bounds + this predicate prune
// chunks at plan time. Empty slice + nil error when the pair has no
// closed TWAP buckets in the window.
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
	// Select BOTH stored directions RAW; the fold into the requested
	// ($1, $2) orientation is [combineDirTWAP] in Go (exact big.Rat, no
	// `1.0/twap` SQL rounding — ADR-0003). A bucket has at most two rows
	// (one per stored direction), so this is a plain index-ordered scan.
	args := []any{p.Base.String(), p.Quote.String()}
	clauses := "((base_asset = $1 AND quote_asset = $2)\n         OR (base_asset = $2 AND quote_asset = $1))" +
		"\n       AND bucket <= now() - INTERVAL '" + interval + "'"
	if !from.IsZero() {
		args = append(args, from.UTC())
		clauses += fmt.Sprintf("\n       AND bucket >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		clauses += fmt.Sprintf("\n       AND bucket < $%d", len(args))
	}
	// #nosec G201 — table + interval derive from the validated
	// twapGranularities set, not user input. See TWAPGranularitySupported.
	q := fmt.Sprintf(`
		SELECT bucket, base_asset, twap::text, COALESCE(sample_count, 0), volume_usd::text
		  FROM %s
		 WHERE %s
		 ORDER BY bucket ASC
	`, table, clauses)
	if rowCap := bucketRowCap(limit); rowCap > 0 {
		args = append(args, rowCap)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: TWAPPointsInRange[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	return scanTWAPPoints(rows, p.Base.String(), limit,
		fmt.Sprintf("TWAPPointsInRange[%s]", granularity))
}

// scanTWAPPoints folds the raw both-directions rows of a
// twap_<granularity> read into one [HistoryPoint] per bucket (the VWAP
// field carries the TWAP value), preserving the query's bucket ordering.
// Rows of the same bucket are adjacent (the query orders by bucket), so
// this is one pass with no map — the [scanHistoryPoints] shape, with
// [combineDirTWAP]'s coverage-weighted fold in place of the volume-
// weighted one and `sample_count` in place of `volume`.
//
// `base` is the requested pair's base asset id — a row whose base_asset
// differs is the flipped storage of the same market. `limit` is the
// caller's BUCKET limit (0 = unbounded); the query fetched [bucketRowCap]
// rows, so the first `limit` buckets are complete and anything past them
// is trimmed. `what` labels wrapped errors.
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
			vusd    sql.NullString
		)
		if err := rows.Scan(&bucket, &rowBase, &twap, &sc, &vusd); err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", what, err)
		}
		if !open || !bucket.Equal(curBucket) {
			flush()
			curBucket, curDirs, curUSD, open = bucket, curDirs[:0], curUSD[:0], true
		}
		curDirs = append(curDirs, dirTWAP{
			twapText:    twap,
			sampleCount: sc,
			flipped:     rowBase != base,
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

// Vwap1mRow is one row from the prices_1m continuous aggregate.
// The fields mirror migrations/0002_create_price_aggregates.up.sql
// — see that file for the SQL semantics. Bucket is the START of the
// 1-minute window; the window's END is `bucket + 1 minute`.
type Vwap1mRow struct {
	Bucket     time.Time
	BaseAsset  string
	QuoteAsset string
	// VWAP, FirstPrice, LastPrice, HighPrice, LowPrice are decimal
	// strings exactly as Postgres serialises NUMERIC. Storing them
	// as strings avoids a float round-trip (ADR-0003) — handlers
	// that need a numeric value parse with big.Rat.
	VWAP       string
	TradeCount int64
	Sources    []string
}

// RecentClosedVWAP1mForPair returns up to `limit` most-recent CLOSED
// 1-minute buckets from the prices_1m CAGG for the given pair,
// newest first. Same closed-bucket guard as
// [LatestClosedVWAP1mForPair] (ADR-0015).
//
// Returns an empty slice + nil error when the pair has no closed
// buckets in scope. The caller (typically the SEP-40 prices
// endpoint) distinguishes "no observations" from "asset unknown"
// by combining this with an asset-existence check.
//
// recentClosedVWAP1mForPairQuery is the single-direction, newest-first
// closed-bucket read behind the SEP-40 prices() passthrough.
//
// `bucket <= now() - INTERVAL '1 minute'`, NOT `bucket + INTERVAL
// '1 minute' <= now()`. The two are semantically identical, but the
// second applies a function to the indexed column, so the planner cannot
// use the bucket index or prune chunks at plan time and the read
// degrades on a large prices_1m (wave-D UNAUTH-DOS-3). Its sargable
// sibling [recentClosedVWAP1mCombinedTemplate] already had the right
// form.
//
// Hoisted to a package-level const for the same reason: the sargability
// regression tests in closed_vwap_at_test.go assert over the package's
// query templates, and an inline `const q` inside the function body is
// invisible to them — which is exactly how this one drifted.
//
// Deliberately NOT given a literal lower bound or the 14-day existence
// gate its neighbours use: both would change what a documented public
// endpoint SERVES (a dormant asset's last N closed buckets becoming an
// empty array), which is an owner decision, not a query-shape fix.
const recentClosedVWAP1mForPairQuery = `
        SELECT bucket, base_asset, quote_asset, vwap::text, trade_count, sources
          FROM prices_1m
         WHERE base_asset = $1
           AND quote_asset = $2
           AND bucket <= now() - INTERVAL '1 minute'
         ORDER BY bucket DESC
         LIMIT $3
    `

// limit is the caller's responsibility to clamp; this method
// assumes a sane bound and doesn't second-guess.
func (s *Store) RecentClosedVWAP1mForPair(ctx context.Context, p canonical.Pair, limit int) ([]Vwap1mRow, error) {
	rows, err := s.db.QueryContext(ctx, recentClosedVWAP1mForPairQuery,
		p.Base.String(), p.Quote.String(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Vwap1mRow, 0, limit)
	for rows.Next() {
		var row Vwap1mRow
		if err := rows.Scan(
			&row.Bucket, &row.BaseAsset, &row.QuoteAsset,
			&row.VWAP, &row.TradeCount,
			(*stringArray)(&row.Sources),
		); err != nil {
			return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair scan: %w", err)
		}
		normalizeVwapSources(&row)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair rows: %w", err)
	}
	return out, nil
}

// recentClosedVWAP1mCombinedTemplate is the newest-first, both-directions,
// LIMIT-N variant of [latestClosedVWAP1mTemplate]. `%[1]s` carries the
// literal `bucket >=` lower-bound clause for plan-time chunk pruning.
//
// It selects the RAW per-direction rows (newest bucket first) and leaves
// the direction combine to [combineDirVWAP] in Go: the fold is exact
// rational money math (ADR-0003), not a NUMERIC expression whose
// intermediate `1.0 / vwap` would round the flipped leg before it was
// weighted. `LIMIT $3` is a ROW cap ([bucketRowCap]) — a bucket has at
// most two rows, so it still bounds the walk to the requested number of
// buckets while keeping the plan a plain index-ordered scan (no CTE, no
// GROUP BY, no source unnest).
const recentClosedVWAP1mCombinedTemplate = `
        SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text,
               COALESCE(trade_count, 0), sources
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket <= now() - INTERVAL '1 minute'
           %[1]s
         ORDER BY bucket DESC
         LIMIT $3
    `

// RecentClosedVWAP1mCombined returns up to `limit` most-recent CLOSED
// 1-minute buckets from the prices_1m CAGG for the pair, newest-first,
// each COMBINED across both stored market directions (the volume-weighted
// union of [combineDirVWAP], identical to the math
// [LatestClosedVWAP1mForPair] serves) so every row expresses the price of
// Base in Quote.
//
// This is the trailing-baseline source for the /v1/price serving-sanity
// guard (aggregate.GuardServedVWAP): the handler compares the latest
// closed bucket against these recent buckets and serves last-known-good
// when the latest is grossly off (a fat-finger / manipulation print in a
// bucket the raw CAGG would otherwise serve unfiltered). The baseline
// MUST use the same combine as the served candidate — otherwise the two
// disagree systematically on every two-sided market and the guard
// false-positives.
//
// It is called ONLY after [LatestClosedVWAP1mForPair] has already
// confirmed the pair is populated (its cheap recent-existence gate ran
// first), so this bounded, index-driven LIMIT-N read never pays the
// empty-pair cold walk — but it still carries the same literal
// `bucket >=` lower bound for plan-time chunk pruning.
//
// Empty slice + nil error when the pair has no closed buckets in scope.
func (s *Store) RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]Vwap1mRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-latestVWAPWindow)
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", cutoff.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — the only interpolated value is `lower`, built from our
	// own time.Time in a fixed layout; pair strings + limit bind as
	// $1/$2/$3. Same discipline as latestClosedVWAP1m.
	q := fmt.Sprintf(recentClosedVWAP1mCombinedTemplate, lower) //nolint:gosec // G201: see note above
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String(), bucketRowCap(limit))
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mCombined: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCombinedVwap1mRows(rows, p, limit, "RecentClosedVWAP1mCombined")
}

// scanCombinedVwap1mRows folds the raw both-directions rows of a
// prices_1m read into one [Vwap1mRow] per bucket, preserving the query's
// bucket ordering. Rows of the same bucket are adjacent (callers order by
// bucket), so this is one pass with no map.
//
// Per bucket the VWAP is the volume-weighted union of the directions
// ([combineDirVWAP]), the trade count is their sum, and the source list
// is their de-duplicated union. `limit` is the caller's BUCKET limit
// (0 = unbounded); the query fetched [bucketRowCap] rows so the first
// `limit` buckets are complete and any partial tail is trimmed. `what`
// labels wrapped errors.
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

// ClosedVWAP1mAtOrBefore returns the most-recent CLOSED 1-minute
// bucket from the prices_1m CAGG for the given pair whose end
// timestamp (`bucket + 1 minute`) is at or before t. Used by
// /v1/assets/{id}'s change_24h_pct path to anchor the
// 24-hours-ago comparison price.
//
// Same closed-bucket guard as [LatestClosedVWAP1mForPair]
// (ADR-0015) — the open bucket is excluded. Returns
// [sql.ErrNoRows] when no closed bucket exists at-or-before t
// (e.g. the pair was first traded < 24h ago, or the prices_1m
// retention horizon (30 d) elided the row).
func (s *Store) ClosedVWAP1mAtOrBefore(ctx context.Context, p canonical.Pair, t time.Time) (Vwap1mRow, error) {
	const q = `
        SELECT bucket, base_asset, quote_asset, vwap::text, trade_count, sources
          FROM prices_1m
         WHERE base_asset = $1
           AND quote_asset = $2
           AND bucket + INTERVAL '1 minute' <= $3
         ORDER BY bucket DESC
         LIMIT 1
    `
	var row Vwap1mRow
	err := s.db.QueryRowContext(ctx, q,
		p.Base.String(), p.Quote.String(), t,
	).Scan(
		&row.Bucket,
		&row.BaseAsset,
		&row.QuoteAsset,
		&row.VWAP,
		&row.TradeCount,
		(*stringArray)(&row.Sources),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: ClosedVWAP1mAtOrBefore: %w", err)
	}
	normalizeVwapSources(&row)
	return row, nil
}

// VWAPAtRow is the result of [Store.ClosedVWAPAtOrBefore]: the VWAP of
// a single closed bucket at-or-before a historical instant, plus which
// CAGG resolution served it. VWAP is a decimal string exactly as
// Postgres serialises NUMERIC (ADR-0003 — no float round-trip). Bucket
// is the START of the window; the window END is
// `Bucket + Resolution.BucketDuration()`.
type VWAPAtRow struct {
	VWAP       string
	Bucket     time.Time
	Resolution HistoryGranularity
}

// priceAtResolutionLadder returns the CAGG resolutions to probe for a
// point-in-time lookup whose target instant is `age` behind now, in
// FINEST-first order. The ladder encodes the served-tier reality
// (ADR-0034): prices_1m holds the recent working set (probe it for
// anything within ~2 days), prices_1h reaches back weeks, and
// prices_1d spans to 2015 (daily OHLC). Probing coarser rungs as a
// fallback means an old instant still resolves via prices_1d even when
// the finer CAGGs never held (or have since aged out) a row that far
// back. Pure — unit-tested without a DB.
func priceAtResolutionLadder(age time.Duration) []HistoryGranularity {
	switch {
	case age <= 48*time.Hour:
		return []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}
	case age <= 45*24*time.Hour:
		return []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}
	default:
		return []HistoryGranularity{Granularity1d}
	}
}

// ClosedVWAPAtOrBefore returns the closed VWAP bucket at-or-before
// `ts` for the pair, picking the FINEST CAGG resolution whose nearest
// at-or-before bucket closes within `maxStaleness` of ts. It is the
// shared point-in-time engine behind /v1/price/at (arbitrary instant)
// and /v1/price/changes (one call per horizon).
//
// Selection: walk [priceAtResolutionLadder] finest-first; for each
// rung read the nearest closed bucket at-or-before ts; accept the
// first whose close instant (bucket + resolution) is within
// maxStaleness of ts, else fall through to the coarser rung. This is
// "pick the finest CAGG whose coverage includes ts" — a dead-market
// gap in prices_1m falls through to a fresher prices_1d bar rather
// than fabricating continuity, and the accepted rung is reported in
// the row's Resolution so callers can label `window_seconds` honestly.
//
// Both stored directions of the market are combined (the SDEX decoder
// records XLM/USDC and USDC/XLM as separate rows); flipped rows invert
// the vwap (1/vwap) so the answer always expresses the price of the
// pair's base in its quote — the same both-directions treatment
// [Store.LatestClosedVWAP1mForPair] and [Store.OHLCSeries] use.
//
// Returns [sql.ErrNoRows] when no rung has an in-window bucket (the
// pair is younger than ts, ts predates recorded history, or the
// nearest observation is staler than maxStaleness on every rung).
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

// closedVWAPAtOrBeforeQueryTemplate finds the nearest closed bucket
// at-or-before an instant for one CAGG resolution and returns that
// bucket's RAW per-direction rows. The fold into the requested
// orientation is [combineDirVWAP] in Go — the volume-weighted union of
// the two stored directions, in exact rational arithmetic.
//
//	%[1]s — table name (prices_<granularity>, from the validated enum)
//	%[2]s — upper bound literal (ts − resolution: the closed-bucket
//	        guard `bucket + resolution <= ts` written sargably as
//	        `bucket <= ts − resolution`, a constant on the RHS)
//	%[3]s — lower bound literal (plan-time chunk exclusion; set below
//	        any bucket ClosedVWAPAtOrBefore would accept)
//
// Both bounds are `bucket <= C` / `bucket >= C` against literal
// constants, so the planner prunes chunks at PLAN time (the
// [Store.LatestClosedVWAP1mForPair] discipline — a bind parameter
// would force a whole-history plan). The pair strings stay bound
// parameters ($1/$2).
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
        SELECT base_asset, vwap::text AS vwap, COALESCE(volume, 0)::text AS volume
          FROM %[1]s
         WHERE bucket = (SELECT b FROM latest)
           AND bucket >= TIMESTAMPTZ '%[3]s'
           AND ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
    )
    SELECT (SELECT b FROM latest), base_asset, vwap, volume
      FROM r
`

// closedVWAPAtOrBeforeRes runs [closedVWAPAtOrBeforeQueryTemplate] for
// one resolution. found=false (nil error) means this rung has no
// closed bucket in the pruning window — the caller falls through to a
// coarser rung. The pruning window's lower bound is set generously
// below any bucket ClosedVWAPAtOrBefore would accept, so a false here
// is never a wrongly-excluded acceptable bucket.
func (s *Store) closedVWAPAtOrBeforeRes(
	ctx context.Context, p canonical.Pair, ts time.Time, maxStaleness time.Duration, g HistoryGranularity,
) (time.Time, string, bool, error) {
	if err := g.Validate(); err != nil {
		return time.Time{}, "", false, err
	}
	res := g.BucketDuration()
	// Closed-bucket guard, sargable form: bucket + res <= ts  ⇒  bucket <= ts − res.
	upper := ts.Add(-res)
	// Lower bound strictly below the oldest acceptable bucket START
	// (ts − maxStaleness − res); the extra res of slack keeps a
	// borderline bucket inside the window so the Go-side staleness
	// check — not chunk pruning — is what rejects it.
	lower := ts.Add(-(maxStaleness + 2*res))
	const layout = "2006-01-02 15:04:05-07"
	table := "prices_" + string(g)
	// #nosec G201 — the ONLY interpolated values are `table` (composed
	// from the validated HistoryGranularity enum, never user input) and
	// two of our own time.Time bounds reformatted to a fixed layout (no
	// injection surface). Pair strings bind as $1/$2. See the template
	// doc + LatestClosedVWAP1mForPair.
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
		// No bucket on this rung, or a degenerate one carrying no usable
		// price/volume: treat as no data and fall through to the coarser
		// rung.
		return time.Time{}, "", false, nil
	}
	return bucket, vwap, true, nil
}

// LatestClosedVWAP1mForPair returns the most-recent CLOSED 1-minute
// bucket from the prices_1m CAGG for the given pair. Per ADR-0015
// the API serves only closed buckets — this method explicitly
// excludes the in-progress bucket via a `bucket + 1 minute <= now()`
// guard, even though the CAGG's refresh policy already drops the
// open bucket from materialised rows.
//
// Returns [sql.ErrNoRows] when the pair has no closed bucket yet —
// callers translate that to the API's price-not-found problem or
// fall back to the latest-trade path.
func (s *Store) LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (Vwap1mRow, error) {
	// Combine BOTH stored directions of the market into the requested
	// orientation. The SDEX decoder records the same market both ways
	// (XLM/USDC and USDC/XLM), so reading only (base=$1, quote=$2) used
	// half the liquidity — and returned ErrNoRows if the latest minute
	// happened to trade only the flipped way. We read both, and for the
	// flipped rows invert the vwap (1/vwap) so every row expresses the
	// price of $1 in $2, then trade-count-weight them within the latest
	// closed bucket. Closed-bucket-only (ADR-0015) is preserved, and the
	// combine is deterministic across regions.
	// Find the latest closed bucket via the (base,quote,bucket DESC)
	// index — one fast max() per direction, UNIONed — then point-read +
	// combine just that bucket's 1-2 rows. The earlier form scanned the
	// pair's ENTIRE prices_1m history (back to 2015) before LIMIT 1,
	// which measured ~1s warm and ballooned to ~9s under load (it drove
	// a latency-burn incident on 2026-06-19).
	//
	// PERF (two layers, both required — 2026-06-20 latency-burn incident):
	//
	//  1. The "closed bucket" predicate MUST be `bucket <= now() - 1min`, NOT
	//     `bucket + 1min <= now()`. The latter is a function on the indexed
	//     `bucket` column → non-sargable → max() runs a per-chunk partial
	//     aggregate over the WHOLE history. The sargable form lets max() read
	//     the newest chunk via the index (446ms → 26ms execution).
	//  2. That still left ~280ms of PLANNING time: prices_1m has ~374 chunks,
	//     and `now()` is only known at RUN time, so TimescaleDB does runtime
	//     (startup) chunk exclusion — the PLANNER still enumerates all 374
	//     chunks. We add a LITERAL recent lower bound (`bucket >= <cutoff>`,
	//     cutoff computed in Go) so the planner excludes old chunks at PLAN
	//     time, collapsing planning to ~2ms. The literal is our own UTC
	//     timestamp — no injection surface.
	//
	// A single bounded query — NO unbounded fallback. An earlier two-tier
	// (bounded → unbounded) made the no-data case slow again: the handler
	// reads native/fiat:USD as an alias on every XLM query, that synthetic
	// pair has zero rows, so the bounded miss fell through to the slow
	// all-chunk scan finding nothing. A pair with no closed bucket in the
	// window returns ErrNoRows, which the price handler already resolves via
	// its Redis-triangulation / last-trade fallback chain — the right path
	// for a synthetic pair, and the honest answer for a genuinely-dead asset
	// (a stale "latest" is not a current price).
	//
	// 2026-07-06 empty-alias latency incident: the two layers above make the
	// EMPTY pair cheap only WARM. The value walk's max() arms still have to
	// PROVE emptiness across the whole (generous, ~400-day) literal window —
	// min/max short-circuits when a matching row exists, but a truly-empty
	// (base,quote) forces touching every chunk in the window to conclude "no
	// rows". COLD (post-ARC-eviction, decompressing hundreds of old chunks)
	// that is minutes, not milliseconds, and /v1/price?asset=native timed out
	// on the native/fiat:USD alias probe BEFORE the fast crypto:XLM/fiat:USD
	// alias was ever tried. So gate the value walk behind a cheap
	// recent-existence probe bounded to the last latestVWAPGateWindow: a
	// populated pair short-circuits at the first row (one recent chunk); a
	// truly-empty pair proves emptiness over only ~2 weeks of recent (hot,
	// mostly-uncompressed) chunks and returns ErrNoRows. The gate — NOT the
	// value walk's window — is the freshness horizon: a pair with no closed
	// 1-minute bucket in a fortnight is not "currently priced", and the
	// handler's fallback chain surfaces its last trade with an honest
	// observed_at. Reordering the handler's aliases can't fix this (it just
	// moves the empty walk onto the SDEX native/<asset> pairs); the gate
	// makes the empty case cheap for EVERY pair. On a gate HIT the value walk
	// below is byte-identical to the pre-incident path (combined-direction,
	// literal-cutoff pruned) and returns the same recent bucket as before.
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

// latestVWAPWindow bounds the lookback for [LatestClosedVWAP1mForPair]. A
// LITERAL cutoff this many days back is interpolated into the query so the
// planner prunes old chunks at PLAN time (planning ~6ms vs ~280ms unbounded).
// 400 days is generous — it covers every recently-traded pair while still
// excluding most of prices_1m's decade of chunks.
const latestVWAPWindow = 400 * 24 * time.Hour

// latestVWAPGateWindow bounds the cheap recent-existence probe
// [LatestClosedVWAP1mForPair] runs BEFORE its combined-direction value
// walk (2026-07-06 empty-alias latency incident). It is the price
// surface's freshness horizon: a pair with no closed 1-minute VWAP
// bucket in the last two weeks is treated as "not currently priced" —
// the read returns [sql.ErrNoRows] and the /v1/price handler resolves it
// via its Redis-triangulation / last-trade fallback chain. Two weeks is
// generous for an on-chain price surface whose freshness contract is
// minutes, yet small enough that PROVING a pair empty touches only a
// fortnight of recent (hot, mostly-uncompressed) prices_1m chunks —
// cheap even cold — instead of the value walk's ~400-day span. It MUST
// stay < [latestVWAPWindow]: on a gate HIT the value walk (bounded by the
// wider window) always finds the just-confirmed recent bucket.
const latestVWAPGateWindow = 14 * 24 * time.Hour

// recentClosedVWAP1mExistsTemplate is the recent-existence gate query.
// `%[1]s` carries the literal lower-bound clause (plan-time chunk
// pruning — the same discipline as [latestClosedVWAP1mTemplate]). It
// probes BOTH stored market directions: the SDEX decoder records XLM/USDC
// and USDC/XLM as separate rows, so a one-direction probe could miss a
// flipped-only bucket and wrongly gate a live pair to ErrNoRows. LIMIT 1
// makes a populated pair short-circuit at the first matching row (one
// recent chunk) rather than scanning the window.
const recentClosedVWAP1mExistsTemplate = `
        SELECT 1
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket <= now() - INTERVAL '1 minute'
           %[1]s
         LIMIT 1
    `

// recentClosedVWAP1mExists reports whether the pair has at least one
// CLOSED 1-minute bucket (in either stored direction) at or after
// `since`. It is the cheap gate in front of
// [LatestClosedVWAP1mForPair]'s value walk: a hit means a live pair (do
// the walk); a miss returns (false, nil) so the caller returns
// [sql.ErrNoRows] and the handler falls back. Both the closed-bucket
// guard (`bucket <= now() - 1 minute`, sargable — no function on the
// indexed column) and the literal lower bound prune chunks, so an empty
// pair proves emptiness over the recent chunks only.
func (s *Store) recentClosedVWAP1mExists(ctx context.Context, p canonical.Pair, since time.Time) (bool, error) {
	// Literal lower bound (our own timestamp, not user input), so
	// TimescaleDB does PLAN-time chunk exclusion — a $N bind parameter
	// would force a whole-history plan. Same discipline as
	// latestClosedVWAP1m.
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

// RecentClosedVWAP1mExists reports whether the pair has at least one
// CLOSED 1-minute VWAP bucket (in either stored direction) within the
// price surface's default freshness horizon ([latestVWAPGateWindow], two
// weeks). It is the EXPORTED sibling of the unexported gate
// [LatestClosedVWAP1mForPair] runs internally — same query, same bounded
// window — exposed for the /v1/price stablecoin-proxy fallback layer.
//
// Why the proxy needs it: `tryStablecoinFiatProxy` walks the operator's
// USD-pegged classics calling `LatestPrice(asset, <peg>)`. A peg's quote
// is a classic asset (USDC-GA5Z…), so on a VWAP miss `LatestPrice` does
// NOT take the synthetic-fiat fast path — it falls through to
// [Store.LatestTradesForPair], an UNBOUNDED last-trade scan. For an
// empty proxy pair (e.g. a pure-Soroban token that only trades vs XLM,
// so <token>/USDC has zero rows) that is a cold full-history walk, and
// the proxy repeats it for EVERY peg. Gating each peg on this cheap
// bounded probe lets the proxy skip empty pairs before the walk — the
// same 2026-07-06 empty-alias latency fix, applied at the proxy layer.
// A hit means a live proxy pair (do the `LatestPrice` read, whose VWAP
// path is itself gated + fast); a miss means skip the peg.
func (s *Store) RecentClosedVWAP1mExists(ctx context.Context, p canonical.Pair) (bool, error) {
	since := time.Now().UTC().Add(-latestVWAPGateWindow)
	return s.recentClosedVWAP1mExists(ctx, p, since)
}

// latestClosedVWAP1m runs the combined-direction latest-closed-bucket query
// with a LITERAL `bucket >= since` lower bound on every prices_1m scan, so the
// planner prunes old chunks at PLAN time (see [LatestClosedVWAP1mForPair]).
// Returns [sql.ErrNoRows] when no closed bucket exists for the pair within the
// window.
//
// latestClosedVWAP1mTemplate is the query with a single `%[1]s` slot for the
// literal lower-bound clause, repeated on all three prices_1m scans.
//
// The final SELECT returns the latest closed bucket's RAW per-direction
// rows (at most two — the market's two stored orientations); the fold
// into the requested ($1, $2) orientation is [combineDirVWAP] in Go,
// which is exact rational arithmetic on the served price (ADR-0003)
// rather than a NUMERIC expression that would round `1.0 / vwap` before
// weighting it. The `latest` / `r` CTEs — and therefore every
// sargability + plan-time-pruning property of this query — are
// unchanged.
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
            SELECT base_asset, vwap::text AS vwap, COALESCE(volume, 0)::text AS volume,
                   COALESCE(trade_count, 0) AS tc, sources
              FROM prices_1m
             WHERE bucket = (SELECT b FROM latest)
               %[1]s
               AND ((base_asset = $1 AND quote_asset = $2)
                 OR (base_asset = $2 AND quote_asset = $1))
        )
        SELECT (SELECT b FROM latest), base_asset, vwap, volume, tc, sources
          FROM r
    `

func (s *Store) latestClosedVWAP1m(ctx context.Context, p canonical.Pair, since time.Time) (Vwap1mRow, error) {
	// Literal lower bound, interpolated (our own timestamp, not user input).
	// Applied to both max() arms AND the point-read so all three prices_1m
	// scans get plan-time chunk exclusion.
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", since.Format("2006-01-02 15:04:05-07"))
	// G201 is suppressed below: the ONLY interpolated value is `lower`, built
	// from our own time.Time formatted to a fixed layout — never user input, no
	// injection surface. The pair strings stay bound parameters ($1/$2). The
	// literal (vs a $N bind parameter) is REQUIRED: TimescaleDB only does
	// plan-time chunk exclusion for a constant, not a bind parameter.
	q := fmt.Sprintf(latestClosedVWAP1mTemplate, lower) //nolint:gosec // G201: see note above
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String())
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: LatestClosedVWAP1mForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// One bucket, so at most two rows (one per stored direction). An
	// empty result — no closed bucket in the window, or a bucket whose
	// rows carry no usable price/volume — stays [sql.ErrNoRows], which
	// the price handler resolves through its fallback chain.
	out, err := scanCombinedVwap1mRows(rows, p, 1, "LatestClosedVWAP1mForPair")
	if err != nil {
		return Vwap1mRow{}, err
	}
	if len(out) == 0 {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	return out[0], nil
}

// TimedVWAPsForPair1m returns chronologically-ordered (oldest-first)
// (vwap, bucket_end) pairs from prices_1m for the half-open window
// [from, to). Used by the multi-window baseline refresher (which
// needs the timestamp to apply [baseline.SplitByLookback] for the
// 1d / 7d / 30d sub-windows).
//
// The bucket-end timestamp is the bucket's start + 1 minute (CAGG
// stores the start; the API surface uses the end). Empty slice +
// nil error when the pair has no closed buckets in the window.
func (s *Store) TimedVWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]domain.BaselineTimedVWAP, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions of the market into the requested
	// ($1, $2) orientation — same fix as LatestClosedVWAP1mForPair. The
	// SDEX decoder records the same market both ways (XLM/USDC and
	// USDC/XLM); reading only (base=$1, quote=$2) fed the anomaly
	// baseline half the liquidity (and no rows at all for minutes that
	// traded only the flipped way). See canonical.Orient.
	//
	// The combine is the VOLUME-weighted union — Σ(requested quote leg)
	// / Σ(requested base leg), each row's legs re-expressed in the
	// requested orientation via Σquote = vwap·Σbase (the identity
	// [combineDirVWAP] and [Store.OHLCSeries] use). Weighting the two
	// directions by TRADE COUNT, as this did, is not a VWAP at all and
	// biased every baseline on a two-sided market. The division stays in
	// NUMERIC and only the final value casts to float8 — this series
	// feeds statistical baseline math whose float contract is documented
	// on [Store.VWAPsForPair1m].
	const q = `
        SELECT (SUM(CASE WHEN base_asset = $1 THEN vwap * COALESCE(volume, 0)
                         ELSE COALESCE(volume, 0) END)
                  / NULLIF(SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume, 0)
                                    ELSE vwap * COALESCE(volume, 0) END), 0))::float8 AS vwap,
               bucket + INTERVAL '1 minute'
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket >= $3
           AND bucket <  $4
         GROUP BY bucket
        HAVING SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume, 0)
                        ELSE vwap * COALESCE(volume, 0) END) > 0
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
		var t domain.BaselineTimedVWAP
		if err := rows.Scan(&t.VWAP, &t.BucketEnd); err != nil {
			return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m rows: %w", err)
	}
	return out, nil
}

// VWAPsForPair1m returns chronologically-ordered (oldest-first) VWAP
// values from prices_1m where bucket falls in [from, to). Used by
// the baseline refresher to pull the 30-day training window for a
// pair. Returns the bare float series (not the full Vwap1mRow) —
// the caller's downstream consumer (`baseline.ReturnsFromVWAPs`)
// only needs the prices, not the metadata.
//
// Empty slice + nil error when the pair has no closed buckets in
// the window.
//
// VWAP is parsed from the NUMERIC column via the Postgres double-
// precision cast — the baseline math runs in float64 anyway and
// the small precision loss on a per-bucket VWAP doesn't matter for
// statistical aggregates over hundreds of buckets.
func (s *Store) VWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]float64, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions into the requested ($1, $2)
	// orientation — the VOLUME-weighted union, so the 30-day baseline
	// training window reflects the full market and not just the
	// direction the CAGG happened to store. See TimedVWAPsForPair1m /
	// combineDirVWAP / canonical.Orient.
	const q = `
        SELECT (SUM(CASE WHEN base_asset = $1 THEN vwap * COALESCE(volume, 0)
                         ELSE COALESCE(volume, 0) END)
                  / NULLIF(SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume, 0)
                                    ELSE vwap * COALESCE(volume, 0) END), 0))::float8 AS vwap
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket >= $3
           AND bucket <  $4
         GROUP BY bucket
        HAVING SUM(CASE WHEN base_asset = $1 THEN COALESCE(volume, 0)
                        ELSE vwap * COALESCE(volume, 0) END) > 0
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

// stringArray is a [sql.Scanner] for Postgres TEXT[] / VARCHAR[]
// columns scanning into a Go []string. Used by the `sources` column
// in prices_1m.
//
// Implements minimal parsing of the Postgres array text format:
// `{a,b,c}`. Quoted entries (with embedded commas) aren't supported
// — fine here because source names are identifier-shaped.
type stringArray []string

// normalizeVwapSources enforces a stable lexical ordering for the
// materialized-view `sources` array before it crosses the storage
// boundary. The current CAGG SQL uses `array_agg(DISTINCT source)`
// without an ORDER BY, so sorting here restores the public contract's
// deterministic contributor ordering for both existing and future rows
// without requiring an immediate CAGG rebuild.
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

// Volume24hUSDForAsset returns the trailing 24h USD-denominated
// trade volume for an asset, summing across every pair the asset
// participates in (as base OR quote). Reads from the prices_1m
// CAGG (1440 rolled buckets per 24h) — far cheaper than scanning
// the trades hypertable directly.
//
// Returns "0" when the asset has zero trades in the window;
// callers presenting the field should treat zero distinctly from
// null (zero = "asset existed and had no trades", null = "asset
// not tracked"). The string return matches the rest of the
// aggregate API — Postgres NUMERIC sums don't fit cleanly into
// any Go fixed-width type.
//
// assetKey is the canonical asset string (e.g.
// "native", "USDC:GA5...", "soroban:CC..."); matches what the
// trades hypertable stores in base_asset / quote_asset.
//
// # Scope caveat (launch-readiness L2.2)
//
// The CAGG sums `coalesce(usd_volume, 0)` per row. Per-trade
// `usd_volume` is populated at insert time (see `tradeUSDVolume`
// in trades.go) for:
//   - off-chain CEX/FX sources with `fiat:USD` or
//     USD-pegged-stablecoin quotes (uniform 10^8 external scale),
//   - on-chain DEX sources whose quote asset is in the operator's
//     `[trades].usd_pegged_classic_assets` allow-list or its SAC
//     wrapper, transitive via `[supply.sac_wrappers]` (L2.2 phase 1).
//
// Deployments that haven't configured the trades allow-list see
// on-chain trades contribute 0 to this sum (the pre-Phase-1
// default). On-chain trades quoted in non-USD assets (XLM/AQUA,
// XLM/BTC) still contribute 0; FX-anchor multiplication for
// non-USD on-chain quotes is L2.2 phase 2 (post-launch). The
// OpenAPI surface (`volume_24h_usd`) carries the same caveat.
func (s *Store) Volume24hUSDForAsset(ctx context.Context, assetKey string) (string, error) {
	// Alias-complete membership: an asset's served volume is the sum
	// across ALL its canonical forms (XLM's native / crypto:XLM / SAC
	// split, plus any configured classic↔SAC wrapper), not just the one
	// spelling the caller passed. Keying on a single form silently
	// omitted the crypto:XLM (CEX) and SAC (Soroban) legs.
	const q = `
        SELECT COALESCE(sum(volume_usd), 0)::text
          FROM prices_1m
         WHERE (base_asset = ANY($1) OR quote_asset = ANY($1))
           AND bucket >= now() - INTERVAL '24 hours'
           AND bucket  < now()
    `
	var out string
	if err := s.db.QueryRowContext(ctx, q, assetAliasArray(assetKey)).Scan(&out); err != nil {
		return "", fmt.Errorf("timescale: Volume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return out, nil
}

// OHLCBar is one bucket of OHLC + volume + trade-count returned by
// [Store.OHLCSeries] / [Store.OHLCSeriesReBucketed]. Bucket is the
// START of the window; window end = bucket + interval.
//
// All price fields are decimal strings from the Postgres NUMERIC
// column (no float round-trip). Volume fields are integer-string
// sums (base/quote stroops). TradeCount is the row count.
type OHLCBar struct {
	Bucket      time.Time
	Open        string
	High        string
	Low         string
	Close       string
	BaseVolume  string
	QuoteVolume string
	TradeCount  int64
}

// OHLCSeries returns chronologically-ordered (oldest-first) OHLC
// bars from the CAGG matching `granularity` for the half-open
// window [from, to). Used by /v1/ohlc's multi-bar mode (F-0071).
//
// Bucket rule: the CAGG's native bucket size IS the interval, so
// rows map 1:1 to bars — no SQL-side re-bucketing. Callers that
// need a non-CAGG-native interval (5m, 30m, 4h) route through
// [Store.OHLCSeriesReBucketed].
//
// Per ADR-0015 the in-progress bucket is excluded via a
// `bucket + <interval> <= now()` guard. `limit` clamps row count
// (0 = unbounded). Returns empty slice + nil error when no
// closed buckets exist in window.
//
// `quote_amount` is derived as `vwap * volume` at SELECT time:
// VWAP is defined as Σ(price·base) / Σ(base), so vwap·Σ(base) =
// Σ(price·base) = Σ(quote). This is exact in NUMERIC arithmetic
// — no precision loss vs storing volume_quote in the CAGG.
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
	// Combine BOTH stored directions of the market into the requested
	// ($1, $2) orientation (the SDEX decoder records XLM/USDC and
	// USDC/XLM as separate rows). The `norm` CTE re-expresses each row in
	// the requested orientation: flipped rows invert every price
	// (1/price) — which SWAPS high↔low — and swap base↔quote volume. Then
	// per bucket: high = max, low = min (order-independent extrema);
	// open/close prefer the requested-direction row and fall back to the
	// inverted flipped row (their intra-bucket ordering across directions
	// is unknowable from the CAGG); base/quote volume + trade_count sum.
	// See canonical.Orient / canonOrientSQL.
	// #nosec G201 — table + interval are derived from the validated
	// HistoryGranularity enum, not user input. See Validate.
	q := fmt.Sprintf(`
		WITH norm AS (
		    SELECT
		        bucket,
		        (base_asset = $1) AS req,
		        CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		        CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		        CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		        CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		        CASE WHEN base_asset = $1 THEN volume        ELSE vwap * volume END AS base_vol,
		        CASE WHEN base_asset = $1 THEN vwap * volume ELSE volume        END AS quote_vol,
		        trade_count AS tc
		      FROM %s
		     WHERE ((base_asset = $1 AND quote_asset = $2)
		         OR (base_asset = $2 AND quote_asset = $1))
		       AND bucket >= $3
		       AND bucket <  $4
		       AND bucket + INTERVAL '%s' <= now()
		)
		SELECT
		    bucket,
		    COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1])::text AS open,
		    max(hi)::text                                                           AS high,
		    min(lo)::text                                                           AS low,
		    COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1])::text AS close,
		    sum(base_vol)::text                                                     AS base_volume,
		    sum(quote_vol)::text                                                    AS quote_volume,
		    sum(tc)::bigint                                                         AS trade_count
		  FROM norm
		 GROUP BY bucket
		 ORDER BY bucket ASC
	`, table, interval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeries[%s] scan: %w", granularity, err)
		}
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s] rows: %w", granularity, err)
	}
	return out, nil
}

// OHLCSeriesReBucketed is [Store.OHLCSeries] but re-buckets the
// source CAGG's rows into a coarser `outInterval` via Postgres
// `time_bucket`. Supports the requested intervals that don't have
// a native CAGG (5m, 30m, 4h) while still reading from a CAGG
// rather than the trades hypertable. Folds N source buckets into
// one output bucket per the standard OHLC roll-up:
//
//   - open  = first_price ORDERED BY bucket ASC  (first input bar's open)
//   - close = last_price  ORDERED BY bucket ASC  (last input bar's close)
//   - high  = max(high_price)
//   - low   = min(low_price)
//   - base_volume  = Σ volume
//   - quote_volume = Σ (vwap * volume)  (Σ quote per the VWAP identity)
//   - trade_count  = Σ trade_count
//
// `outInterval` MUST be an integer multiple of the source CAGG's
// native bucket size — caller responsibility (e.g. granularity=1m
// + outInterval='5 minutes'). Postgres time_bucket snaps to its
// configured origin, which for our CAGGs is the Unix epoch (UTC).
// 5m buckets land at 12:00/12:05/12:10..., 4h at 00:00/04:00/...
//
// `outInterval` composes directly into the SQL after a literal
// allow-list check — never user-passed verbatim. Same ADR-0015
// closed-bucket guard as [Store.OHLCSeries].
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
	// Allow-list — outInterval composes directly into the SQL, so
	// it MUST NOT come from untrusted input. The handler maps
	// fixed-enum interval strings to these Postgres literals.
	switch outInterval {
	case "5 minutes", "15 minutes", "30 minutes", "1 hour", "4 hours",
		"1 day", "1 week":
	default:
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed: outInterval %q not in allow-list", outInterval)
	}
	table := "prices_" + string(sourceGranularity)
	// Combine BOTH stored directions of the market before re-bucketing:
	// the `norm` CTE collapses each SOURCE bucket's two directions into
	// one bar expressed in the requested ($1, $2) orientation (flipped
	// rows invert prices — swapping high↔low — and swap base↔quote
	// volume; open/close prefer the requested direction), and the outer
	// query folds those normalized source bars into the coarser
	// out_bucket. See OHLCSeries / canonical.Orient.
	// #nosec G201 — table comes from the validated enum;
	// outInterval comes from the allow-list above. No user input
	// reaches the SQL string.
	q := fmt.Sprintf(`
		WITH norm AS (
		    SELECT bucket,
		           COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1]) AS open,
		           max(hi) AS high,
		           min(lo) AS low,
		           COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1]) AS close,
		           sum(base_vol)  AS base_vol,
		           sum(quote_vol) AS quote_vol,
		           sum(tc)        AS tc
		      FROM (
		        SELECT bucket, (base_asset = $1) AS req,
		               CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		               CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		               CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		               CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		               CASE WHEN base_asset = $1 THEN volume        ELSE vwap * volume END AS base_vol,
		               CASE WHEN base_asset = $1 THEN vwap * volume ELSE volume        END AS quote_vol,
		               trade_count AS tc
		          FROM %[1]s
		         WHERE ((base_asset = $1 AND quote_asset = $2)
		             OR (base_asset = $2 AND quote_asset = $1))
		           AND bucket >= $3
		           AND bucket <  $4
		      ) raw
		     GROUP BY bucket
		)
		SELECT
		    time_bucket(INTERVAL '%[2]s', bucket)                       AS out_bucket,
		    (array_agg(open  ORDER BY bucket ASC))[1]::text             AS open,
		    max(high)::text                                             AS high,
		    min(low)::text                                              AS low,
		    (array_agg(close ORDER BY bucket DESC))[1]::text            AS close,
		    sum(base_vol)::text                                         AS base_volume,
		    sum(quote_vol)::text                                        AS quote_volume,
		    sum(tc)::bigint                                             AS trade_count
		  FROM norm
		 GROUP BY out_bucket
		 HAVING time_bucket(INTERVAL '%[2]s', bucket) + INTERVAL '%[2]s' <= now()
		 ORDER BY out_bucket ASC
	`, table, outInterval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s]: %w", sourceGranularity, outInterval, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] scan: %w", sourceGranularity, outInterval, err)
		}
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] rows: %w", sourceGranularity, outInterval, err)
	}
	return out, nil
}

// MarketSubstance summarises a pair's trailing closed-bucket market
// activity across BOTH stored directions of the market, as measured by
// the prices_1m CAGG. It is the input to the serving-side thin-market
// gate (internal/pricingguard.SubstanceGate): "is there enough real,
// recent market here for an aggregated price claim to be publishable?"
//
// VolumeUSD is the NUMERIC sum of prices_1m.volume_usd over the window
// as a decimal string (ADR-0003 — the comparison against the serve
// floor is exact-rational, never float64). Buckets is the number of
// DISTINCT closed 1-minute buckets with at least one trade; SpanSeconds
// is max(bucket)-min(bucket) — how much wall-clock the activity spans.
// Buckets + SpanSeconds together are the persistence signal: a burst of
// N trades in one minute produces Buckets=1 / SpanSeconds=0 no matter
// how large N is, which is exactly what distinguishes a dust-set market
// from a continuously-traded one.
//
// Rows whose usd_volume could not be valued at insert time contribute 0
// to VolumeUSD (the CAGG stores sum(coalesce(usd_volume, 0))) — so an
// unvaluable market fails a positive volume floor BY CONSTRUCTION,
// which is the fail-closed posture the gate wants: if the volume cannot
// be verified, the floor cannot be verified.
type MarketSubstance struct {
	VolumeUSD   string
	Buckets     int64
	SpanSeconds int64
}

// PairMarketSubstance measures [MarketSubstance] for the pair over the
// trailing `window` (closed buckets only, ADR-0015). Same plan-time
// chunk-pruning discipline as the other prices_1m readers in this file:
// the lower bound is interpolated as a LITERAL timestamptz computed in
// Go (no injection surface), and the closed-bucket predicate is the
// sargable `bucket <= now() - 1min` form.
//
// An empty pair returns {VolumeUSD: "0", Buckets: 0, SpanSeconds: 0}
// with a nil error — absence of market is a measurement, not an error.
func (s *Store) PairMarketSubstance(ctx context.Context, p canonical.Pair, window time.Duration) (MarketSubstance, error) {
	if window <= 0 {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstance: non-positive window %v", window)
	}
	cutoff := time.Now().UTC().Add(-window)
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", cutoff.Format("2006-01-02 15:04:05-07"))
	// #nosec G201 — the only interpolated value is `lower`, built from our
	// own time.Time in a fixed layout; pair strings bind as $1/$2. Same
	// discipline as latestClosedVWAP1m / RecentClosedVWAP1mCombined.
	q := fmt.Sprintf(`
        SELECT COALESCE(sum(bucket_usd), 0)::text,
               count(*),
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0)
          FROM (
            SELECT bucket, sum(volume_usd) AS bucket_usd
              FROM prices_1m
             WHERE ((base_asset = $1 AND quote_asset = $2)
                 OR (base_asset = $2 AND quote_asset = $1))
               AND bucket <= now() - INTERVAL '1 minute'
               %[1]s
             GROUP BY bucket
          ) b
    `, lower) //nolint:gosec // G201: see note above
	var sub MarketSubstance
	if err := s.db.QueryRowContext(ctx, q, p.Base.String(), p.Quote.String()).Scan(
		&sub.VolumeUSD, &sub.Buckets, &sub.SpanSeconds,
	); err != nil {
		return MarketSubstance{}, fmt.Errorf("timescale: PairMarketSubstance: %w", err)
	}
	return sub, nil
}
