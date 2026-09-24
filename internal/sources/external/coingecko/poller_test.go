package coingecko

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
		if r.URL.Path != SimplePricePath {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("include_last_updated_at"); got != "true" {
			t.Errorf("include_last_updated_at = %q, want true (rows must carry the upstream publication time)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestPollOnce_HappyPath(t *testing.T) {
	srv := newTestServer(t, `{
      "stellar":  {"usd": 0.17582, "eur": 0.16230, "last_updated_at": 1710000000},
      "bitcoin":  {"usd": 50000.0, "eur": 46250.0, "last_updated_at": 1710000000}
    }`, http.StatusOK)
	defer srv.Close()

	p := NewPoller()
	p.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trades, updates, err := p.PollOnce(ctx, buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(trades) != 0 {
		t.Errorf("expected 0 trades (aggregator emits updates only), got %d", len(trades))
	}
	// Exact-combo filter: we asked for XLM/USD, XLM/EUR, BTC/USD.
	// BTC/EUR is returned by the venue but not emitted because
	// no operator-configured pair targets that combo.
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates (XLM/USD, XLM/EUR, BTC/USD — BTC/EUR filtered), got %d", len(updates))
	}

	// Verify XLM/USD specifically.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	var xlmU *canonical.OracleUpdate
	for i := range updates {
		if updates[i].Asset.Equal(xlm) && updates[i].Quote.Equal(usd) {
			xlmU = &updates[i]
			break
		}
	}
	if xlmU == nil {
		t.Fatal("missing XLM/USD update")
	}
	// 0.17582 × 10^8 = 17_582_000 (with ±rounding tolerance).
	priceInt := xlmU.Price.BigInt().Int64()
	if priceInt < 17_580_000 || priceInt > 17_584_000 {
		t.Errorf("XLM/USD price = %d want ~17582000", priceInt)
	}
	if xlmU.Decimals != 8 {
		t.Errorf("decimals = %d want 8", xlmU.Decimals)
	}
	if len(xlmU.TxHash) != 64 {
		t.Errorf("tx_hash len = %d", len(xlmU.TxHash))
	}
	if xlmU.Source != "coingecko" {
		t.Errorf("Source = %q", xlmU.Source)
	}
}

func TestPollOnce_UnknownTickerSkipped(t *testing.T) {
	// A pair with DOT (not in tickerToID default) shouldn't cause
	// errors; CoinGecko will just not receive the id, and the
	// matching response won't contain it.
	dot, err := canonical.NewCryptoAsset("DOT")
	if err != nil {
		t.Skip("DOT not on crypto allow-list; skipping")
	}
	usd, _ := canonical.NewFiatAsset("USD")
	dotUSD, _ := canonical.NewPair(dot, usd)

	srv := newTestServer(t, `{"stellar":{"usd":0.17,"last_updated_at":1710000000}}`, http.StatusOK)
	defer srv.Close()
	p := NewPoller()
	p.Endpoint = srv.URL

	// DOT is in tickerToID but "polkadot" not in fixture response.
	// Just XLM/USD should come back if the pair also included XLM.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{dotUSD, xlmUSD})
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	// Venue returned only stellar/usd; we emit 1 update.
	if len(updates) != 1 {
		t.Errorf("expected 1 update, got %d", len(updates))
	}
}

func TestPollOnce_CryptoOnlyPairs_NoOp(t *testing.T) {
	// Both sides of the pair are crypto — no fiat quote to request.
	// Poller should no-op silently (no HTTP call).
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	xlmUsdt, _ := canonical.NewPair(xlm, usdt)

	p := NewPoller()
	p.Endpoint = "http://localhost:1" // would fail if reached
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{xlmUsdt})
	if err != nil {
		t.Fatalf("should no-op, got err: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(updates))
	}
}

func TestPollOnce_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := NewPoller()
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err == nil {
		t.Error("expected error on HTTP 429")
	}
}

func TestPollOnce_MalformedJSON(t *testing.T) {
	srv := newTestServer(t, `{not json`, http.StatusOK)
	defer srv.Close()
	p := NewPoller()
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestPollOnce_TickerToIDOverride(t *testing.T) {
	// With a custom TickerToID, the poller queries only the tickers
	// the operator passed in — even if the package default would
	// have resolved more. Catalogue-driven wiring (R-018 Phase 1.2)
	// relies on this to scope the poll set to the verified seed.
	srv := newTestServer(t, `{
      "stellar":  {"usd": 0.17, "last_updated_at": 1710000000},
      "bitcoin":  {"usd": 50000.0, "last_updated_at": 1710000000}
    }`, http.StatusOK)
	defer srv.Close()

	p := NewPoller()
	p.Endpoint = srv.URL
	// Override scope: only XLM. BTC pair is supplied but BTC isn't
	// in the override map, so it must be skipped — even though the
	// venue returns a bitcoin price in the response.
	p.TickerToID = map[string]string{"XLM": "stellar"}

	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	// XLM × {USD, EUR} = 2 expected; BTC filtered.
	xlmCount := 0
	for _, u := range updates {
		if u.Asset.Code != "XLM" {
			t.Errorf("unexpected non-XLM update: %s/%s", u.Asset.Code, u.Quote.Code)
		}
		xlmCount++
	}
	if xlmCount == 0 {
		t.Error("expected at least one XLM update; got none")
	}
}

func TestPollInterval_Default(t *testing.T) {
	p := NewPoller()
	if p.PollInterval() != DefaultPollInterval {
		t.Errorf("default = %v want %v", p.PollInterval(), DefaultPollInterval)
	}
	// Pin the demo-tier-safe 300s default so a future cadence drop
	// past 300s — which would push burn rate back toward the 10K/day
	// ceiling on a shared IP — fails this test (F-0030).
	if DefaultPollInterval != 300*time.Second {
		t.Errorf("DefaultPollInterval = %v; expected 300s — dropping this below 300s risks tripping CoinGecko's 10K/day demo cap (F-0030)", DefaultPollInterval)
	}
}

func TestFloatToScaledInt_Precision(t *testing.T) {
	// 0.17582 at 10^8 should round to 17_582_000 (±1).
	got, err := scale.FloatToScaledInt(0.17582, 8)
	if err != nil {
		t.Fatalf("floatToScaledInt: %v", err)
	}
	n := got.Int64()
	if n < 17_581_999 || n > 17_582_001 {
		t.Errorf("0.17582 → %d, expected ≈17582000", n)
	}
	// Negative rejected.
	if _, err := scale.FloatToScaledInt(-1, 8); err == nil {
		t.Error("expected error for negative")
	}
}

// TestPollOnce_StampsUpstreamLastUpdated pins that each row carries its
// id's upstream last_updated_at, not our poll time. A CoinGecko cache
// frozen for hours must reach the aggregator tier with its real age so
// the tier's freshness filter (10 min default) rejects it.
func TestPollOnce_StampsUpstreamLastUpdated(t *testing.T) {
	frozen := time.Now().Add(-3 * time.Hour).Truncate(time.Second).UTC()
	fresh := time.Now().Add(-20 * time.Second).Truncate(time.Second).UTC()
	srv := newTestServer(t, fmt.Sprintf(`{
      "stellar": {"usd": 0.17, "eur": 0.16, "last_updated_at": %d},
      "bitcoin": {"usd": 50000.0, "last_updated_at": %d}
    }`, frozen.Unix(), fresh.Unix()), http.StatusOK)
	defer srv.Close()

	p := NewPoller()
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
			t.Errorf("%s/%s Timestamp = %s, want upstream last_updated_at %s",
				u.Asset.Code, u.Quote.Code, u.Timestamp, want)
		}
	}
}

// TestPollOnce_MissingLastUpdatedFailsClosed pins the fail-closed rule:
// an id without an upstream time is dropped, and a response in which
// no id is datable is a poll error rather than an empty success.
func TestPollOnce_MissingLastUpdatedFailsClosed(t *testing.T) {
	srv := newTestServer(t, `{
      "stellar": {"usd": 0.17, "eur": 0.16, "last_updated_at": 1710000000},
      "bitcoin": {"usd": 50000.0}
    }`, http.StatusOK)
	defer srv.Close()
	p := NewPoller()
	p.Endpoint = srv.URL
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 XLM updates (undated bitcoin dropped), got %d", len(updates))
	}
	for _, u := range updates {
		if u.Asset.Code != "XLM" {
			t.Errorf("undated id emitted a row: %s/%s", u.Asset.Code, u.Quote.Code)
		}
	}

	allUndated := newTestServer(t, `{"stellar": {"usd": 0.17}, "bitcoin": {"usd": 50000.0}}`, http.StatusOK)
	defer allUndated.Close()
	p2 := NewPoller()
	p2.Endpoint = allUndated.URL
	_, updates, err = p2.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates, got %d", len(updates))
	}
}
