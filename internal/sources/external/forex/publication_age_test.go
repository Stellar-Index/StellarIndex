package forex

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// datedProvider is a standby whose publication day the test sets.
type datedProvider struct {
	name        string
	rates       map[string]float64
	publishedAt time.Time
	calls       int
}

func (d *datedProvider) Name() string { return d.name }
func (d *datedProvider) LatestUSDRates(context.Context) (map[string]float64, time.Time, error) {
	d.calls++
	return d.rates, d.publishedAt, nil
}

func utcDate(y int, m time.Month, d, hour int) time.Time {
	return time.Date(y, m, d, hour, 0, 0, 0, time.UTC)
}

func TestPublicationStale(t *testing.T) {
	fri := utcDate(2026, 11, 13, 0)
	if fri.Weekday() != time.Friday {
		t.Fatalf("fixture day is a %s, want Friday", fri.Weekday())
	}
	for _, tc := range []struct {
		name  string
		pub   time.Time
		now   time.Time
		stale bool
	}{
		{"same day", fri, utcDate(2026, 11, 13, 23), false},
		{"friday bar on saturday", fri, utcDate(2026, 11, 14, 12), false},
		{"friday bar on sunday", fri, utcDate(2026, 11, 15, 12), false},
		{"friday bar on monday", fri, utcDate(2026, 11, 16, 23), false},
		{"friday bar on tuesday", fri, utcDate(2026, 11, 17, 0), true},
		{"monday bar on tuesday", utcDate(2026, 11, 16, 0), utcDate(2026, 11, 17, 9), false},
		{"monday bar on wednesday", utcDate(2026, 11, 16, 0), utcDate(2026, 11, 18, 0), true},
		{"sunday bar on tuesday", utcDate(2026, 11, 15, 0), utcDate(2026, 11, 17, 0), true},
		{"publication ahead of the clock", utcDate(2026, 11, 18, 0), utcDate(2026, 11, 17, 0), false},
		{"no publication", time.Time{}, utcDate(2026, 11, 17, 0), true},
		{"thursday bar the day after a friday christmas", utcDate(2026, 12, 24, 0), utcDate(2026, 12, 26, 12), false},
		{"thursday bar the monday after a friday christmas", utcDate(2026, 12, 24, 0), utcDate(2026, 12, 28, 12), false},
		{"thursday bar the tuesday after a friday christmas", utcDate(2026, 12, 24, 0), utcDate(2026, 12, 29, 0), true},
		{"new year's eve bar on the second", utcDate(2026, 12, 31, 0), utcDate(2027, 1, 2, 12), false},
	} {
		if got := publicationStale(tc.pub, tc.now); got != tc.stale {
			t.Errorf("%s: publicationStale = %v, want %v", tc.name, got, tc.stale)
		}
	}
}

// stalePrimary is a primary that publishes nothing for today and answers the walk-back with an older bar, and a worker whose clock
// is far enough ahead that the bar has missed a weekday session.
func stalePrimary(t *testing.T, writer FXQuoteWriter) *Worker {
	t.Helper()
	up := &fakeMassive{names: guardedNames, history: map[string]float64{"EUR": 0.92}}
	w := newGuardedWorker(t, up, writer)
	ahead := time.Now().Add(4 * 24 * time.Hour)
	w.now = func() time.Time { return ahead }
	return w
}

func TestFetchRates_StalePrimaryHandsOffToANewerStandby(t *testing.T) {
	w := stalePrimary(t, nil)
	fb := &datedProvider{name: "ecb", rates: map[string]float64{"EUR": 0.8}, publishedAt: w.clock().UTC().Truncate(24 * time.Hour)}
	w.fallbacks = []RateProvider{fb}

	rates, publishedAt, source, err := w.fetchRates(context.Background())
	if err != nil {
		t.Fatalf("fetchRates: %v", err)
	}
	if source != "ecb" || !closeTo(rates["EUR"], 0.8) || !publishedAt.Equal(fb.publishedAt) {
		t.Fatalf("got source %q EUR %v published %v, want the newer standby", source, rates["EUR"], publishedAt)
	}
	// The vendor answered, so its hourly bars are still fetched for what it lists.
	if got := namedTickers(w.stalePrimaryCodes, guardedNames); len(got) != 1 || got[0] != "EUR" {
		t.Errorf("fixing tickers behind the standby = %v, want [EUR]", got)
	}
}

// A standby that answered last refresh and fails this one must not move the served rates back to the primary's older bar.
func TestFetchRates_StalePrimaryDoesNotReplaceANewerServedSnapshot(t *testing.T) {
	w := stalePrimary(t, nil)
	w.cache.Set(&Snapshot{PublishedAt: w.clock().UTC().Truncate(24 * time.Hour)})
	w.fallbacks = []RateProvider{&stubProvider{name: "ecb", err: errors.New("standby down")}}

	if rates, _, source, err := w.fetchRates(context.Background()); err == nil {
		t.Fatalf("got source %q rates %v, want an error so the cache keeps the newer snapshot", source, rates)
	}
}

func TestFetchRates_StalePrimaryIsServedWhenNoStandbyIsNewer(t *testing.T) {
	w := stalePrimary(t, nil)
	fb := &datedProvider{name: "ecb", rates: map[string]float64{"EUR": 0.8}, publishedAt: time.Now().UTC().AddDate(0, 0, -3)}
	w.fallbacks = []RateProvider{fb}

	rates, _, source, err := w.fetchRates(context.Background())
	if err != nil {
		t.Fatalf("fetchRates: %v", err)
	}
	if fb.calls != 1 {
		t.Errorf("standby calls = %d, want it consulted once", fb.calls)
	}
	if source != fxSource || !closeTo(rates["eur"], 0.92) {
		t.Fatalf("got source %q rates %v, want the primary's older bar kept", source, rates)
	}
}

func TestFetchRates_CurrentPrimaryDoesNotConsultTheStandby(t *testing.T) {
	up := &fakeMassive{names: guardedNames, current: map[string]float64{"EUR": 0.92}}
	w := newGuardedWorker(t, up, nil)
	fb := &datedProvider{name: "ecb", rates: map[string]float64{"EUR": 0.8}, publishedAt: time.Now().UTC()}
	w.fallbacks = []RateProvider{fb}

	if _, _, source, err := w.fetchRates(context.Background()); err != nil || source != fxSource {
		t.Fatalf("source %q err %v, want the primary", source, err)
	}
	if fb.calls != 0 {
		t.Errorf("standby calls = %d, want 0", fb.calls)
	}
}

// A feed that keeps re-serving an old bar commits rows every cycle; the liveness gauge must not advance on them.
func TestRefreshOnce_StalePublicationDoesNotStampLiveness(t *testing.T) {
	cw := &captureWriter{}
	w := stalePrimary(t, cw)
	gauge := obs.ExternalFXLastQuoteUnix.WithLabelValues(fxSource)
	gauge.Set(1)

	for range 3 {
		w.refreshOnce(context.Background())
	}

	if len(cw.batches) != 3 || rateFor(cw.batches[2], "EUR") != 0.92 {
		t.Fatalf("batches = %+v, want the old bar still committed each cycle", cw.batches)
	}
	if got := fxGauge(); got != 1 {
		t.Fatalf("liveness gauge = %v, want it left at 1 while the publication is stale", got)
	}
}

// A series held across a day roll still ends in the bar fetched while that day was open. Writing it would replace the day's last
// current rate with its first.
func TestPersistSnapshot_HeldSeriesDoesNotRewriteTheDayItWasFetchedOn(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	fetchedOn := utcDate(2026, 11, 11, 0)
	w.rawHistoryFetchedAt = fetchedOn.Add(30 * time.Minute)
	snap := &Snapshot{
		PublishedAt: fetchedOn.AddDate(0, 0, 1),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 0.92}},
		History7d: map[string][]HistoryPoint{"EUR": {
			{Date: fetchedOn.AddDate(0, 0, -1), RateUSD: 0.91},
			{Date: fetchedOn, RateUSD: 0.915},
		}},
	}

	w.persistSnapshot(context.Background(), snap)

	batch := cw.batches[0]
	if !hasHistoryRow(batch, "EUR", fetchedOn.AddDate(0, 0, -1), 0.91) {
		t.Errorf("batch %+v lacks the closed bar", batch)
	}
	if got := currentDayRate(batch, "EUR", fetchedOn); got != -1 {
		t.Errorf("batch rewrites %s with %v, the bar fetched while that day was open", fetchedOn.Format("2006-01-02"), got)
	}
	if got := currentDayRate(batch, "EUR", snap.PublishedAt); got != 0.92 {
		t.Errorf("current row = %v, want 0.92", got)
	}
}
