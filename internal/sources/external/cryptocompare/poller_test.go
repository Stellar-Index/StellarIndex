package cryptocompare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func buildPairs(t *testing.T) []canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	btc, _ := canonical.NewCryptoAsset("BTC")
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	xlmEUR, _ := canonical.NewPair(xlm, eur)
	btcUSD, _ := canonical.NewPair(btc, usd)
	return []canonical.Pair{xlmUSD, xlmEUR, btcUSD}
}

func newTestServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Literal, not the constant: only /data/pricemultifull carries
		// LASTUPDATE, so the path itself is part of the contract.
		if r.URL.Path != "/data/pricemultifull" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Apikey ") {
			t.Errorf("bad Authorization header %q (want 'Apikey ...')", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestNewPoller_RejectsEmptyKey(t *testing.T) {
	_, err := NewPoller("")
	if !errors.Is(err, ErrAPIKeyRequired) {
		t.Errorf("expected ErrAPIKeyRequired, got %v", err)
	}
}

func TestPollOnce_HappyPath(t *testing.T) {
	srv := newTestServer(t, `{"RAW": {
      "XLM": {"USD": {"PRICE": 0.17582, "LASTUPDATE": 1710000000}, "EUR": {"PRICE": 0.16230, "LASTUPDATE": 1710000000}},
      "BTC": {"USD": {"PRICE": 50000.0, "LASTUPDATE": 1710000000}, "EUR": {"PRICE": 46250.0, "LASTUPDATE": 1710000000}}
    }}`, http.StatusOK)
	defer srv.Close()

	p, err := NewPoller("TEST_KEY")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, updates, err := p.PollOnce(ctx, buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	// XLM×2 + BTC×1 (no BTC/EUR pair asked for).
	// Actually our pair list has XLM/USD, XLM/EUR, BTC/USD.
	// Venue returns XLM/{USD,EUR} + BTC/{USD,EUR}.
	// We emit only configured combos → 3 updates.
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates, got %d", len(updates))
	}
}

func TestPollOnce_ErrorResponse(t *testing.T) {
	// CryptoCompare returns 200 OK with error envelope on auth
	// failures and unknown symbols.
	srv := newTestServer(t, `{
      "Response": "Error",
      "Message": "cccagg_or_exchange market does not exist for this coin pair"
    }`, http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrAPIRejected) {
		t.Errorf("expected ErrAPIRejected, got %v", err)
	}
}

func TestPollOnce_HTTP5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err == nil {
		t.Error("expected error on HTTP http.StatusServiceUnavailable")
	}
}

func TestPollOnce_CryptoOnlyPairsNoOp(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	p1, _ := canonical.NewPair(xlm, usdt)
	p, _ := NewPoller("TEST")
	p.Endpoint = "http://localhost:1" // would fail if reached
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{p1})
	if err != nil {
		t.Fatalf("should no-op, got: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(updates))
	}
}

// TestPollOnce_StampsUpstreamLastUpdate pins that each row carries the
// quote's upstream LASTUPDATE, not our poll time, so a frozen upstream
// reaches the aggregator tier with its real age.
func TestPollOnce_StampsUpstreamLastUpdate(t *testing.T) {
	frozen := time.Now().Add(-3 * time.Hour).Truncate(time.Second).UTC()
	fresh := time.Now().Add(-20 * time.Second).Truncate(time.Second).UTC()
	srv := newTestServer(t, fmt.Sprintf(`{"RAW": {
      "XLM": {"USD": {"PRICE": 0.17, "LASTUPDATE": %[1]d}, "EUR": {"PRICE": 0.16, "LASTUPDATE": %[1]d}},
      "BTC": {"USD": {"PRICE": 50000.0, "LASTUPDATE": %[2]d}}
    }}`, frozen.Unix(), fresh.Unix()), http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates, got %d", len(updates))
	}
	for _, u := range updates {
		want := fresh
		if u.Asset.Code == "XLM" {
			want = frozen
		}
		if !u.Timestamp.Equal(want) {
			t.Errorf("%s/%s Timestamp = %s, want upstream LASTUPDATE %s",
				u.Asset.Code, u.Quote.Code, u.Timestamp, want)
		}
	}
}

// TestPollOnce_MissingLastUpdateFailsClosed pins the fail-closed rule:
// a quote without LASTUPDATE is dropped, and a response with no datable
// quote is a poll error rather than an empty success.
func TestPollOnce_MissingLastUpdateFailsClosed(t *testing.T) {
	srv := newTestServer(t, `{"RAW": {
      "XLM": {"USD": {"PRICE": 0.17, "LASTUPDATE": 1710000000}, "EUR": {"PRICE": 0.16}},
      "BTC": {"USD": {"PRICE": 50000.0}}
    }}`, http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 1 || updates[0].Asset.Code != "XLM" || updates[0].Quote.Code != "USD" {
		t.Fatalf("expected only the dated XLM/USD row, got %d update(s)", len(updates))
	}

	allUndated := newTestServer(t, `{"RAW": {"XLM": {"USD": {"PRICE": 0.17}}}}`, http.StatusOK)
	defer allUndated.Close()
	p.Endpoint = allUndated.URL
	_, updates, err = p.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates, got %d", len(updates))
	}
}

func TestPollInterval_Default(t *testing.T) {
	p, _ := NewPoller("TEST")
	if p.PollInterval() != 60*time.Second {
		t.Errorf("default = %v", p.PollInterval())
	}
}
