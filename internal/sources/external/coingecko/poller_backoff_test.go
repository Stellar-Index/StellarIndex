package coingecko

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// If an upstream 429 only returned an error, the poll loop's
// fixed-cadence ticker would re-fire 60 s later, hitting the venue
// again — observed live on r1 as one WARN per minute.
// These tests pin the behaviour:
//   - 429 arms a cooldown using Retry-After (or exponential backoff).
//   - Subsequent PollOnce calls during cooldown skip the HTTP request
//     entirely and return (nil, nil, nil) — distinct from an error.
//   - A successful response resets the backoff.
//   - 403 is treated the same as 429 (CoinGecko's post-2024 demo-key-
//     required path returns 403, not 429).

func newCountingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestPollOnce_429_ArmsCooldown_SkipsNextCall(t *testing.T) {
	srv, hits := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"throttled"}`))
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	// First call hits the venue and gets 429 — error returned, cooldown armed.
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err == nil {
		t.Fatal("expected error from 429")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 after first 429", got)
	}
	if p.cooldownRemaining() <= 0 {
		t.Fatal("cooldown should be armed after 429")
	}

	// Second call must be a silent no-op (no HTTP, no error).
	trades, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Errorf("second call should not error during cooldown: %v", err)
	}
	if trades != nil || updates != nil {
		t.Errorf("second call should return (nil, nil), got %v / %v", trades, updates)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d after second call, want still 1 (cooldown skipped HTTP)", got)
	}
}

func TestPollOnce_429_HonorsRetryAfter(t *testing.T) {
	const retrySeconds = 90 // > MinBackoff so we can verify it overrides the floor
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		w.WriteHeader(http.StatusTooManyRequests)
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))

	cooldown := p.cooldownRemaining()
	// Retry-After of 90 s, with a small slack for test scheduling.
	if cooldown < 85*time.Second || cooldown > retrySeconds*time.Second {
		t.Errorf("cooldown = %v, want ~%ds (Retry-After honoured)", cooldown, retrySeconds)
	}
}

func TestPollOnce_429_NoRetryAfter_UsesMinBackoff(t *testing.T) {
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))

	cooldown := p.cooldownRemaining()
	if cooldown < (MinBackoff-2*time.Second) || cooldown > MinBackoff {
		t.Errorf("cooldown = %v, want ~%v (no Retry-After → MinBackoff)", cooldown, MinBackoff)
	}
}

func TestPollOnce_SuccessResetsBackoff(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)

	srv, _ := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			_, _ = w.Write([]byte(`{"stellar":{"usd":0.17,"last_updated_at":1710000000}}`))
		}
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	// First poll: 429, backoff armed.
	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	if p.currentBackoff == 0 {
		t.Fatal("expected currentBackoff > 0 after 429")
	}

	// Manually expire the cooldown (test scaffolding — production
	// waits real time, but we don't want the test to sleep 60s).
	p.mu.Lock()
	p.nextAllowedAt = time.Time{}
	p.mu.Unlock()

	// Flip the server to 200 OK; poll again.
	status.Store(http.StatusOK)
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if p.currentBackoff != 0 {
		t.Errorf("currentBackoff = %v after success, want 0 (reset)", p.currentBackoff)
	}
	if !p.nextAllowedAt.IsZero() {
		t.Errorf("nextAllowedAt should be zero after success reset")
	}
}

func TestPollOnce_403_TreatedAsThrottling(t *testing.T) {
	// CoinGecko's post-2024 free-tier-without-demo-key returns 403,
	// not 429. The cooldown must apply either way so we don't hammer
	// the venue with refusals.
	srv, hits := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":{"error_code":429,"error_message":"You've exceeded the Rate Limit"}}`))
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if err == nil {
		t.Fatal("expected error from 403")
	}
	if p.cooldownRemaining() <= 0 {
		t.Error("cooldown should be armed after 403")
	}

	// Second call: skipped by cooldown.
	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1 (cooldown skipped second HTTP)", got)
	}
}

func TestBackoffFromRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		hdr  string
		want time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "30", 30 * time.Second},
		{"seconds-with-whitespace", "  60 ", 60 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"garbage", "not-a-number", 0},
		{"http-date-past", "Mon, 01 Jan 2000 00:00:00 GMT", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backoffFromRetryAfter(tc.hdr)
			if got != tc.want {
				t.Errorf("backoffFromRetryAfter(%q) = %v, want %v", tc.hdr, got, tc.want)
			}
		})
	}
}

// G10-04: the demo key now travels in the x-cg-demo-api-key HEADER,
// never the query string (so it can't leak via a *url.Error in logs).
func TestPollOnce_DemoAPIKeyParameter(t *testing.T) {
	var seenQuery, seenDemo, seenPro string
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		seenDemo = r.Header.Get("x-cg-demo-api-key")
		seenPro = r.Header.Get("x-cg-pro-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	p := NewPoller()
	p.Endpoint = srv.URL
	p.DemoAPIKey = "demo-test-key-123"

	_, _, _ = p.PollOnce(context.Background(), []canonical.Pair{
		mustPair(t, "XLM", "USD"),
	})

	if seenDemo != "demo-test-key-123" {
		t.Errorf("missing demo key header; got %q", seenDemo)
	}
	if seenPro != "" {
		t.Error("Pro key header should not be set when only DemoAPIKey is configured")
	}
	// Security property: the key must NOT appear in the query string.
	if contains(seenQuery, "demo-test-key-123") || contains(seenQuery, "x_cg_demo_api_key") {
		t.Errorf("key/param leaked into query string (G10-04): %q", seenQuery)
	}
}

// G10-04: the Pro key travels in the x-cg-pro-api-key HEADER and still
// wins over the demo key when both are configured.
func TestPollOnce_ProAPIKeyWinsOverDemo(t *testing.T) {
	var seenQuery, seenDemo, seenPro string
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		seenDemo = r.Header.Get("x-cg-demo-api-key")
		seenPro = r.Header.Get("x-cg-pro-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	p := NewPoller()
	p.Endpoint = srv.URL
	p.APIKey = "pro-key"
	p.DemoAPIKey = "demo-key"

	_, _, _ = p.PollOnce(context.Background(), []canonical.Pair{
		mustPair(t, "XLM", "USD"),
	})

	if seenPro != "pro-key" {
		t.Errorf("Pro key header should be sent when both are set; got %q", seenPro)
	}
	if seenDemo != "" {
		t.Error("Demo key header should NOT be sent when Pro key is set")
	}
	// Security property: neither key in the query string.
	if contains(seenQuery, "pro-key") || contains(seenQuery, "demo-key") ||
		contains(seenQuery, "x_cg_pro_api_key") || contains(seenQuery, "x_cg_demo_api_key") {
		t.Errorf("key/param leaked into query string (G10-04): %q", seenQuery)
	}
}

// TestPollOnce_429_LowRetryAfter_StillGrowsBackoff — a Retry-After
// branch that takes the hint at face value (clamped to MinBackoff)
// bypasses the doubling. CoinGecko's free tier
// returns Retry-After consistently below MinBackoff (≈30s), so
// clamping lands the cooldown at exactly MinBackoff = 60s
// forever. The runner's PollInterval is also 60s, so each
// recovery attempt produces another 429 → another 60s cooldown
// → indefinite throttling at one 429-per-minute. Observed live
// on r1 for a full day.
//
// applyBackoff treats Retry-After as a FLOOR, not a
// ceiling — consecutive 429s grow the cooldown exponentially
// regardless of what the venue claims you can retry after.
func TestPollOnce_429_LowRetryAfter_StillGrowsBackoff(t *testing.T) {
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Simulate CoinGecko free tier: Retry-After consistently
		// below MinBackoff. Clamping would pin us to MinBackoff;
		// the doubling wins.
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	first := p.cooldownRemaining()
	if first < (MinBackoff-2*time.Second) || first > MinBackoff {
		t.Fatalf("first cooldown = %v, want ~MinBackoff (%v)", first, MinBackoff)
	}

	// Second 429 (cooldown reset for the test by stomping nextAllowedAt
	// — the runner would not call PollOnce until cooldown expired, but
	// in production we'd then get another 429 and need to grow). Force
	// the second call by clearing nextAllowedAt without touching
	// currentBackoff.
	p.mu.Lock()
	p.nextAllowedAt = time.Time{}
	p.mu.Unlock()

	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	second := p.cooldownRemaining()
	// Want ~120s (doubled from 60s), well above the 10s Retry-After
	// hint. Allow generous slack for scheduling noise.
	if second < 110*time.Second || second > 130*time.Second {
		t.Fatalf("second cooldown = %v, want ~2×MinBackoff = 120s (Retry-After=10s should NOT defeat exponential growth)", second)
	}

	// Third 429 → ~240s.
	p.mu.Lock()
	p.nextAllowedAt = time.Time{}
	p.mu.Unlock()
	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	third := p.cooldownRemaining()
	if third < 230*time.Second || third > 250*time.Second {
		t.Fatalf("third cooldown = %v, want ~4×MinBackoff = 240s", third)
	}
}

// TestPollOnce_429_HighRetryAfter_HonouredAsFloor — when the
// venue asks for an unusually long Retry-After (longer than our
// growth trajectory), we honour it. The fix raised the floor; it
// did not lower the ceiling.
func TestPollOnce_429_HighRetryAfter_HonouredAsFloor(t *testing.T) {
	srv, _ := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "300") // 5 min — well above first doubled value (60 s)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	p := NewPoller()
	p.Endpoint = srv.URL

	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	cool := p.cooldownRemaining()
	if cool < 290*time.Second || cool > 300*time.Second {
		t.Errorf("cooldown = %v, want ~300s (Retry-After honoured as floor)", cool)
	}
}

func mustPair(t *testing.T, base, quote string) canonical.Pair {
	t.Helper()
	b, err := canonical.NewCryptoAsset(base)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%q): %v", base, err)
	}
	q, err := canonical.NewFiatAsset(quote)
	if err != nil {
		t.Fatalf("NewFiatAsset(%q): %v", quote, err)
	}
	pair, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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

func TestPollOnce_CryptoOnlyPairs_NoApplicablePairs(t *testing.T) {
	// Both sides of the pair are crypto — no fiat quote to request. No HTTP
	// call, and the runner must not read it as a fresh poll.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	xlmUsdt, _ := canonical.NewPair(xlm, usdt)

	p := NewPoller()
	p.Endpoint = "http://localhost:1" // would fail if reached
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{xlmUsdt})
	if !errors.Is(err, external.ErrNoApplicablePairs) {
		t.Fatalf("err = %v, want ErrNoApplicablePairs", err)
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
	// have resolved more. Catalogue-driven wiring
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

// The paid key travels in a custom header, which net/http re-sends on
// every redirect hop regardless of host.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "x-cg-pro-api-key")
	p := NewPoller()
	p.Endpoint = trap.URL
	p.APIKey = "probe"
	if _, _, err := p.PollOnce(context.Background(), buildPairs(t)); err == nil {
		t.Error("PollOnce succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}

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
