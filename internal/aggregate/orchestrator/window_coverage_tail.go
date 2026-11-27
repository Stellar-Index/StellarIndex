package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// minuteSums returns the leg's minute rows for [from, m). Minutes at or past the view's watermark (lowered to its earliest pending
// invalidation) are computed from raw trades when they span at most [timescale.TradeSums1mLiveMaxSpan]: a burst can make the raw
// part shorter than the view's refresh lag, and those minutes would otherwise be served by neither. A non-empty reason refuses.
func (o *Orchestrator) minuteSums(
	ctx context.Context, ss TradeSumsStore, fetch canonical.Pair, from, m time.Time,
) ([]timescale.TradeMinuteSum, string) {
	sums, watermark, err := ss.TradeSums1mInRange(ctx, fetch, from, m)
	if err != nil {
		return nil, "minute-sums read failed: " + err.Error()
	}
	if !watermark.Before(m) {
		return sums, ""
	}
	start := watermark.Truncate(time.Minute)
	if start.Before(from) {
		start = from
	}
	if tail := m.Sub(start); tail > timescale.TradeSums1mLiveMaxSpan {
		return nil, fmt.Sprintf("minute sums materialised to %s, %.0fs behind the raw part: tail %s beyond the %s cap",
			watermark.UTC().Format(time.RFC3339), m.Sub(watermark).Seconds(), tail, timescale.TradeSums1mLiveMaxSpan)
	}
	live, err := ss.TradeSums1mLive(ctx, fetch, start, m)
	if err != nil {
		return nil, "unmaterialised-tail read failed: " + err.Error()
	}
	// Rows at or past start are unmaterialised or stale (invalidated); the live rows replace them.
	kept := make([]timescale.TradeMinuteSum, 0, len(sums)+len(live))
	for i := range sums {
		if sums[i].Bucket.Before(start) {
			kept = append(kept, sums[i])
		}
	}
	return append(kept, live...), ""
}
