package forex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRefreshOnce_ECBStandbyAttributesEachRowToItsFetcher drives a full
// refresh on a cold worker whose primary serves dated history but not
// current rates or names. The ECB standby answers the current rates, so
// the current-day rows are ECB's; the dated bars still come from the
// primary client and must say so.
func TestRefreshOnce_ECBStandbyAttributesEachRowToItsFetcher(t *testing.T) {
	const groupedPrefix = "/v2/aggs/grouped/locale/global/market/fx/"
	recent := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		date := strings.TrimPrefix(r.URL.Path, groupedPrefix)
		if !strings.HasPrefix(r.URL.Path, groupedPrefix) || date >= recent {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeGrouped(w, map[string]float64{"EUR": 0.8, "GBP": 0.68, "JPY": 128})
	}))
	t.Cleanup(primary.Close)

	writer := &recordingFXWriter{}
	w := (&Worker{
		client:       NewClient("test-key").WithBase(primary.URL),
		cache:        NewCache(),
		writer:       writer,
		logger:       discardLogger(),
		guards:       map[string]*rateGuard{},
		activeSource: fxSource,
	}).WithFallbacks(ECBProvider{Endpoint: ecbServer(t, ecbDailyXML, http.StatusOK).URL})
	w.refreshOnce(context.Background())

	for _, ticker := range []string{"EUR", "GBP", "JPY"} {
		if _, ok := servedRate(t, w.cache, ticker); !ok {
			t.Errorf("%s not served from the ECB standby", ticker)
		}
	}
	if len(writer.batches) != 1 {
		t.Fatalf("persisted %d batches, want 1", len(writer.batches))
	}
	published := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	var current, dated int
	for _, q := range writer.batches[0] {
		want := fxSource
		if q.Bucket.Equal(published) {
			want = "ecb"
			current++
		} else {
			dated++
		}
		if q.Source != want {
			t.Errorf("%s %s row source = %q, want %q", q.Ticker, q.Bucket.Format("2006-01-02"), q.Source, want)
		}
	}
	if current == 0 || dated == 0 {
		t.Fatalf("batch has %d current and %d dated rows; the scenario needs both", current, dated)
	}
}
