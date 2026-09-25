package external

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func mustFiat(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%q): %v", code, err)
	}
	return a
}

func mustCrypto(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%q): %v", code, err)
	}
	return a
}

func mustPair(t *testing.T, base, quote canonical.Asset) canonical.Pair {
	t.Helper()
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// FiatCodesFromPairs is the shared FX interest-set derivation: fiat
// codes from EITHER side of any pair, uppercased, minus the venue's
// own base. All three FX pollers ride on it, so pin the semantics.
func TestFiatCodesFromPairs(t *testing.T) {
	xlm := mustCrypto(t, "XLM")
	usd := mustFiat(t, "USD")
	eur := mustFiat(t, "EUR")
	gbp := mustFiat(t, "GBP")

	pairs := []canonical.Pair{
		mustPair(t, xlm, eur), // crypto base skipped, EUR excluded (base)
		mustPair(t, usd, eur), // USD in via base side
		mustPair(t, gbp, usd), // GBP + USD both fiat sides
		mustPair(t, xlm, usd), // crypto base skipped
	}

	got := FiatCodesFromPairs(pairs, "eur") // exclusion is case-insensitive
	if len(got) != 2 {
		t.Fatalf("got %d codes (%v), want 2 (USD, GBP)", len(got), got)
	}
	for _, code := range []string{"USD", "GBP"} {
		a, ok := got[code]
		if !ok {
			t.Fatalf("missing %s in %v", code, got)
		}
		if a.Type != canonical.AssetFiat {
			t.Errorf("%s mapped to non-fiat asset %v", code, a)
		}
	}
	if _, ok := got["EUR"]; ok {
		t.Error("EUR (the venue base) must be excluded")
	}

	if crypto := FiatCodesFromPairs([]canonical.Pair{mustPair(t, xlm, mustCrypto(t, "BTC"))}, "USD"); len(crypto) != 0 {
		t.Errorf("all-crypto pair list should derive no fiat codes, got %v", crypto)
	}
}

// DefaultFXPairs("EUR") must include a USD/EUR pair — ecb is wired
// with base EUR in production (cmd/stellarindex-indexer) and its
// USD cube row must not be silently dropped by FiatCodesFromPairs'
// derived interest set (CA2-A26).
func TestDefaultFXPairs_EURBaseIncludesUSD(t *testing.T) {
	pairs := DefaultFXPairs("EUR")
	wanted := FiatCodesFromPairs(pairs, "EUR")
	if _, ok := wanted["USD"]; !ok {
		t.Fatalf("DefaultFXPairs(EUR) must yield a USD cross pair, got codes %v", wanted)
	}
}

// GetBody must set the caller's headers, cap the body read at
// LimitBytes, and return the status verbatim (status interpretation
// stays with the venue).
func TestGetBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q, want Bearer k", got)
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	status, body, err := GetBody(context.Background(), GetRequest{
		URL:        srv.URL,
		Headers:    map[string]string{"Authorization": "Bearer k"},
		LimitBytes: 10,
	})
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	if status != http.StatusTeapot {
		t.Errorf("status = %d, want 418", status)
	}
	if len(body) != 10 {
		t.Errorf("body len = %d, want 10 (LimitBytes cap)", len(body))
	}
}

// A transport error with RedactURL set must not echo the request
// URL's query string (the query param is where key-only vendors
// carry the API key — G10-04).
func TestGetBody_redactsTransportError(t *testing.T) {
	// Closed port → guaranteed transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, _, err := GetBody(context.Background(), GetRequest{
		URL:        url + "/latest?access_key=SECRET",
		LimitBytes: 10,
		RedactURL:  url + "/latest",
	})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("transport error leaked the query string: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("transport error should carry the redacted URL form: %v", err)
	}
}

// A request whose URL carries a secret (RedactURL set) must not follow
// a redirect: Go copies the full previous URL, query included, into the
// next hop's Referer header.
func TestGetBody_refusesRedirectWhenURLCarriesSecret(t *testing.T) {
	const canary = "LEAK-CANARY"
	var hops int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		if strings.Contains(r.Header.Get("Referer"), canary) {
			t.Errorf("redirect target received the secret in Referer: %q", r.Header.Get("Referer"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved", http.StatusFound)
	}))
	defer origin.Close()

	_, _, err := GetBody(context.Background(), GetRequest{
		URL:        origin.URL + "/latest?access_key=" + canary,
		LimitBytes: 10,
		RedactURL:  origin.URL + "/latest",
	})
	if hops != 0 {
		t.Errorf("redirect target was requested %d time(s), want 0", hops)
	}
	if err == nil {
		t.Fatal("GetBody followed a redirect for a secret-bearing URL; want an error")
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("redirect error leaked the query string: %v", err)
	}
}

// Requests without a URL secret keep following redirects.
func TestGetBody_followsRedirectWithoutURLSecret(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	status, body, err := GetBody(context.Background(), GetRequest{URL: origin.URL, LimitBytes: 10})
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Errorf("got status %d body %q, want 200 \"ok\"", status, body)
	}
}
