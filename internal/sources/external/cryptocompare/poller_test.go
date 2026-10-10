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
	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
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

func TestPollOnce_CryptoOnlyPairsNoApplicablePairs(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	p1, _ := canonical.NewPair(xlm, usdt)
	p, _ := NewPoller("TEST")
	p.Endpoint = "http://localhost:1" // would fail if reached
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{p1})
	if !errors.Is(err, external.ErrNoApplicablePairs) {
		t.Fatalf("err = %v, want ErrNoApplicablePairs", err)
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

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	p, err := NewPoller("probe")
	if err != nil {
		t.Fatal(err)
	}
	p.Endpoint = trap.URL
	if _, _, err := p.PollOnce(context.Background(), buildPairs(t)); err == nil {
		t.Error("PollOnce succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}

// CryptoCompare is an aggregator (composite index across upstream
// venues), NOT an exchange. Class must be ClassAggregator so the
// aggregator's VWAP filter excludes it — including a composite
// index against retail liquidity would double-count the same
// upstream trades.

func TestPoller_NameAndClass(t *testing.T) {
	p, err := NewPoller("key")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if got := p.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
	if got := p.Class(); got != external.ClassAggregator {
		t.Errorf("Class() = %v, want ClassAggregator (CryptoCompare is an index, not an exchange)", got)
	}
}

// decimalStringToScaledInt mirrors the helper in exchangeratesapi /
// CMC / coingecko. Pin edges so a refactor that breaks
// negative-handling, fractional truncation, or scientific-notation
// rejection gets caught.

func TestDecimalStringToScaledInt_edges(t *testing.T) {
	cases := []struct {
		in        string
		decimals  int
		want      string
		wantError bool
	}{
		{"1.0", 8, "100000000", false},
		{"-2.5", 8, "-250000000", false},
		{".5", 8, "50000000", false},
		{"1.123456789", 8, "112345678", false}, // truncates
		{"42", 0, "42", false},
		{"", 8, "", true},
		{"1e3", 8, "", true},
		{"abc", 8, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := scale.DecimalStringToScaledInt(c.in, c.decimals)
			if c.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.want {
				t.Errorf("got %s, want %s", got.String(), c.want)
			}
		})
	}
}

func TestPoller_PollInterval_defaultAndOverride(t *testing.T) {
	p, err := NewPoller("k")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Interval = 0
	if got := p.PollInterval(); got != DefaultPollInterval {
		t.Errorf("PollInterval(zero) = %v, want %v", got, DefaultPollInterval)
	}
	p.Interval = 7 * time.Second
	if got := p.PollInterval(); got != 7*time.Second {
		t.Errorf("PollInterval(7s) = %v, want 7s", got)
	}
}
