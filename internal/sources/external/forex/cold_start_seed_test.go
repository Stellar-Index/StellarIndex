package forex

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeFXReader returns rows regardless of since, so the worker's own
// maxHeldRateAge check is what a too-old row has to get past.
type fakeFXReader struct {
	rows  []FXQuote
	err   error
	calls int
	since time.Time
}

func (f *fakeFXReader) LatestFXQuotes(_ context.Context, since time.Time) ([]FXQuote, error) {
	f.calls++
	f.since = since
	return f.rows, f.err
}

var seedNames = map[string]string{
	"UZS": "Uzbekistan Som",
	"EUR": "Euro",
	"AED": "United Arab Emirates Dirham",
	"CNY": "Chinese Yuan",
}

func servedCurrency(t *testing.T, c *Cache, ticker string) (Currency, bool) {
	t.Helper()
	for _, cur := range c.Latest().Currencies {
		if cur.Ticker == ticker {
			return cur, true
		}
	}
	return Currency{}, false
}

// A restart must keep serving a ticker the upstream has not republished
// yet today, from its last guarded fx_quotes row.
func TestRefreshOnce_ColdStartSeedsHeldRatesFromFXQuotes(t *testing.T) {
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	stale := time.Now().UTC().Add(-maxHeldRateAge - 24*time.Hour)
	reader := &fakeFXReader{rows: []FXQuote{
		{Bucket: yesterday, Ticker: "AED", RateUSD: 3.6725, Source: "massive"},
		{Bucket: stale, Ticker: "CNY", RateUSD: 7.1, Source: "massive"},
		{Bucket: yesterday, Ticker: "EUR", RateUSD: 0.5, Source: "massive"},
	}}
	up := &fakeMassive{
		current: map[string]float64{"EUR": 0.92},
		history: map[string]float64{"EUR": 0.92},
		names:   seedNames,
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{}).WithReader(reader)

	w.refreshOnce(context.Background())

	aed, ok := servedCurrency(t, w.cache, "AED")
	if !ok {
		t.Fatalf("AED absent from today's payload was dropped; want the seeded fx_quotes row held")
	}
	if aed.RateUSD != 3.6725 || aed.Source != "massive" || !aed.UpdateAt.Equal(yesterday) {
		t.Fatalf("seeded AED = %+v, want rate 3.6725, source massive, UpdateAt %v", aed, yesterday)
	}
	if aed.Name != "United Arab Emirates Dirham" {
		t.Fatalf("seeded AED name = %q, want the upstream display name", aed.Name)
	}
	if cny, ok := servedCurrency(t, w.cache, "CNY"); ok {
		t.Fatalf("served CNY %+v from a row older than maxHeldRateAge; want it refused", cny)
	}
	if eur, _ := servedCurrency(t, w.cache, "EUR"); eur.RateUSD != 0.92 {
		t.Fatalf("served EUR = %v, want today's accepted 0.92 over the seeded 0.5", eur.RateUSD)
	}
	if want := w.cache.Latest().FetchedAt.Add(-maxHeldRateAge); !reader.since.Equal(want) {
		t.Fatalf("reader since = %v, want %v", reader.since, want)
	}

	w.refreshOnce(context.Background())
	if reader.calls != 1 {
		t.Fatalf("reader called %d times, want once: only a cold start seeds", reader.calls)
	}
	if _, ok := servedCurrency(t, w.cache, "AED"); !ok {
		t.Fatalf("seeded AED not carried by the next refresh's hold")
	}
}

// A seed must not resurrect the sample the history-majority heal refuted.
func TestRefreshOnce_ColdStartSeedSkipsRefutedTicker(t *testing.T) {
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	up := &fakeMassive{
		current: map[string]float64{"UZS": 1820, "EUR": 0.92},
		history: map[string]float64{"UZS": 11790, "EUR": 0.92},
		names:   seedNames,
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{}).WithReader(&fakeFXReader{rows: []FXQuote{
		{Bucket: yesterday, Ticker: "UZS", RateUSD: 1820, Source: "massive"},
	}})

	w.refreshOnce(context.Background())

	if uzs, ok := servedCurrency(t, w.cache, "UZS"); ok {
		t.Fatalf("served UZS %+v after the heal refuted it; want absent", uzs)
	}
}

func TestRefreshOnce_ColdStartWithoutUsableReaderHoldsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader FXQuoteReader
	}{
		{name: "nil reader"},
		{name: "reader error", reader: &fakeFXReader{err: errors.New("db down")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeMassive{
				current: map[string]float64{"EUR": 0.92},
				history: map[string]float64{"EUR": 0.92},
				names:   seedNames,
			}
			w := newGuardedWorker(t, up, nil).WithReader(tc.reader)

			w.refreshOnce(context.Background())

			if _, ok := servedCurrency(t, w.cache, "AED"); ok {
				t.Fatalf("served AED with no seed source")
			}
			if eur, _ := servedCurrency(t, w.cache, "EUR"); eur.RateUSD != 0.92 {
				t.Fatalf("served EUR = %v, want 0.92", eur.RateUSD)
			}
		})
	}
}
