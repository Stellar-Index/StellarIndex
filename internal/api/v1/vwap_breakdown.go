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

// VWAPSourceBreakdown is one venue's contribution to one /v1/vwap bucket in the response's volume units; Weight is a decimal string floored to 10 places, and Price is null when the outlier filter left no trades.
type VWAPSourceBreakdown struct {
	Source           string  `json:"source"`
	Price            *string `json:"price"`
	BaseVolume       string  `json:"base_volume"`
	QuoteVolume      string  `json:"quote_volume"`
	TradeCount       int     `json:"trade_count"`
	Weight           string  `json:"weight"`
	OutliersExcluded int     `json:"outliers_excluded"`
}

// VWAPBreakdownBucket is one time bucket holding a fetched trade, with UTC-aligned Start/End not clamped to the window.
type VWAPBreakdownBucket struct {
	Start       WireTime              `json:"start"`
	End         WireTime              `json:"end"`
	QuoteVolume string                `json:"quote_volume"`
	TradeCount  int                   `json:"trade_count"`
	Sources     []VWAPSourceBreakdown `json:"sources"`
}

// VWAPBreakdown is the opt-in ?breakdown=source block; Truncated mirrors the response, making the breakdown a lower bound, and Interval is null when no ?interval= was given.
type VWAPBreakdown struct {
	Interval  *string               `json:"interval"`
	Truncated bool                  `json:"truncated"`
	Buckets   []VWAPBreakdownBucket `json:"buckets"`
}

// parseVWAPBreakdown validates ?breakdown= and ?interval=: nil interval with ok=true means no opt-in, "" means one bucket; ok=false means
// a problem+json is written.
func parseVWAPBreakdown(w http.ResponseWriter, r *http.Request) (interval *ohlcInterval, ok bool) {
	q := r.URL.Query()
	rawBreakdown, rawInterval := q.Get("breakdown"), q.Get("interval")
	if rawBreakdown == "" {
		if rawInterval != "" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-interval",
				"Invalid interval", http.StatusBadRequest,
				"interval is only valid together with breakdown=source")
			return nil, false
		}
		return nil, true
	}
	if rawBreakdown != vwapBreakdownSource {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-breakdown",
			"Invalid breakdown", http.StatusBadRequest,
			"breakdown must be \""+vwapBreakdownSource+"\" or omitted")
		return nil, false
	}
	var iv ohlcInterval
	if rawInterval != "" {
		if iv, ok = parseOHLCInterval(w, r, rawInterval); !ok {
			return nil, false
		}
	}
	return &iv, true
}

// vwapBreakdown builds the ?breakdown=source block, or nil when interval is nil; prices get the same nonstandard-decimals correction as
// the headline.
func (s *Server) vwapBreakdown(
	interval *ohlcInterval, pair canonical.Pair, pre, post []canonical.Trade, from, to time.Time, truncated bool,
) *VWAPBreakdown {
	if interval == nil {
		return nil
	}
	adjust := func(p *big.Rat) *big.Rat {
		return aggregate.AdjustPrice(p,
			aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Base),
			aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Quote))
	}
	return buildVWAPBreakdown(pre, post, *interval, from, to, adjust, truncated)
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
	default:
		d := interval.duration()
		s := t.UTC().Truncate(d)
		return s, s.Add(d)
	}
}

type vwapBucketTrades struct {
	start, end time.Time
	pre, post  []canonical.Trade
}

// buildVWAPBreakdown buckets pre- and post-outlier-filter trades and reduces each per source ([aggregate.SourceContributions]);
// exclusions are pre minus post, since the filter records none.
func buildVWAPBreakdown(
	pre, post []canonical.Trade, interval ohlcInterval, from, to time.Time,
	adjust func(*big.Rat) *big.Rat, truncated bool,
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
		out.Buckets = append(out.Buckets, vwapBucket(b, adjust))
	}
	return out
}

func vwapBucket(b *vwapBucketTrades, adjust func(*big.Rat) *big.Rat) VWAPBreakdownBucket {
	// b.post is already at the window's one scale, so the weights compare
	// like with like and the volumes match the headline sums.
	contribs := aggregate.SourceContributions(b.post)
	weights := make(map[string]*big.Rat, len(contribs))
	for _, c := range contribs {
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
	for _, s := range bySrc {
		sources = append(sources, s)
	}
	// Rank by weight, the exact quote-volume share; b.post is at the
	// window's one scale, so this orders the same as the volumes shown.
	weightOf := func(src string) *big.Rat {
		if w := weights[src]; w != nil {
			return w
		}
		return new(big.Rat)
	}
	sort.Slice(sources, func(i, j int) bool {
		if c := weightOf(sources[i].Source).Cmp(weightOf(sources[j].Source)); c != 0 {
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
