package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// staleFXAge is well past the fx-cross staleness budget (default 76h,
// pricing_guard.fx_cross_max_age_hours): the forex worker has stopped refreshing (upstream
// outage past its maxHeldRateAge hold, or the worker wedged) and nothing has pruned the
// cached snapshot.

const staleFXAge = 10 * 24 * time.Hour

// TestPriceUSDAnchoredFiatCrossRejectsStaleRate: a wallet must not be shown a BRL balance
// derived from a forex rate the worker stopped refreshing long ago. tryUSDAnchoredFiatCross
// must check the age of the s.Currencies.Latest() entry before using c.RateUSD, or a wedged
// forex worker keeps serving derived local-currency prices off an arbitrarily old rate.
func TestPriceUSDAnchoredFiatCrossRejectsStaleRate(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	stale := time.Now().UTC().Add(-staleFXAge)
	currencies := &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{
				{Ticker: "BRL", Name: "Brazilian real", RateUSD: brlRateUSD, UpdatedAt: stale},
			},
			PublishedAt: stale,
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a forex rate stale by %s must not be "+
			"derived from as if current. Body: %s", resp.StatusCode, staleFXAge, body)
	}
}

// TestPriceFiatCrossRateRejectsStaleRate applies the same discipline to the fiat-vs-fiat
// path. tryFiatCrossRate stamps observed_at from the older leg's timestamp, so it must also
// bound that timestamp; otherwise a currency unrefreshed for weeks is still served as a
// derived price, merely labelled with its stale observed_at.
func TestPriceFiatCrossRateRejectsStaleRate(t *testing.T) {
	reader := &usdLegReader{}
	stale := time.Now().UTC().Add(-staleFXAge)
	currencies := &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{
				{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: stale},
			},
			PublishedAt: stale,
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=fiat:EUR&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a forex rate stale by %s must not be "+
			"derived from as if current. Body: %s", resp.StatusCode, staleFXAge, body)
	}
}
