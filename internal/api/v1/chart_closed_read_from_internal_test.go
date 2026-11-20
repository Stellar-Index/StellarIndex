package v1

import (
	"testing"
	"time"
)

// TestChartClosedReadFrom_EveryTimeframeAddressesAClosedBucket applies the store's closed-bucket predicate
// (bucket >= from AND bucket + width <= now) to every timeframe at each forced grain: a window shorter than one
// bucket must still address the last closed one, or the chart answers an empty 200 for a priced asset.
func TestChartClosedReadFrom_EveryTimeframeAddressesAClosedBucket(t *testing.T) {
	day := 24 * time.Hour
	for _, now := range []time.Time{
		time.Date(2026, 6, 10, 0, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 10, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 6, 10, 13, 47, 0, 0, time.UTC),
	} {
		for tf, spec := range chartTimeframes {
			if spec.Duration == 0 {
				continue
			}
			from := now.Add(-spec.Duration)
			for gran, width := range map[string]time.Duration{"1h": time.Hour, "1d": day} {
				readFrom := chartClosedReadFrom(now, from, gran)
				last := now.Truncate(width).Add(-width)
				if last.Before(readFrom) {
					t.Errorf("now=%s timeframe=%s gran=%s: read from %s misses the last closed bucket %s", now, tf, gran, readFrom, last)
				}
				if readFrom.After(from) {
					t.Errorf("now=%s timeframe=%s gran=%s: read from %s is later than the window start %s", now, tf, gran, readFrom, from)
				}
			}
			if qFrom, qTo := fxClosedDayWindow(now, from); qFrom.After(now.Truncate(day).Add(-day)) || !qTo.After(qFrom) {
				t.Errorf("now=%s timeframe=%s: fx window [%s, %s] holds no closed day", now, tf, qFrom, qTo)
			}
		}
	}
}
