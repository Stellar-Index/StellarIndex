package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// staleFXAge is comfortably past the fx-cross staleness budget
// (default 76h, pricing_guard.fx_cross_max_age_hours) — the forex
// worker has stopped refreshing (upstream outage past its own
// maxHeldRateAge hold, or the worker itself wedged) and nothing has
// pruned the cached snapshot since.

const staleFXAge = 10 * 24 * time.Hour

// TestPriceUSDAnchoredFiatCrossRejectsStaleRate — a wallet must not be
// shown a BRL balance derived from a forex rate the worker stopped
// refreshing over a week ago. tryUSDAnchoredFiatCross must check the
// age of the s.Currencies.Latest() entry before using c.RateUSD; with no
// check a wedged forex worker keeps silently
// serving derived local-currency prices off an arbitrarily old rate
// forever.
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

// TestPriceFiatCrossRateRejectsStaleRate — same discipline on the
// fiat-vs-fiat cross-rate path. tryFiatCrossRate stamps observed_at from
// the older leg's own timestamp, so it must also check that timestamp
// against a bound; otherwise a currency the worker
// hadn't refreshed in weeks is still served as a fresh derived price
// (merely honestly labelled with its stale observed_at).
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
