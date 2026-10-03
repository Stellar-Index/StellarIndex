package forex

import (
	"testing"
	"time"
)

func TestShouldRefreshHistory_UsesNewestBarAcrossTickers(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	published := day(10).Add(7 * time.Hour)

	cases := []struct {
		name string
		hist map[string][]HistoryPoint
		want bool
	}{
		{"empty", nil, true},
		{"all tickers empty", map[string][]HistoryPoint{"EUR": {}}, true},
		{"current everywhere", map[string][]HistoryPoint{
			"EUR": {{Date: day(10)}}, "GBP": {{Date: day(10)}},
		}, false},
		{"one ticker stale, others current", map[string][]HistoryPoint{
			"EUR": {{Date: day(10)}}, "OLD": {{Date: day(3)}}, "GBP": {{Date: day(10)}},
		}, false},
		{"all behind publish day", map[string][]HistoryPoint{
			"EUR": {{Date: day(9)}}, "OLD": {{Date: day(3)}},
		}, true},
	}
	w := &Worker{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Map order is random; repeat so a single-ticker sample cannot pass by luck.
			for i := 0; i < 50; i++ {
				if got := w.shouldRefreshHistory(tc.hist, published); got != tc.want {
					t.Fatalf("iteration %d: got %v, want %v", i, got, tc.want)
				}
			}
		})
	}
}
