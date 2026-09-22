package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestPriceFiatCrossStampsTheOlderLeg pins the honesty of observed_at on
// the fiat-vs-fiat path once the forex worker can HOLD a rate (F004 /
// F026 / K032): when the sanity band refuses the upstream's new UZS bar,
// the snapshot keeps the last guarded UZS rate with its ORIGINAL
// per-currency timestamp, under a snapshot PublishedAt that has moved
// on. Stamping the cross with PublishedAt would present a days-old rate
// as today's — the "no stale signal" half of the finding.
func TestPriceFiatCrossStampsTheOlderLeg(t *testing.T) {
	// Relative to "now", not fixed calendar dates: the T650 staleness
	// gate refuses a snapshot leg older than its configured budget
	// (default 76h), and a fixed past timestamp would eventually cross
	// that budget regardless of what this test exercises. The held leg
	// is deliberately kept well inside the budget — this test pins the
	// OLDER-LEG-WINS disclosure, not the separate reject-when-stale
	// behaviour (see TestPriceFiatCrossRefusesAStaleRate).
	published := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	heldSince := published.Add(-10 * time.Hour)
	currencies := &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies: []v1.CurrencyEntry{
			{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: published},
			{Ticker: "UZS", Name: "Uzbekistan Som", RateUSD: 11800, UpdatedAt: heldSince},
		},
		PublishedAt: published,
	}}
	srv := v1.New(v1.Options{Prices: &usdLegReader{}, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=fiat:UZS&quote=fiat:EUR")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"observed_at":"`+heldSince.Format(time.RFC3339)+`"`) {
		t.Errorf("observed_at must be the HELD leg's own timestamp (%s), "+
			"not the snapshot's publication time: %s", heldSince.Format(time.RFC3339), body)
	}

	// Both legs fresh: unchanged behaviour, the publication time.
	resp = mustGet(t, ts.URL+"/v1/price?asset=fiat:EUR&quote=fiat:USD")
	body, _ = readAll(resp)
	if !strings.Contains(body, `"observed_at":"`+published.Format(time.RFC3339)+`"`) {
		t.Errorf("fresh legs must keep the snapshot publication time: %s", body)
	}
}

// TestPriceFiatCrossRefusesAStaleRate is the T650 regression on
// [Server.tryFiatCrossRate]: [forex.Cache.Latest] never expires a
// snapshot on its own, so with no bound a stalled forex worker would
// keep answering forever from its last good fetch — stamped with an
// observed_at that looks current relative to itself, but is in fact
// far older than any live rate. Both currencies here are older than
// the default 76h serving budget.
func TestPriceFiatCrossRefusesAStaleRate(t *testing.T) {
	stale := time.Now().UTC().Add(-100 * time.Hour)
	currencies := &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies: []v1.CurrencyEntry{
			{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: stale},
		},
		PublishedAt: stale,
	}}
	srv := v1.New(v1.Options{Prices: &usdLegReader{}, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=fiat:EUR&quote=fiat:USD")
	if resp.StatusCode == http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("a 100h-old FX rate was served as a live cross-rate — the forex "+
			"Cache never expires on its own, so a stalled worker would answer "+
			"forever with a stale rate. Body: %s", body)
	}
}

// TestPriceUSDAnchoredCrossRefusesAStaleRate is the T650 regression on
// [Server.tryUSDAnchoredFiatCross]: same gap, the USD-anchored path
// (ADR-0051). The USD leg itself is fresh — only the FX rate that
// crosses it into BRL is stale — proving the gate looks at the FX
// leg's OWN freshness, not the derived price's.
func TestPriceUSDAnchoredCrossRefusesAStaleRate(t *testing.T) {
	stale := time.Now().UTC().Add(-100 * time.Hour)
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	currencies := &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies: []v1.CurrencyEntry{
			{Ticker: "BRL", Name: "Brazilian real", RateUSD: brlRateUSD, UpdatedAt: stale},
		},
		PublishedAt: stale,
	}}
	srv := v1.New(v1.Options{Prices: reader, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if resp.StatusCode == http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("a 100h-old FX rate was served as a live USD-anchored cross — "+
			"the fresh USD leg's own observed_at was hiding the stale FX "+
			"component. Body: %s", body)
	}
}
