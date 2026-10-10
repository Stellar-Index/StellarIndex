package forex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The served in-memory snapshot and the fx sanity band.
//
// /v1/price's two fiat paths read the forex Cache directly. A worker that
// installs the snapshot built from the RAW upstream map and only
// afterwards runs the band, inside persistSnapshot, protects fx_quotes
// and nothing else. During the Massive UZS incident
// the guard kept 1820 (true level ~11,800) out of fx_quotes all day while
// the cache served it to every fiat:UZS request.
//
// These tests drive the production refresh entry point, [Worker.refreshOnce],
// against a fake of massive's two REST endpoints. None of them calls the
// band helpers directly: the defect was an ORDERING between the band and
// the install, which only the real refresh path can show.

// fakeMassive serves the grouped-daily FX endpoint and the reference
// tickers endpoint. `current` answers the most recent UTC date (what
// LatestUSDRates reads); `history` answers every earlier date. A nil
// `history` means the upstream has no dated bars at all.
type fakeMassive struct {
	mu      sync.Mutex
	current map[string]float64
	history map[string]float64
	names   map[string]string
	// groupedStatus, when non-zero, is returned by the grouped-daily
	// endpoint instead of rates (a quota-limited aggregates product).
	groupedStatus int
	// aggs, when set, answers the per-ticker aggregates endpoint.
	aggs http.HandlerFunc
}

func (f *fakeMassive) setCurrent(rates map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = rates
}

func (f *fakeMassive) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const groupedPrefix = "/v2/aggs/grouped/locale/global/market/fx/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, groupedPrefix):
			if f.groupedStatus != 0 {
				w.WriteHeader(f.groupedStatus)
				return
			}
			rates := f.history
			if strings.TrimPrefix(r.URL.Path, groupedPrefix) == time.Now().UTC().Format("2006-01-02") {
				rates = f.current
			}
			writeGrouped(w, rates)
		case f.aggs != nil && strings.HasPrefix(r.URL.Path, "/v2/aggs/ticker/"):
			f.aggs(w, r)
		case r.URL.Path == "/v3/reference/tickers":
			writeTickers(w, f.names)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeGrouped(w http.ResponseWriter, rates map[string]float64) {
	type row struct {
		T string  `json:"T"`
		C float64 `json:"c"`
	}
	rows := make([]row, 0, len(rates))
	for code, rate := range rates {
		rows = append(rows, row{T: "C:USD" + code, C: rate})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"resultsCount": len(rows), "results": rows})
}

func writeTickers(w http.ResponseWriter, names map[string]string) {
	type row struct {
		Ticker             string `json:"ticker"`
		BaseCurrencySymbol string `json:"base_currency_symbol"`
		BaseCurrencyName   string `json:"base_currency_name"`
		CurrencySymbol     string `json:"currency_symbol"`
		CurrencyName       string `json:"currency_name"`
	}
	rows := make([]row, 0, len(names))
	for code, name := range names {
		rows = append(rows, row{
			Ticker:             "C:USD" + code,
			BaseCurrencySymbol: "USD", BaseCurrencyName: "United States Dollar",
			CurrencySymbol: code, CurrencyName: name,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"results": rows})
}

var guardedNames = map[string]string{
	"UZS": "Uzbekistan Som",
	"EUR": "Euro",
	"EGP": "Egyptian Pound",
}

// newGuardedWorker builds a production-shaped worker (the same fields
// NewWorker sets) against the fake upstream. writer may be nil — the
// cache-only configuration.
func newGuardedWorker(t *testing.T, up *fakeMassive, writer FXQuoteWriter) *Worker {
	t.Helper()
	return &Worker{
		client:       NewClient("test-key").WithBase(up.serve(t).URL),
		cache:        NewCache(),
		writer:       writer,
		logger:       discardLogger(),
		guards:       map[string]*rateGuard{},
		activeSource: fxSource,
	}
}

// servedRate returns the rate the cache currently serves for ticker.
func servedRate(t *testing.T, c *Cache, ticker string) (float64, bool) {
	t.Helper()
	snap := c.Latest()
	if snap == nil {
		t.Fatalf("no snapshot installed")
	}
	for _, cur := range snap.Currencies {
		if cur.Ticker == ticker {
			return cur.RateUSD, true
		}
	}
	return 0, false
}

// recordingFXWriter keeps every batch so a test can compare what was
// SERVED with what was PERSISTED.
type recordingFXWriter struct{ batches [][]FXQuote }

func (r *recordingFXWriter) InsertFXQuoteBatch(_ context.Context, q []FXQuote) error {
	r.batches = append(r.batches, append([]FXQuote(nil), q...))
	return nil
}

// TestRefreshOnce_BandRejectedRateIsNeverInstalled is the UZS incident:
// a healthy refresh, then the current feed starts serving 1820 against a
// true ~11,800 and keeps serving it. The band refuses it (first as a
// deviation, then — because agreeing dated bars refute it — via the
// confirm veto). The cache must serve the last GUARDED rate throughout,
// exactly as fx_quotes does.
func TestRefreshOnce_BandRejectedRateIsNeverInstalled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cacheOnly bool
	}{
		{name: "with fx_quotes writer"},
		// A band behind `if w.writer == nil { return }` would let a
		// cache-only worker serve every upstream bar unbanded.
		{name: "cache-only (nil writer)", cacheOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeMassive{
				current: map[string]float64{"UZS": 11800, "EUR": 0.92},
				history: map[string]float64{"UZS": 11790, "EUR": 0.92},
				names:   guardedNames,
			}
			rec := &recordingFXWriter{}
			var writer FXQuoteWriter = rec
			if tc.cacheOnly {
				writer = nil
			}
			w := newGuardedWorker(t, up, writer)
			ctx := context.Background()

			w.refreshOnce(ctx)
			if got, ok := servedRate(t, w.cache, "UZS"); !ok || got != 11800 {
				t.Fatalf("healthy refresh: served UZS = %v (present=%v), want 11800", got, ok)
			}

			up.setCurrent(map[string]float64{"UZS": 1820, "EUR": 0.921})
			for refresh := 2; refresh <= 4; refresh++ {
				w.refreshOnce(ctx)
				got, ok := servedRate(t, w.cache, "UZS")
				if !ok {
					t.Fatalf("refresh %d: UZS vanished from the served snapshot; a "+
						"band rejection holds the last guarded rate", refresh)
				}
				if got != 11800 {
					t.Fatalf("refresh %d: served UZS = %v, want the last GUARDED rate "+
						"11800 — the band refused 1820 for fx_quotes, so the cache "+
						"must not serve it either", refresh, got)
				}
				// A sibling the band accepted keeps moving: the hold is
				// per ticker, not a frozen snapshot.
				if eur, _ := servedRate(t, w.cache, "EUR"); eur != 0.921 {
					t.Fatalf("refresh %d: served EUR = %v, want the accepted 0.921", refresh, eur)
				}
			}

			for i, batch := range rec.batches {
				for _, q := range batch {
					if q.Ticker == "UZS" && q.RateUSD == 1820 {
						t.Fatalf("batch %d persisted the refused UZS bar — the reorder "+
							"must not have weakened the fx_quotes band", i)
					}
				}
			}
		})
	}
}

// TestRefreshOnce_HealedBootstrapIsRefusedNotServed is the same incident
// at process restart: the FIRST sample is the broken one, the bootstrap
// arm accepts it sight-unseen, and the ticker's own dated bars then refute
// it (the history-majority heal scrubs the row from the fx_quotes batch).
// A scrubbed row is not a guarded rate, so the cache serves NOTHING for
// the ticker — a refusal — rather than the refuted sample.
func TestRefreshOnce_HealedBootstrapIsRefusedNotServed(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"UZS": 1820, "EUR": 0.92},
		history: map[string]float64{"UZS": 11790, "EUR": 0.92},
		names:   guardedNames,
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{})

	w.refreshOnce(context.Background())

	if got, ok := servedRate(t, w.cache, "UZS"); ok {
		t.Fatalf("served UZS = %v after the heal refuted the bootstrap sample; "+
			"want the ticker absent (refusal)", got)
	}
	if eur, ok := servedRate(t, w.cache, "EUR"); !ok || eur != 0.92 {
		t.Fatalf("served EUR = %v (present=%v), want 0.92 — one refuted ticker "+
			"must not cost the rest of the snapshot", eur, ok)
	}

	// The current feed recovers: the healed baseline accepts it and the
	// ticker comes back.
	up.setCurrent(map[string]float64{"UZS": 11810, "EUR": 0.92})
	w.refreshOnce(context.Background())
	if got, ok := servedRate(t, w.cache, "UZS"); !ok || got != 11810 {
		t.Fatalf("after recovery: served UZS = %v (present=%v), want 11810", got, ok)
	}
}

// TestRefreshOnce_ConfirmedMoveReachesTheCache pins the other half of the
// two-strike band: a REAL devaluation the upstream keeps reporting is
// held for one refresh and then served. The reorder must not turn the
// band into a permanent freeze.
func TestRefreshOnce_ConfirmedMoveReachesTheCache(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"EGP": 30, "EUR": 0.92},
		names:   guardedNames, // no dated bars: nothing to veto the confirm
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{})
	ctx := context.Background()

	w.refreshOnce(ctx)
	up.setCurrent(map[string]float64{"EGP": 50, "EUR": 0.92})

	w.refreshOnce(ctx)
	if got, _ := servedRate(t, w.cache, "EGP"); got != 30 {
		t.Fatalf("first sighting of a 67%% move: served EGP = %v, want the held 30", got)
	}
	w.refreshOnce(ctx)
	if got, _ := servedRate(t, w.cache, "EGP"); got != 50 {
		t.Fatalf("second agreeing fetch: served EGP = %v, want the confirmed 50", got)
	}
}

// TestRefreshOnce_HeldRateExpires bounds the hold. A rate nothing has
// re-confirmed is served with its ORIGINAL UpdateAt, and only for as long
// as the fx_quotes read path would serve its last-good row (the 7-day
// forex-snap lookback). Past that it is dropped: fail closed, never an
// indefinitely stale money value.
func TestRefreshOnce_HeldRateExpires(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"EUR": 0.92},
		names:   guardedNames,
	}
	w := newGuardedWorker(t, up, nil)
	now := time.Now().UTC()
	recent, expired := now.Add(-48*time.Hour), now.Add(-8*24*time.Hour)
	w.cache.Set(&Snapshot{
		Currencies: []Currency{
			{Ticker: "EGP", Name: "Egyptian Pound", RateUSD: 30, UpdateAt: recent},
			{Ticker: "UZS", Name: "Uzbekistan Som", RateUSD: 11800, UpdateAt: expired},
		},
		PublishedAt: recent,
		FetchedAt:   recent,
	})

	w.refreshOnce(context.Background())

	snap := w.cache.Latest()
	var egp *Currency
	for i := range snap.Currencies {
		switch snap.Currencies[i].Ticker {
		case "EGP":
			egp = &snap.Currencies[i]
		case "UZS":
			t.Fatalf("UZS held past the 7-day bound: %+v", snap.Currencies[i])
		}
	}
	if egp == nil {
		t.Fatalf("EGP (absent from this refresh, last guarded 48h ago) was dropped; "+
			"want it held. Snapshot: %+v", snap.Currencies)
	}
	if egp.RateUSD != 30 || !egp.UpdateAt.Equal(recent) {
		t.Fatalf("held EGP = %+v, want rate 30 with its ORIGINAL UpdateAt %v — a hold "+
			"must never be re-stamped as fresh", *egp, recent)
	}
}

// TestRefreshOnce_ECBFallbackJoinsAgainstPrimaryNames is the finding's
// scenario: massive's grouped-aggregates product is quota-limited (429)
// while its reference endpoint still answers, so the refresh pairs ECB's
// UPPER-case rates with massive's lower-case names.
func TestRefreshOnce_ECBFallbackJoinsAgainstPrimaryNames(t *testing.T) {
	up := &fakeMassive{
		groupedStatus: http.StatusTooManyRequests,
		names: map[string]string{
			"GBP": "British Pound", "JPY": "Japanese Yen", "EUR": "Euro",
		},
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{})
	w.fallbacks = []RateProvider{ECBProvider{Endpoint: ecbServer(t, ecbDailyXML, http.StatusOK).URL}}

	w.refreshOnce(context.Background())

	snap := w.cache.Latest()
	if snap == nil {
		t.Fatalf("no snapshot installed from the ECB standby")
	}
	if len(snap.Currencies) == 1 {
		t.Fatalf("snapshot collapsed to the synthetic USD row: ECB served GBP/JPY/EUR "+
			"but the case-sensitive join matched none of them. Got %+v", snap.Currencies)
	}
	// ecbDailyXML: 1 EUR = 1.25 USD = 0.85 GBP = 160 JPY.
	for ticker, want := range map[string]float64{
		"USD": 1, "EUR": 0.8, "GBP": 0.68, "JPY": 128,
	} {
		got, ok := servedRate(t, w.cache, ticker)
		if !ok || !closeTo(got, want) {
			t.Errorf("served %s = %v (present=%v), want %v", ticker, got, ok, want)
		}
	}
	if got := w.sourceLabel(); got != "ecb" {
		t.Errorf("source label = %q, want ecb", got)
	}
}

// TestRefreshOnce_ReusedNamesJoinAgainstPrimaryRates is the second
// trigger: rates are healthy, the NAMES endpoint fails, and the worker
// reuses the last snapshot's names — which it re-keys by the UPPER-case
// Ticker — against the primary's lower-case rates.
//
// The assertion is on the NEW rate, not on presence: since the served
// snapshot holds a ticker's last guarded rate, a collapsed join no longer
// makes EUR vanish — it silently pins it at the previous refresh's value.
func TestRefreshOnce_ReusedNamesJoinAgainstPrimaryRates(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"EUR": 0.92, "UZS": 11800},
		history: map[string]float64{"EUR": 0.92, "UZS": 11790},
		names:   guardedNames,
	}
	w := newGuardedWorker(t, up, &recordingFXWriter{})
	ctx := context.Background()
	w.refreshOnce(ctx)

	up.mu.Lock()
	up.names = nil // writeTickers emits an empty list -> CurrencyNames errors
	up.current = map[string]float64{"EUR": 0.93, "UZS": 11850}
	up.mu.Unlock()
	w.refreshOnce(ctx)

	for ticker, want := range map[string]float64{"EUR": 0.93, "UZS": 11850} {
		got, ok := servedRate(t, w.cache, ticker)
		if !ok || got != want {
			t.Errorf("served %s = %v (present=%v), want this refresh's %v — the reused "+
				"(UPPER-keyed) names must still join the primary's lower-case rates",
				ticker, got, ok, want)
		}
	}
	snap := w.cache.Latest()
	for _, c := range snap.Currencies {
		if c.Ticker == "EUR" && c.Name != "Euro" {
			t.Errorf("EUR name = %q, want the reused %q", c.Name, "Euro")
		}
	}
}

// TestRefreshOnce_ColdStartWithoutNamesStillInstallsSnapshot is the
// cold-start arm of a primary outage: the process has never installed a
// snapshot, the rates fetch succeeds (here directly; in production via
// the ECB fallback) and the primary's names endpoint errors. Names are
// static display labels, so the refresh must still install the rates —
// labelled by ticker — rather than leave the feed empty until the
// primary returns. The warm path already reuses cached names; this
// pins the one-time cold path, which must not return before cache.Set.
func TestRefreshOnce_ColdStartWithoutNamesStillInstallsSnapshot(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"EUR": 0.92, "UZS": 11800},
		history: map[string]float64{"EUR": 0.92, "UZS": 11790},
		names:   nil, // writeTickers emits an empty list -> CurrencyNames errors
	}
	writer := &recordingFXWriter{}
	w := newGuardedWorker(t, up, writer)
	w.refreshOnce(context.Background())

	if w.cache.Latest() == nil {
		t.Fatal("no snapshot installed: a cold start with the names endpoint down left the feed empty")
	}
	for ticker, want := range map[string]float64{"EUR": 0.92, "UZS": 11800} {
		got, ok := servedRate(t, w.cache, ticker)
		if !ok || got != want {
			t.Errorf("served %s = %v (present=%v), want %v — rates in hand must be served even without names",
				ticker, got, ok, want)
		}
	}
	if len(writer.batches) == 0 {
		t.Error("nothing persisted to fx_quotes on the cold-start path")
	}
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

// The served snapshot names the feed behind each rate: what the standby
// answered says ecb, and a ticker it does not carry is held with the
// primary's name, so /v1/price can credit the feed that actually priced it.
func TestRefreshOnce_ServedCurrenciesCarryTheirPublishingFeed(t *testing.T) {
	w := newTestWorker(t).WithFallbacks(ECBProvider{Endpoint: ecbServer(t, ecbDailyXML, http.StatusOK).URL})
	held := time.Now().UTC().Add(-time.Hour)
	w.cache.Set(&Snapshot{Currencies: []Currency{
		{Ticker: "EUR", Name: "Euro", RateUSD: 0.8, UpdateAt: held, Source: fxSource},
		{Ticker: "NGN", Name: "Nigerian Naira", RateUSD: 1500, UpdateAt: held, Source: fxSource},
	}})
	w.refreshOnce(context.Background())

	want := map[string]string{"EUR": "ecb", "NGN": fxSource}
	got := map[string]string{}
	for _, c := range w.cache.Latest().Currencies {
		got[c.Ticker] = c.Source
	}
	for ticker, source := range want {
		if got[ticker] != source {
			t.Errorf("%s source = %q, want %q (served: %v)", ticker, got[ticker], source, got)
		}
	}
}

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
