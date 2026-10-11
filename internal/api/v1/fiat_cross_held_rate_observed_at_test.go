package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestPriceFiatCrossStampsTheOlderLeg pins observed_at honesty on the fiat-vs-fiat path when the
// forex worker holds a rate: a held UZS rate keeps its ORIGINAL per-currency timestamp under a
// newer snapshot PublishedAt, and stamping the cross with PublishedAt would present it as fresh.
func TestPriceFiatCrossStampsTheOlderLeg(t *testing.T) {
	// Relative to now: a fixed past timestamp would eventually cross the staleness budget
	// (default 76h). The held leg stays well inside it, since this pins OLDER-LEG-WINS, not
	// reject-when-stale (see TestPriceFiatCrossRefusesAStaleRate).
	published := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	heldSince := published.Add(-10 * time.Hour)
	currencies := &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies: []v1.CurrencyEntry{
			{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: published},
			{Ticker: "UZS", Name: "Uzbekistan Som", RateUSD: 11800, UpdatedAt: heldSince},
		},
		PublishedAt: published,
	}}
	// The closed surface binds fixings: UZS's newest bar is the held one.
	fixings := fixingsOf(hourlyFixing("EUR", "0.92", published), hourlyFixing("UZS", "11800", heldSince))
	srv := v1.New(v1.Options{Prices: &usdLegReader{}, Currencies: currencies, FXFixings: fixings})
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

// TestPriceFiatCrossRefusesAStaleRate guards [Server.tryFiatCrossRate]: [forex.Cache.Latest] never
// expires a snapshot, so without a bound a stalled forex worker would answer forever from its last
// fetch. Both currencies here are older than the default 76h serving budget.
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

// TestPriceUSDAnchoredCrossRefusesAStaleRate is the same guard on [Server.tryUSDAnchoredFiatCross]
// (ADR-0051). The USD leg is fresh and only the BRL FX rate is stale, so the gate must look at the
// FX leg's own freshness, not the derived price's.
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
