package v1

import (
	"math/big"
	"net/http"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// vwapBreakdownSource is the only accepted ?breakdown= value.
const vwapBreakdownSource = "source"

// VWAPSourceBreakdown is one venue's contribution to one bucket of a
// /v1/vwap window. Volumes are in the same units as the response's
// base_volume / quote_volume, so a bucket's sources sum to the bucket.
//
// Weight is the source's post-filter quote volume over the bucket's total
// post-filter quote volume, as a decimal string floored to 10 places (a
// bucket's weights therefore sum to 1 only to that precision). Price is
// null when the outlier filter left the source no trades in the bucket.
type VWAPSourceBreakdown struct {
	Source           string  `json:"source"`
	Price            *string `json:"price"`
	BaseVolume       string  `json:"base_volume"`
	QuoteVolume      string  `json:"quote_volume"`
	TradeCount       int     `json:"trade_count"`
	Weight           string  `json:"weight"`
	OutliersExcluded int     `json:"outliers_excluded"`
}

// VWAPBreakdownBucket is one time bucket. Only buckets holding at least
// one fetched trade appear. Start/End are the bucket's UTC-aligned bounds
// (not clamped to the window).
type VWAPBreakdownBucket struct {
	Start       WireTime              `json:"start"`
	End         WireTime              `json:"end"`
	QuoteVolume string                `json:"quote_volume"`
	TradeCount  int                   `json:"trade_count"`
	Sources     []VWAPSourceBreakdown `json:"sources"`
}

// VWAPBreakdown is the opt-in ?breakdown=source block. Truncated mirrors
// the response's `truncated`: the fetch cap was hit, so the breakdown
// covers only the newest trades of the window and is a lower bound.
// Interval is null when no ?interval= was given (one bucket spans the
// whole window).
type VWAPBreakdown struct {
	Interval  *string               `json:"interval"`
	Truncated bool                  `json:"truncated"`
	Buckets   []VWAPBreakdownBucket `json:"buckets"`
}

// parseVWAPBreakdown validates ?breakdown= and ?interval=. enabled=false
// with ok=true means the caller did not opt in. ok=false means a
// problem+json has been written.
func parseVWAPBreakdown(w http.ResponseWriter, r *http.Request) (enabled bool, interval ohlcInterval, ok bool) {
	q := r.URL.Query()
	rawBreakdown, rawInterval := q.Get("breakdown"), q.Get("interval")
	if rawBreakdown == "" {
		if rawInterval != "" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-interval",
				"Invalid interval", http.StatusBadRequest,
				"interval is only valid together with breakdown=source")
			return false, "", false
		}
		return false, "", true
	}
	if rawBreakdown != vwapBreakdownSource {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-breakdown",
			"Invalid breakdown", http.StatusBadRequest,
			"breakdown must be \""+vwapBreakdownSource+"\" or omitted")
		return false, "", false
	}
	if rawInterval == "" {
		return true, "", true
	}
	interval, ok = parseOHLCInterval(w, r, rawInterval)
	return ok, interval, ok
}

// vwapBucketBounds returns the UTC-aligned bounds of the bucket holding t.
// A zero interval is one bucket over [from, to).
func vwapBucketBounds(t time.Time, interval ohlcInterval, from, to time.Time) (time.Time, time.Time) {
	switch interval {
	case "":
		return from, to
	case ohlcInterval1mo:
		t = t.UTC()
		s := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return s, s.AddDate(0, 1, 0)
	}
	d := interval.duration()
	s := t.UTC().Truncate(d)
	return s, s.Add(d)
}

type vwapBucketTrades struct {
	start, end time.Time
	pre, post  []canonical.Trade
}

// buildVWAPBreakdown buckets the pre- and post-outlier-filter trade slices
// and reduces each bucket per source with [aggregate.SourceContributions].
// Exclusions are pre minus post per (bucket, source): the filter records
// nothing, so they are derived here. adjust applies the same
// nonstandard-decimals price correction as the headline price.
func buildVWAPBreakdown(
	pre, post []canonical.Trade, interval ohlcInterval, from, to time.Time,
	needsScaling bool, adjust func(*big.Rat) *big.Rat, truncated bool,
) *VWAPBreakdown {
	buckets := map[time.Time]*vwapBucketTrades{}
	get := func(t time.Time) *vwapBucketTrades {
		s, e := vwapBucketBounds(t, interval, from, to)
		b, ok := buckets[s]
		if !ok {
			b = &vwapBucketTrades{start: s, end: e}
			buckets[s] = b
		}
		return b
	}
	for i := range pre {
		b := get(pre[i].Timestamp)
		b.pre = append(b.pre, pre[i])
	}
	for i := range post {
		b := get(post[i].Timestamp)
		b.post = append(b.post, post[i])
	}
	ordered := make([]*vwapBucketTrades, 0, len(buckets))
	for _, b := range buckets {
		ordered = append(ordered, b)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start.Before(ordered[j].start) })

	out := &VWAPBreakdown{Truncated: truncated, Buckets: make([]VWAPBreakdownBucket, 0, len(ordered))}
	if interval != "" {
		s := string(interval)
		out.Interval = &s
	}
	for _, b := range ordered {
		out.Buckets = append(out.Buckets, vwapBucket(b, needsScaling, adjust))
	}
	return out
}

func vwapBucket(b *vwapBucketTrades, needsScaling bool, adjust func(*big.Rat) *big.Rat) VWAPBreakdownBucket {
	contribs := aggregate.SourceContributions(b.post)
	// Weights must compare like with like. A fiat-quote fetch is already
	// lifted to one scale; any other fetch is raw per-source, so lift a
	// copy for the weights only and leave the volumes matching the
	// headline sums.
	weightSrc := contribs
	if needsScaling {
		weightSrc = aggregate.SourceContributions(
			aggregate.NormalizeAmountScale(b.post, amountScaleDecimalsFor))
	}
	weights := make(map[string]*big.Rat, len(weightSrc))
	for _, c := range weightSrc {
		weights[c.Source] = c.Weight
	}
	preBySrc, postBySrc := map[string]int{}, map[string]int{}
	for i := range b.pre {
		preBySrc[b.pre[i].Source]++
	}
	for i := range b.post {
		postBySrc[b.post[i].Source]++
	}

	bySrc := map[string]VWAPSourceBreakdown{}
	for _, c := range contribs {
		var price *string
		if c.BaseVolume.Sign() > 0 {
			p := ratToDecimal(adjust(new(big.Rat).SetFrac(c.QuoteVolume, c.BaseVolume)), ohlcPriceDigits)
			price = &p
		}
		bySrc[c.Source] = VWAPSourceBreakdown{
			Source: c.Source, Price: price,
			BaseVolume: c.BaseVolume.String(), QuoteVolume: c.QuoteVolume.String(),
			TradeCount: c.TradeCount, Weight: ratToDecimal(weights[c.Source], ohlcPriceDigits),
		}
	}
	for src, n := range preBySrc {
		s, ok := bySrc[src]
		if !ok {
			s = VWAPSourceBreakdown{Source: src, BaseVolume: "0", QuoteVolume: "0", Weight: "0"}
		}
		s.OutliersExcluded = n - postBySrc[src]
		bySrc[src] = s
	}
	sources := make([]VWAPSourceBreakdown, 0, len(bySrc))
	quoteOf := make(map[string]*big.Int, len(bySrc))
	for k, s := range bySrc {
		sources = append(sources, s)
		quoteOf[k], _ = new(big.Int).SetString(s.QuoteVolume, 10)
	}
	sort.Slice(sources, func(i, j int) bool {
		if c := quoteOf[sources[i].Source].Cmp(quoteOf[sources[j].Source]); c != 0 {
			return c > 0
		}
		return sources[i].Source < sources[j].Source
	})
	return VWAPBreakdownBucket{
		Start: WireTime(b.start), End: WireTime(b.end),
		QuoteVolume: aggregate.TotalQuoteVolume(b.post).String(),
		TradeCount:  len(b.post),
		Sources:     sources,
	}
}
