package forex

import (
	"context"
	"testing"
	"time"
)

// The rates<->names join and currency-code case.
//
// A `names[code]` join in buildSnapshot is an exact-case
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
