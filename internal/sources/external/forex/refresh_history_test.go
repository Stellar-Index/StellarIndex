package forex

import (
	"testing"
	"time"
)

func TestShouldRefreshHistory_UsesNewestAcrossTickers(t *testing.T) {
	pub := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	w := &Worker{}

	// One ticker is missing the latest day; another is current. The
	// answer must not depend on which entry the map yields first.
	hist := map[string][]HistoryPoint{
		"AAA": {{Date: day(8)}},
		"BBB": {{Date: day(10)}},
		"CCC": {{Date: day(9)}},
		"DDD": {},
	}
	for i := 0; i < 200; i++ {
		if w.shouldRefreshHistory(hist, pub) {
			t.Fatalf("iteration %d: refreshed although a ticker already has the published day", i)
		}
	}

	stale := map[string][]HistoryPoint{
		"AAA": {{Date: day(8)}},
		"BBB": {{Date: day(9)}},
	}
	for i := 0; i < 200; i++ {
		if !w.shouldRefreshHistory(stale, pub) {
			t.Fatalf("iteration %d: no refresh although every ticker is behind", i)
		}
	}

	if !w.shouldRefreshHistory(map[string][]HistoryPoint{"AAA": {}}, pub) {
		t.Fatal("all-empty history must refresh")
	}
}

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
