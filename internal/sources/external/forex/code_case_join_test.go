package forex

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The rates<->names join and currency-code case (F033, audit-2026-09-02).
//
// buildSnapshot joined the two maps with `names[code]` — an exact-case
// lookup between maps whose producers never agreed on a case:
//
//	primary rates   lower  (client.go lower-cases C:USDEUR -> "eur")
//	primary names   lower  (client.go lower-cases both symbols)
//	ECB rates       UPPER  (ecb/rates.go keys on the XML attribute)
//	reused names    UPPER  (refreshOnce rebuilds them from Currency.Ticker)
//
// Any refresh that mixed a lower map with an UPPER one matched nothing,
// and the snapshot collapsed to the one row buildSnapshot always adds:
// a synthetic USD. Both mixes are the DEGRADED paths — the ones that
// exist so an outage costs coverage rather than the whole feed.
//
// Every test here goes through [Worker.refreshOnce] with the real ECB
// provider or the real client, so the case each producer ACTUALLY emits
// is what gets joined.

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

// TestFetchHistory_JoinsNamesInAnyCase covers the same join's second
// site: fetchHistory drops every dated bar whose code is not in `names`,
// so UPPER-keyed (reused) names silently emptied the trailing-7d series —
// and with it the evidence the history-majority heal and the confirm veto
// run on.
func TestFetchHistory_JoinsNamesInAnyCase(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"UZS": 11800},
		history: map[string]float64{"UZS": 11790},
		names:   guardedNames,
	}
	w := newGuardedWorker(t, up, nil)

	upperKeyed := map[string]string{"UZS": "Uzbekistan Som"}
	got := w.fetchHistory(context.Background(), upperKeyed, time.Now().UTC())

	if len(got["UZS"]) != 7 {
		t.Fatalf("UZS history = %d bars, want 7 — the lower-case dated rates must "+
			"join names keyed in any case. Got %+v", len(got["UZS"]), got)
	}
}

// TestBuildSnapshot_MixedCaseCodesCollapseToOneTicker pins determinism
// when one map carries the same currency in two cases: exactly one row,
// never two and never a map-iteration-order coin flip.
func TestBuildSnapshot_MixedCaseCodesCollapseToOneTicker(t *testing.T) {
	now := time.Now().UTC()
	for i := 0; i < 50; i++ {
		snap := buildSnapshot(
			map[string]float64{"EUR": 0.8, "eur": 0.9, "Gbp": 0.68},
			map[string]string{"eur": "Euro", "GBP": "British Pound"},
			now, now, nil, nil,
		)
		if len(snap.Currencies) != 3 {
			t.Fatalf("want exactly EUR, GBP, USD; got %+v", snap.Currencies)
		}
		if snap.Currencies[0].Ticker != "EUR" || snap.Currencies[0].RateUSD != 0.8 {
			t.Fatalf("EUR row = %+v, want the deterministic first-in-sorted-order 0.8",
				snap.Currencies[0])
		}
	}
}
