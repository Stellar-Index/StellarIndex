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
	published := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	heldSince := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
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
	if !strings.Contains(body, `"observed_at":"2026-09-16T00:00:00Z"`) {
		t.Errorf("observed_at must be the HELD leg's own timestamp (2026-09-16), "+
			"not the snapshot's publication time: %s", body)
	}

	// Both legs fresh: unchanged behaviour, the publication time.
	resp = mustGet(t, ts.URL+"/v1/price?asset=fiat:EUR&quote=fiat:USD")
	body, _ = readAll(resp)
	if !strings.Contains(body, `"observed_at":"2026-09-19T00:00:00Z"`) {
		t.Errorf("fresh legs must keep the snapshot publication time: %s", body)
	}
}
