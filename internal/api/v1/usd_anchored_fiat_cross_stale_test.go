package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestPriceUSDAnchoredFiatCrossRejectsStaleTickerEvenWithFreshSnapshot —
// T670. The snapshot's own PublishedAt is fresh here; only the BRL
// ticker's own UpdatedAt is 77h stale. Proves [Server.fxCrossMaxAge]
// gates on the OLDER of the two (the same held-rate rule
// [tryFiatCrossRate] applies), not just the snapshot's publication
// time — a wedged upstream ticker held past the budget must not hide
// behind an otherwise-current snapshot.
func TestPriceUSDAnchoredFiatCrossRejectsStaleTickerEvenWithFreshSnapshot(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	staleAt := time.Now().UTC().Add(-77 * time.Hour)
	currencies := &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{
				{Ticker: "BRL", Name: "Brazilian real", RateUSD: brlRateUSD, UpdatedAt: staleAt},
			},
			PublishedAt: time.Now().UTC(),
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 404 — the BRL rate is 77h old (past the 76h "+
			"budget), so the layer must decline to derive rather than compose a "+
			"price off a rate the forex worker may have abandoned. Body: %s",
			resp.StatusCode, body)
	}
}
