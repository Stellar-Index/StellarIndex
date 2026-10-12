package forex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// TestGet_TruncatedBodyFails pins that a body cut short of its declared
// Content-Length is an error, not a short success: only io.EOF ends a read cleanly.
func TestGet_TruncatedBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"results":[`))
	}))
	defer srv.Close()

	body, err := NewClient("test-key").get(context.Background(), srv.URL)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("get(truncated) = %q, %v; want io.ErrUnexpectedEOF", body, err)
	}
}

func TestGet_OversizeBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 16<<20+1))
	}))
	defer srv.Close()

	if _, err := NewClient("test-key").get(context.Background(), srv.URL); err == nil {
		t.Fatal("get(16 MiB + 1) succeeded; want size-cap error")
	}
}

// TestCurrencyNames_RefusesPaginationOffHost pins that a next_url off the configured Massive host is not followed:
// [Client.get] sends the bearer token to any URL, so a compromised upstream could exfiltrate the API key.
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

// TestIsFiniteFloat covers the large-magnitude case that the old
// `f != f+1` inf check got wrong: for |f| >= 2^53, f+1 rounds back
// to f, so a perfectly finite value reads as non-finite.
func TestIsFiniteFloat(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want bool
	}{
		{"zero", 0, true},
		{"small", 1.2345, true},
		{"negative", -42.5, true},
		{"large_positive_above_2e53", 1e300, true},
		{"exactly_2e53", math.Pow(2, 53), true},
		{"max_float64", math.MaxFloat64, true},
		{"nan", math.NaN(), false},
		{"pos_inf", math.Inf(1), false},
		{"neg_inf", math.Inf(-1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFiniteFloat(tc.in); got != tc.want {
				t.Errorf("isFiniteFloat(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestClient_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	c := NewClient("probe").WithBase(trap.URL)
	if _, _, err := c.LatestUSDRates(context.Background()); err == nil {
		t.Error("LatestUSDRates succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
