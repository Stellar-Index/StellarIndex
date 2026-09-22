package forex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestCurrencyNames_RefusesPaginationOffHost pins T019: a next_url that
// points off the configured Massive host must not be followed, because
// [Client.get] unconditionally attaches the bearer token to whatever
// URL it is given. A malicious or compromised upstream response could
// otherwise redirect CurrencyNames' pagination loop to an attacker host
// and exfiltrate the API key via the Authorization header.
func TestCurrencyNames_RefusesPaginationOffHost(t *testing.T) {
	var attackerHits int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attackerHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer attacker.Close()

	legit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{
				{
					"ticker":               "C:USDEUR",
					"base_currency_symbol": "USD",
					"base_currency_name":   "United States Dollar",
					"currency_symbol":      "EUR",
					"currency_name":        "Euro",
				},
			},
			"next_url": attacker.URL + "/v3/reference/tickers?cursor=evil",
		})
	}))
	defer legit.Close()

	c := NewClient("super-secret-key").WithBase(legit.URL)
	names, err := c.CurrencyNames(context.Background())
	if err != nil {
		t.Fatalf("CurrencyNames returned error: %v", err)
	}
	if got, want := names["eur"], "Euro"; got != want {
		t.Errorf("names[eur] = %q, want %q — first page must still be served", got, want)
	}
	if hits := atomic.LoadInt32(&attackerHits); hits != 0 {
		t.Errorf("attacker host received %d request(s); a cross-host next_url must never be followed (leaks the bearer token)", hits)
	}
}
