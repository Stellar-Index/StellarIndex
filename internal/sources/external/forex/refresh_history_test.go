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
