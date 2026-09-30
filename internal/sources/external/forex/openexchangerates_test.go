package forex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

const testAppID = "oxr-app-id-must-not-leak"

// oxrRequest is what oxrServer saw of the provider's request.
type oxrRequest struct{ path, query, auth string }

// oxrServer serves body with status and records the request it saw.
func oxrServer(t *testing.T, status int, body string) (*httptest.Server, *oxrRequest) {
	t.Helper()
	seen := &oxrRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = oxrRequest{path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestOpenExchangeRates_GoldenBoard(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "oxr-latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv, seen := oxrServer(t, http.StatusOK, string(body))
	p := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: srv.URL}

	rates, published, err := p.LatestUSDRates(context.Background())
	if err != nil {
		t.Fatalf("LatestUSDRates: %v", err)
	}
	if len(rates) != 172 {
		t.Errorf("rates = %d, want 172", len(rates))
	}
	for code, want := range map[string]float64{"GBP": 0.753763, "CHF": 0.834806, "EUR": 0.881493, "USD": 1} {
		if rates[code] != want {
			t.Errorf("rates[%s] = %v, want %v", code, rates[code], want)
		}
	}
	if want := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC); !published.Equal(want) {
		t.Errorf("published = %v, want %v", published, want)
	}

	if seen.path != "/latest.json" {
		t.Errorf("path = %q, want /latest.json", seen.path)
	}
	if seen.query != "" {
		t.Errorf("query = %q, want none (the Free plan refuses base/symbols; app_id must not ride the URL)", seen.query)
	}
	if got := seen.auth; got != "Token "+testAppID {
		t.Errorf("Authorization = %q, want the Token scheme", got)
	}
}

func TestOpenExchangeRates_RefusesBadBoards(t *testing.T) {
	cases := map[string]string{
		"non_usd_base":  `{"timestamp":1790784000,"base":"EUR","rates":{"GBP":0.85}}`,
		"missing_base":  `{"timestamp":1790784000,"rates":{"GBP":0.85}}`,
		"missing_rates": `{"timestamp":1790784000,"base":"USD"}`,
		"empty_rates":   `{"timestamp":1790784000,"base":"USD","rates":{}}`,
		"no_timestamp":  `{"base":"USD","rates":{"GBP":0.75}}`,
		"zero_rate":     `{"timestamp":1790784000,"base":"USD","rates":{"GBP":0.75,"EUR":0}}`,
		"negative_rate": `{"timestamp":1790784000,"base":"USD","rates":{"GBP":0.75,"EUR":-0.88}}`,
		"null_rate":     `{"timestamp":1790784000,"base":"USD","rates":{"GBP":0.75,"EUR":null}}`,
		"string_rate":   `{"timestamp":1790784000,"base":"USD","rates":{"GBP":0.75,"EUR":"NaN"}}`,
		"overflow_rate": `{"timestamp":1790784000,"base":"USD","rates":{"GBP":0.75,"EUR":1e400}}`,
		"not_json":      `<html>maintenance</html>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := oxrServer(t, http.StatusOK, body)
			rates, _, err := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: srv.URL}.LatestUSDRates(context.Background())
			if err == nil {
				t.Fatalf("accepted %s: %v", name, rates)
			}
			if rates != nil {
				t.Errorf("returned %d rates beside the error; a refused board must yield none", len(rates))
			}
			if !errors.Is(err, ErrOXRBoard) {
				t.Errorf("err = %v, want ErrOXRBoard", err)
			}
		})
	}
}

func TestOpenExchangeRates_AppIDNeverInErrors(t *testing.T) {
	// The upstream echoing the id back is the worst case for redaction.
	echo := `{"error":true,"status":401,"message":"invalid_app_id ` + testAppID + `"}`
	cases := map[string]struct {
		status int
		body   string
	}{
		"unauthorized": {http.StatusUnauthorized, echo},
		"quota":        {http.StatusTooManyRequests, `{"message":"access_restricted"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := oxrServer(t, tc.status, tc.body)
			_, _, err := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: srv.URL}.LatestUSDRates(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), testAppID) {
				t.Errorf("error leaks the app id: %v", err)
			}
		})
	}

	// Transport failure: the URL in *url.Error must not carry the id either.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, _, err := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: dead.URL}.LatestUSDRates(context.Background())
	if err == nil || strings.Contains(err.Error(), testAppID) {
		t.Errorf("transport error = %v; want an error without the app id", err)
	}
}

func TestOpenExchangeRates_EmptyAppIDSkipsTheRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	if _, _, err := (OpenExchangeRatesProvider{Endpoint: srv.URL}).LatestUSDRates(context.Background()); err == nil {
		t.Error("empty app id: want an error")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream hit %d time(s) without an app id; each request spends metered quota", n)
	}
}

func TestOpenExchangeRates_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	p := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: trap.URL}
	if _, _, err := p.LatestUSDRates(context.Background()); err == nil {
		t.Error("LatestUSDRates succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}

func TestWorker_CorroboratorIsHeldNotConsulted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	oxr := OpenExchangeRatesProvider{AppID: testAppID, Endpoint: srv.URL}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	w := NewWorker(NewClient("k").WithBase(failing.URL), NewCache(), slog.New(slog.NewTextHandler(io.Discard, nil)), 0).
		WithCorroborator(oxr)

	if got := w.Corroborator(); got != RateProvider(oxr) {
		t.Errorf("Corroborator() = %v, want the registered provider", got)
	}
	if _, _, _, err := w.fetchRates(context.Background()); err == nil {
		t.Error("fetchRates served with only a corroborator behind a failing primary")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("corroborator fetched %d time(s); it must stay out of the serving chain", n)
	}
}
