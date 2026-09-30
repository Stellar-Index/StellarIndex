package divergence_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

const (
	testProKey  = "pro-secret-key"
	testDemoKey = "demo-secret-key"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// capturingClient answers every request with a fresh /simple/price body and
// records the last request, so host selection is observable without DNS.
func capturingClient(seen **http.Request) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*seen = r
		body := fmt.Sprintf(`{"stellar":{"usd":0.1,"last_updated_at":%d}}`, time.Now().Unix())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
}

func TestCoinGeckoReference_DemoKeyHeader(t *testing.T) {
	var gotDemo, gotPro, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDemo = r.Header.Get("x-cg-demo-api-key")
		gotPro = r.Header.Get("x-cg-pro-api-key")
		gotQuery = r.URL.RawQuery
		_, _ = fmt.Fprintf(w, `{"stellar":{"usd":0.1,"last_updated_at":%d}}`, time.Now().Unix())
	}))
	defer ts.Close()

	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL:    ts.URL,
		DemoAPIKey: testDemoKey,
		IDMap:      map[string]string{"native": "stellar"},
	})
	if _, err := ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()); err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if gotDemo != testDemoKey {
		t.Errorf("x-cg-demo-api-key = %q, want the demo key", gotDemo)
	}
	if gotPro != "" {
		t.Errorf("x-cg-pro-api-key = %q, want empty with only a demo key", gotPro)
	}
	if strings.Contains(gotQuery, testDemoKey) || strings.Contains(gotQuery, "api_key") {
		t.Errorf("query %q carries the key", gotQuery)
	}
}

func TestCoinGeckoReference_ProKeySelectsProHost(t *testing.T) {
	var seen *http.Request
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		HTTPClient: capturingClient(&seen),
		APIKey:     testProKey,
		DemoAPIKey: testDemoKey,
		IDMap:      map[string]string{"native": "stellar"},
	})
	if _, err := ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()); err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if seen == nil {
		t.Fatal("no request issued")
	}
	if seen.URL.Host != "pro-api.coingecko.com" || seen.URL.Path != "/api/v3/simple/price" {
		t.Errorf("request went to %s%s, want pro-api.coingecko.com/api/v3/simple/price", seen.URL.Host, seen.URL.Path)
	}
	if got := seen.Header.Get("x-cg-pro-api-key"); got != testProKey {
		t.Errorf("x-cg-pro-api-key = %q, want the pro key", got)
	}
	if got := seen.Header.Get("x-cg-demo-api-key"); got != "" {
		t.Errorf("x-cg-demo-api-key = %q, want empty when a pro key wins", got)
	}
	if strings.Contains(seen.URL.RawQuery, testProKey) {
		t.Errorf("query %q carries the key", seen.URL.RawQuery)
	}
}

func TestCoinGeckoReference_NoKeyNoAuthHeader(t *testing.T) {
	var seen *http.Request
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		HTTPClient: capturingClient(&seen),
		IDMap:      map[string]string{"native": "stellar"},
	})
	if _, err := ref.LookupQuote(context.Background(), xlmUSD(t), time.Now()); err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if seen == nil {
		t.Fatal("no request issued")
	}
	if seen.URL.Host != "api.coingecko.com" {
		t.Errorf("host = %q, want api.coingecko.com when keyless", seen.URL.Host)
	}
	for _, h := range []string{"x-cg-pro-api-key", "x-cg-demo-api-key"} {
		if got := seen.Header.Get(h); got != "" {
			t.Errorf("%s = %q, want absent when keyless", h, got)
		}
	}
}

// The key must never surface in the wrapped error (it reaches Result
// outcomes) or the warning log, for an HTTP failure or a transport one.
func TestCoinGeckoReference_FailuresDoNotLeakKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	var logBuf bytes.Buffer
	ref := divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		BaseURL:    ts.URL,
		DemoAPIKey: testDemoKey,
		Logger:     slog.New(slog.NewTextHandler(&logBuf, nil)),
		IDMap:      map[string]string{"native": "stellar"},
	})
	_, err := ref.LookupQuote(context.Background(), xlmUSD(t), time.Now())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), testDemoKey) {
		t.Errorf("error %q carries the key", err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "status=401") {
		t.Errorf("log = %q, want one WARN with status=401", logged)
	}
	if strings.Count(logged, "\n") != 1 {
		t.Errorf("log has %d lines, want 1 per failed batch", strings.Count(logged, "\n"))
	}
	if strings.Contains(logged, testDemoKey) || strings.Contains(logged, "ids=") {
		t.Errorf("log %q carries the key or the query", logged)
	}

	failing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	ref = divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
		HTTPClient: failing,
		APIKey:     testProKey,
		IDMap:      map[string]string{"native": "stellar"},
	})
	_, err = ref.LookupQuote(context.Background(), xlmUSD(t), time.Now())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the transport failure", err)
	}
	if strings.Contains(err.Error(), testProKey) {
		t.Errorf("error %q carries the key", err)
	}
}
