package forex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The served in-memory snapshot and the C2-030 sanity band (F004 / F026 /
// K032, audit-2026-09-02).
//
// /v1/price's two fiat paths read the forex Cache directly. The worker
// used to install the snapshot built from the RAW upstream map and only
// afterwards run the band, inside persistSnapshot — so the band protected
// fx_quotes and nothing else. During the 2026-08-24 Massive UZS incident
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
		// The band used to live behind `if w.writer == nil { return }`,
		// so a cache-only worker served every upstream bar unbanded.
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
