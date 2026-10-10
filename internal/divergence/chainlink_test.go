package divergence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	externalchainlink "github.com/Stellar-Index/StellarIndex/internal/sources/external/chainlink"
)

// fakeChainlinkRPC returns a server that responds to a latestRoundData()
// eth_call with the supplied result (hex string with 0x prefix) and to a
// decimals() eth_call with 8 — Chainlink's standard, which is what every
// feed spec in these tests asserts, so the on-chain verification agrees
// and the price path under test is reached. Cases that need a different
// or failing decimals() use newChainlinkFakeRPC.
func fakeChainlinkRPC(t *testing.T, answerHex string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if ethCallData(r) == chainlinkDecimalsSelector {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + bigInt256Hex(big.NewInt(8)) + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + answerHex + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ethCallData extracts params[0].data from a JSON-RPC eth_call request
// body so fakes can dispatch on the function selector. Empty when the
// body is not an eth_call.
func ethCallData(r *http.Request) string {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method != "eth_call" || len(req.Params) == 0 {
		return ""
	}
	var call struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(req.Params[0], &call); err != nil {
		return ""
	}
	return call.Data
}

func mustPair(t *testing.T, base, quote string) canonical.Pair {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", base, err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", quote, err)
	}
	p, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func TestChainlink_LookupQuote_HappyPath_BTC_USD(t *testing.T) {
	// Chainlink feed answer: 65,432.10 USD * 10^8 = 6,543,210,000,000.
	// Hex of 6543210000000 = 0x5F3115DBE80, padded to 32 bytes =
	// 0x0000000000000000000000000000000000000000000000000000005F3115DBE80
	// Wait — 6543210000000 = 0x5F3115DBE80 (44 bits). Pad to 64 hex chars.
	answer := big.NewInt(6_543_210_000_000)
	hexStr := roundDataHex(answer, time.Now().Add(-time.Minute))

	srv := fakeChainlinkRPC(t, hexStr)
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"native/fiat:USD": {
				Address:  "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c",
				Decimals: 8,
			},
		},
	})

	pair := mustPair(t, "native", "fiat:USD")
	got, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	want := 65432.10
	if abs(got-want) > 0.001 {
		t.Errorf("got %.4f, want %.4f", got, want)
	}
}

func TestChainlink_LookupQuote_Inverted(t *testing.T) {
	// Feed publishes EUR/USD = 1.08. Operator wants USD/EUR.
	// Raw answer: 108,000,000 (1.08 × 10^8). Inverted = 0.9259...
	answer := big.NewInt(108_000_000)
	hexStr := roundDataHex(answer, time.Now().Add(-time.Minute))

	srv := fakeChainlinkRPC(t, hexStr)
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"fiat:USD/fiat:EUR": {
				Address:  "0xb49f677943BC038e9857d61E7d053CaA2C1734C1",
				Decimals: 8,
				Invert:   true,
			},
		},
	})

	pair := mustPair(t, "fiat:USD", "fiat:EUR")
	got, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	want := 1.0 / 1.08
	if abs(got-want) > 0.001 {
		t.Errorf("inverted got %.4f, want %.4f", got, want)
	}
}

// TestChainlink_LookupQuote_InvertedIsCorrectlyRounded pins the inverse
// to the nearest float64 of the exact 10^8/answer. Inverting the already
// rounded float instead lands one ulp off for this answer.
func TestChainlink_LookupQuote_InvertedIsCorrectlyRounded(t *testing.T) {
	answer := big.NewInt(100_000_001)
	srv := fakeChainlinkRPC(t, roundDataHex(answer, time.Now().Add(-time.Minute)))
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"fiat:USD/fiat:EUR": {
				Address:  "0xb49f677943BC038e9857d61E7d053CaA2C1734C1",
				Decimals: 8,
				Invert:   true,
			},
		},
	})
	got, err := priceOf(ref.LookupQuote(context.Background(), mustPair(t, "fiat:USD", "fiat:EUR"), time.Now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	want, _ := big.NewRat(100_000_000, 100_000_001).Float64()
	if got != want {
		t.Errorf("inverted = %v, want %v (correctly rounded 10^8/100000001)", got, want)
	}
}

func TestChainlink_LookupQuote_UnsupportedAsset(t *testing.T) {
	srv := fakeChainlinkRPC(t, "0x"+strings.Repeat("0", 64))
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL:  srv.URL,
		FeedMap: map[string]ChainlinkFeed{}, // empty
	})
	pair := mustPair(t, "native", "fiat:USD")
	_, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
	if err == nil {
		t.Fatal("expected ErrAssetUnsupported")
	}
	if !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("err=%v not wrapping ErrAssetUnsupported", err)
	}
}

func TestChainlink_LookupQuote_RPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":-32603,"message":"internal"}}`))
	}))
	defer srv.Close()
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"native/fiat:USD": {Address: "0x" + strings.Repeat("a", 40), Decimals: 8},
		},
	})
	pair := mustPair(t, "native", "fiat:USD")
	_, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "rpc status 500") {
		t.Errorf("err=%v missing 'rpc status 500'", err)
	}
}

func TestChainlink_ScaleAnswer(t *testing.T) {
	// 12_345_678 * 10^8 should scale to 0.12345678
	got, err := scaleChainlinkAnswer(big.NewInt(12_345_678), 8, false)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	want := 0.12345678
	if abs(got-want) > 1e-9 {
		t.Errorf("got %.10f, want %.10f", got, want)
	}
}

// TestChainlink_ScaleAnswerZeroDecimalsAboveInt64 pins the zero-decimals
// path to the same exact conversion as every other scale: an int256
// answer beyond int64 must not wrap through big.Int.Int64().
func TestChainlink_ScaleAnswerZeroDecimalsAboveInt64(t *testing.T) {
	answer := new(big.Int).Lsh(big.NewInt(1), 70)
	got, err := scaleChainlinkAnswer(answer, 0, false)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if want := 1180591620717411303424.0; got != want {
		t.Errorf("scaleChainlinkAnswer(2^70, 0, false) = %v, want %v", got, want)
	}
	neg, err := scaleChainlinkAnswer(new(big.Int).Neg(answer), 0, false)
	if err != nil {
		t.Fatalf("scale negative: %v", err)
	}
	if want := -1180591620717411303424.0; neg != want {
		t.Errorf("scaleChainlinkAnswer(-2^70, 0, false) = %v, want %v", neg, want)
	}
}

func TestChainlink_Name(t *testing.T) {
	r := NewChainlinkReference(ChainlinkOptions{})
	if r.Name() != "chainlink" {
		t.Errorf("Name=%q want chainlink", r.Name())
	}
}

// TestChainlink_DefaultFeedMapCoversCommonPairs pins the regression
// fixed alongside the CoinGecko default-IDMap fix: an
// operator deploying with a stock config (no
// `[divergence.chainlink].feed_map` block) must not get
// asset_unsupported on the major crypto + fiat-anchor pairs the
// aggregator computes by default.
func TestChainlink_DefaultFeedMapCoversCommonPairs(t *testing.T) {
	// Fake RPC: every call returns 100 * 10^8 = 10_000_000_000.
	answer := big.NewInt(10_000_000_000)
	srv := fakeChainlinkRPC(t, roundDataHex(answer, time.Now().Add(-time.Minute)))

	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		// FeedMap deliberately empty — the constructor must seed
		// the default.
	})

	// Every pair below is in the default. Each should resolve
	// without asset_unsupported.
	pairs := []struct{ base, quote string }{
		{"crypto:BTC", "fiat:USD"},
		{"crypto:ETH", "fiat:USD"},
		{"crypto:LINK", "fiat:USD"},
		{"fiat:EUR", "fiat:USD"},
		{"fiat:GBP", "fiat:USD"},
		{"fiat:JPY", "fiat:USD"},
	}
	for _, p := range pairs {
		t.Run(p.base+"_"+p.quote, func(t *testing.T) {
			pair := mustPair(t, p.base, p.quote)
			price, err := priceOf(ref.LookupQuote(context.Background(), pair, time.Now()))
			if err != nil {
				t.Fatalf("LookupQuote(%s): %v — default feed map missing entry?", pair.String(), err)
			}
			if price <= 0 {
				t.Errorf("LookupQuote(%s) = %g, want positive", pair.String(), price)
			}
		})
	}
}

// TestChainlink_DefaultFeedMapMatchesSource pins the invariant that
// this package's default feed map cannot silently drift from the
// ingest source's: same key set, same address, decimals and invert
// per key (two independently hardcoded copies gave a real
// divergence between the two proxies no way to surface).
func TestChainlink_DefaultFeedMapMatchesSource(t *testing.T) {
	src := externalchainlink.DefaultFeedMap()
	got := defaultChainlinkFeedMap()
	if len(got) != len(src) {
		t.Fatalf("defaultChainlinkFeedMap has %d entries, source has %d", len(got), len(src))
	}
	for k, want := range src {
		g, ok := got[k]
		if !ok {
			t.Errorf("defaultChainlinkFeedMap missing key %q present in source", k)
			continue
		}
		if g.Address != want.Address {
			t.Errorf("%s: Address = %s, source has %s", k, g.Address, want.Address)
		}
		if g.Decimals != int(want.Decimals) {
			t.Errorf("%s: Decimals = %d, source has %d", k, g.Decimals, want.Decimals)
		}
		if g.Invert != want.Invert {
			t.Errorf("%s: Invert = %v, source has %v", k, g.Invert, want.Invert)
		}
	}
}

// TestChainlink_OperatorOverridesDefault verifies that an entry in
// opts.FeedMap wins over the matching default entry.
func TestChainlink_OperatorOverridesDefault(t *testing.T) {
	srv := fakeChainlinkRPC(t, bigInt256Hex(big.NewInt(1_00000000)))
	const overrideAddr = "0xdeadbeef00000000000000000000000000000001"
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"crypto:BTC/fiat:USD": {Address: overrideAddr, Decimals: 8},
		},
	})
	got := ref.feedMap["crypto:BTC/fiat:USD"]
	if got.Address != overrideAddr {
		t.Errorf("operator override lost — got Address=%s want %s", got.Address, overrideAddr)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────

// bigInt256Hex pads a positive big.Int to 32-byte 0x-prefixed hex.
// roundDataHex builds a 160-byte latestRoundData() eth_call result:
// (roundId, answer, startedAt, updatedAt, answeredInRound), one
// 32-byte word each. roundId/startedAt/answeredInRound are dummies.
func roundDataHex(answer *big.Int, updatedAt time.Time) string {
	word := func(n *big.Int) string {
		h := n.Text(16)
		for len(h) < 64 {
			h = "0" + h
		}
		return h
	}
	one := big.NewInt(1)
	ts := big.NewInt(updatedAt.Unix())
	return "0x" + word(one) + word(answer) + word(ts) + word(ts) + word(one)
}

func bigInt256Hex(n *big.Int) string {
	hexStr := n.Text(16)
	for len(hexStr) < 64 {
		hexStr = "0" + hexStr
	}
	return "0x" + hexStr
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestChainlink_StaleRoundRejected pins: a round
// whose updatedAt exceeds the feed's MaxAge (relative to the
// comparison's observedAt) must surface as ErrPriceUnavailable —
// "reference unavailable", never a fresh-looking price that can
// mask or fabricate divergence.
func TestChainlink_StaleRoundRejected(t *testing.T) {
	answer := big.NewInt(6_543_210_000_000)
	observedAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		maxAge    time.Duration
		updatedAt time.Time
		wantStale bool
	}{
		{"fresh crypto round passes", 0 /* default 3h */, observedAt.Add(-30 * time.Minute), false},
		{"crypto round beyond default 3h rejected", 0, observedAt.Add(-4 * time.Hour), true},
		{"fx weekend round passes under 76h", 76 * time.Hour, observedAt.Add(-70 * time.Hour), false},
		{"frozen feed beyond explicit MaxAge rejected", 76 * time.Hour, observedAt.Add(-80 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeChainlinkRPC(t, roundDataHex(answer, tc.updatedAt))
			ref := NewChainlinkReference(ChainlinkOptions{
				RPCURL: srv.URL,
				FeedMap: map[string]ChainlinkFeed{
					"native/fiat:USD": {
						Address:  "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c",
						Decimals: 8,
						MaxAge:   tc.maxAge,
					},
				},
			})
			pair := mustPair(t, "native", "fiat:USD")
			_, err := priceOf(ref.LookupQuote(context.Background(), pair, observedAt))
			if tc.wantStale {
				if !errors.Is(err, ErrPriceUnavailable) {
					t.Fatalf("want ErrPriceUnavailable for stale round, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fresh round rejected: %v", err)
			}
		})
	}
}

// TestChainlink_LegacyAnswerShapeFailsLoudly — a proxy answering the
// 32-byte latestAnswer shape must error, not decode garbage.
func TestChainlink_LegacyAnswerShapeFailsLoudly(t *testing.T) {
	srv := fakeChainlinkRPC(t, bigInt256Hex(big.NewInt(6_543_210_000_000)))
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: srv.URL,
		FeedMap: map[string]ChainlinkFeed{
			"native/fiat:USD": {Address: "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c", Decimals: 8},
		},
	})
	_, err := priceOf(ref.LookupQuote(context.Background(), mustPair(t, "native", "fiat:USD"), time.Now()))
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("want too-short decode error for legacy 32-byte result, got %v", err)
	}
}

// TestChainlink_OmittedMaxAgeKeepsFXBudget pins that an operator feed
// entry omitting MaxAge does not silently swap an FX feed's 76h
// weekend budget for the 3h crypto one: a built-in key inherits the
// built-in budget and any other fiat/fiat key gets the FX default. A
// 70h-old round is a normal Sunday-night FX read, not a dead feed.
func TestChainlink_OmittedMaxAgeKeepsFXBudget(t *testing.T) {
	answer := big.NewInt(1_27000000)
	observedAt := time.Date(2026, 7, 5, 22, 0, 0, 0, time.UTC)
	cases := []struct {
		name, key, base, quote string
		updatedAt              time.Time
		wantStale              bool
	}{
		{"built-in FX key pasted from example.toml", "fiat:GBP/fiat:USD", "fiat:GBP", "fiat:USD", observedAt.Add(-70 * time.Hour), false},
		{"operator-added FX key", "fiat:CHF/fiat:USD", "fiat:CHF", "fiat:USD", observedAt.Add(-70 * time.Hour), false},
		{"FX key still bounded by 76h", "fiat:GBP/fiat:USD", "fiat:GBP", "fiat:USD", observedAt.Add(-80 * time.Hour), true},
		{"built-in crypto key keeps 3h", "crypto:BTC/fiat:USD", "crypto:BTC", "fiat:USD", observedAt.Add(-4 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeChainlinkRPC(t, roundDataHex(answer, tc.updatedAt))
			ref := NewChainlinkReference(ChainlinkOptions{
				RPCURL: srv.URL,
				FeedMap: map[string]ChainlinkFeed{
					tc.key: {Address: "0x5c0Ab2d9b5a7ed9f470386e82BB36A3613cDd4b5", Decimals: 8},
				},
			})
			_, err := ref.LookupQuote(context.Background(), mustPair(t, tc.base, tc.quote), observedAt)
			if tc.wantStale {
				if !errors.Is(err, ErrPriceUnavailable) {
					t.Fatalf("want ErrPriceUnavailable for a round beyond budget, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("round within the FX budget rejected: %v", err)
			}
		})
	}
}

// chainlinkFakeRPC is a selector-dispatching JSON-RPC fake for the
// on-chain decimals() verification tests: latestRoundData() answers
// roundHex; decimals() answers the current decimalsValue as a 32-byte
// word, or the configured HTTP status when decimalsStatus != 200. Both
// call kinds are counted so the cadence tests can assert how often the
// reference actually re-reads the chain.
type chainlinkFakeRPC struct {
	srv            *httptest.Server
	roundHex       string
	decimalsValue  atomic.Int64
	decimalsStatus atomic.Int64
	decimalsCalls  atomic.Int64
	roundCalls     atomic.Int64
}

func newChainlinkFakeRPC(t *testing.T, roundHex string, decimals int, decimalsStatus int) *chainlinkFakeRPC {
	t.Helper()
	f := &chainlinkFakeRPC{roundHex: roundHex}
	f.decimalsValue.Store(int64(decimals))
	f.decimalsStatus.Store(int64(decimalsStatus))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ethCallData(r) == chainlinkDecimalsSelector {
			f.decimalsCalls.Add(1)
			if st := int(f.decimalsStatus.Load()); st != http.StatusOK {
				w.WriteHeader(st)
				_, _ = w.Write([]byte(`{"error":{"code":-32603,"message":"internal"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + bigInt256Hex(big.NewInt(f.decimalsValue.Load())) + `"}`))
			return
		}
		f.roundCalls.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + f.roundHex + `"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeClock is a mutex-guarded injectable clock for the refresh/retry
// cadence tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newDecimalsTestRef builds a reference against the fake with one feed
// for pair, the given configured decimals, an injected clock and a
// captured logger.
func newDecimalsTestRef(t *testing.T, f *chainlinkFakeRPC, pairKey, address string, configured int) (*ChainlinkReference, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	clock := &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: f.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		FeedMap: map[string]ChainlinkFeed{
			pairKey: {Address: address, Decimals: configured, MaxAge: 76 * time.Hour},
		},
	})
	ref.now = clock.Now
	return ref, clock, &logBuf
}

// TestChainlink_Decimals_AbsentAdoptsOnChain — an operator entry with
// no decimals adopts the contract's decimals(). The fake feed publishes
// at 18 decimals with an answer of 1.08 × 10^18; a constructor that
// coerced the absent value to 8 and would have served 1.08 × 10^10.
func TestChainlink_Decimals_AbsentAdoptsOnChain(t *testing.T) {
	t.Parallel()
	answer, _ := new(big.Int).SetString("1080000000000000000", 10) // 1.08 × 10^18
	f := newChainlinkFakeRPC(t, roundDataHex(answer, time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 18, http.StatusOK)
	ref, _, _ := newDecimalsTestRef(t, f, "fiat:EUR/fiat:USD", "0x00000000000000000000000000000000000000e1", 0)
	pair := mustPair(t, "fiat:EUR", "fiat:USD")

	got, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if want := 1.08; abs(got-want) > 1e-9 {
		t.Fatalf("price = %.12g, want %.12g — absent config must scale by the on-chain 18, not a default 8", got, want)
	}
	// Second read within the refresh window: served from the verified
	// state, no second decimals() round-trip.
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); err != nil {
		t.Fatalf("second LookupQuote: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times across two lookups, want 1 (verified value is cached)", n)
	}
}

// TestChainlink_Decimals_EqualFlows — configured 8, chain 8: readings
// flow and nothing is counted as a mismatch.
func TestChainlink_Decimals_EqualFlows(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "fiat:GBP", "fiat:USD")
	before := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String()))
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(127_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)
	ref, _, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e2", 8)

	got, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if want := 1.27; abs(got-want) > 1e-9 {
		t.Errorf("price = %.12g, want %.12g", got, want)
	}
	if after := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String())); after != before {
		t.Errorf("mismatch counter moved %v → %v on an agreeing feed", before, after)
	}
}

// TestChainlink_Decimals_MismatchRefusedAndCounted — configured 8 but
// the chain says 18: every reading is refused as ErrPriceUnavailable +
// ErrChainlinkDecimalsMismatch, each refusal increments the counter, the
// price is never even read, decimals() is re-read only after the retry
// interval, and readings resume once config and chain agree.
func TestChainlink_Decimals_MismatchRefusedAndCounted(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:LINK", "fiat:USD")
	counter := obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String())
	before := testutil.ToFloat64(counter)
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(1_500_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 18, http.StatusOK)
	ref, clock, logBuf := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e3", 8)

	for i := 1; i <= 2; i++ {
		_, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
		if !errors.Is(err, ErrPriceUnavailable) {
			t.Fatalf("lookup %d: err = %v, want ErrPriceUnavailable (Compare must classify the refusal as price_unavailable)", i, err)
		}
		if !errors.Is(err, ErrChainlinkDecimalsMismatch) {
			t.Fatalf("lookup %d: err = %v, want ErrChainlinkDecimalsMismatch", i, err)
		}
		if !strings.Contains(err.Error(), "configured decimals=8") || !strings.Contains(err.Error(), "decimals()=18") {
			t.Errorf("lookup %d: error %q must name both values", i, err)
		}
	}
	if got := testutil.ToFloat64(counter) - before; got != 2 {
		t.Errorf("mismatch counter advanced by %v across two refused lookups, want 2", got)
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times while refused, want 0 — refuse before reading a price", n)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times within the retry window, want 1", n)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "decimals mismatch") {
		t.Errorf("no ERROR mismatch line logged; got:\n%s", logBuf.String())
	}

	// Past the retry interval the chain is re-read; still 18 → still refused.
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); !errors.Is(err, ErrChainlinkDecimalsMismatch) {
		t.Fatalf("after retry interval: err = %v, want ErrChainlinkDecimalsMismatch", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}

	// Chain and config agree again → readings resume at the agreed scale.
	f.decimalsValue.Store(8)
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	got, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
	if err != nil {
		t.Fatalf("after agreement: %v", err)
	}
	if want := 15.0; abs(got-want) > 1e-9 {
		t.Errorf("price after agreement = %.12g, want %.12g", got, want)
	}
}

// TestChainlink_Decimals_MismatchDoesNotBleedAcrossPairsSharingAddress
// — two canonical pairs mapped to the SAME feed address, one whose
// configured decimals disagrees with the chain and one that agrees
// (asserts none): the verification state is keyed by address only, so
// it must never let the first pair's refusal leak into the second
// pair's verdict on a cache-hit call within the same retry window.
func TestChainlink_Decimals_MismatchDoesNotBleedAcrossPairsSharingAddress(t *testing.T) {
	t.Parallel()
	const address = "0x00000000000000000000000000000000000000e7"
	answer := big.NewInt(1_500_000_000)
	f := newChainlinkFakeRPC(t, roundDataHex(answer, time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)

	// Distinct codes from every other decimals test in this file — the
	// mismatch counter is a global Prometheus metric keyed by pair
	// string, so reusing a pair here would race with another test's
	// before/after counter snapshot.
	mismatchPair := mustPair(t, "crypto:SOL", "fiat:USD")
	okPair := mustPair(t, "crypto:AVAX", "fiat:USD")

	var logBuf bytes.Buffer
	clock := &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: f.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		FeedMap: map[string]ChainlinkFeed{
			// Configured 6 vs on-chain 8: refused.
			mismatchPair.String(): {Address: address, Decimals: 6, MaxAge: 76 * time.Hour},
			// No configured value: adopts on-chain 8, must NOT be
			// refused just because it shares an address with the
			// mismatching pair above.
			okPair.String(): {Address: address, Decimals: 0, MaxAge: 76 * time.Hour},
		},
	})
	ref.now = clock.Now

	// First call establishes the shared per-address state as a
	// mismatch (and caches it for chainlinkDecimalsRetryInterval).
	if _, err := priceOf(ref.LookupQuote(context.Background(), mismatchPair, ref.now())); !errors.Is(err, ErrChainlinkDecimalsMismatch) {
		t.Fatalf("mismatchPair: err = %v, want ErrChainlinkDecimalsMismatch", err)
	}

	// Second call, different pair, same address, well within the
	// retry window (cache hit path in resolveDecimals): must adopt
	// the on-chain 8 and succeed, not inherit the other pair's
	// mismatch verdict.
	got, err := priceOf(ref.LookupQuote(context.Background(), okPair, ref.now()))
	if err != nil {
		t.Fatalf("okPair: LookupQuote = %v, want success — its own config (none asserted) agrees with on-chain 8, "+
			"a sibling pair's mismatch on the same feed address must not bleed into this verdict", err)
	}
	if want := 15.0; abs(got-want) > 1e-9 {
		t.Errorf("okPair: price = %.12g, want %.12g (scaled by on-chain 8)", got, want)
	}
}

// TestChainlink_Decimals_RPCErrorKeepsConfiguredWithRetry — decimals()
// fails (500) but latestRoundData() works: the configured value is kept
// with a WARN, the fail-open counter advances so the failure is
// alertable, the read is retried only after the retry interval, and
// nothing crashes.
func TestChainlink_Decimals_RPCErrorKeepsConfiguredWithRetry(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "fiat:JPY", "fiat:USD")
	failCounter := obs.ChainlinkFeedDecimalsVerifyFailedTotal.WithLabelValues("divergence", pair.String())
	before := testutil.ToFloat64(failCounter)
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(670_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusInternalServerError)
	ref, clock, logBuf := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e4", 8)

	got, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
	if err != nil {
		t.Fatalf("LookupQuote with failing decimals(): %v — configured value must be kept", err)
	}
	if want := 0.0067; abs(got-want) > 1e-12 {
		t.Errorf("price = %.12g, want %.12g at the configured 8", got, want)
	}
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "decimals() read failed") {
		t.Errorf("no WARN line for the failed decimals() read; got:\n%s", logBuf.String())
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v after one failed decimals() read, want 1", got)
	}

	// Within the retry window: no re-read, counter unmoved.
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); err != nil {
		t.Fatalf("second LookupQuote: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times inside the retry window, want 1", n)
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v inside the retry window, want 1 (no re-read)", got)
	}
	// Past it: retried, and once the chain answers it is verified equal.
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	f.decimalsStatus.Store(http.StatusOK)
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); err != nil {
		t.Fatalf("LookupQuote after retry: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v once the retry succeeded, want 1 (no further failure)", got)
	}
}

// TestChainlink_Decimals_RPCErrorWithoutConfigRefuses — no configured
// value AND decimals() unavailable: there is no scale to serve at, so
// the feed is refused (never guessed), as ErrPriceUnavailable.
func TestChainlink_Decimals_RPCErrorWithoutConfigRefuses(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:ETH", "fiat:USD")
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(250_000_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusInternalServerError)
	ref, _, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e5", 0)

	_, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now()))
	if !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("err = %v, want ErrPriceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "decimals unresolved") {
		t.Errorf("error %q should say the decimals are unresolved", err)
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times with no known scale, want 0", n)
	}
}

// TestChainlink_Decimals_RefreshedDaily — a verified value is re-read
// after the refresh interval, so a proxy re-pointed at a differently
// scaled aggregator is caught within a day.
func TestChainlink_Decimals_RefreshedDaily(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:BTC", "fiat:USD")
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(6_543_210_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)
	ref, clock, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e6", 8)

	for i := 0; i < 3; i++ {
		if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); err != nil {
			t.Fatalf("LookupQuote %d: %v", i, err)
		}
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Fatalf("decimals() called %d times inside the refresh window, want 1", n)
	}
	// Staleness is measured against observedAt, which the clock also
	// drives: 25h is inside the feed's 76h MaxAge, so the round still
	// reads fresh after the jump.
	clock.Advance(chainlinkDecimalsRefreshInterval + time.Second)
	if _, err := priceOf(ref.LookupQuote(context.Background(), pair, ref.now())); err != nil {
		t.Fatalf("LookupQuote after refresh interval: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the refresh interval, want 2", n)
	}
}

// TestDecodeChainlinkDecimals pins the uint8 word decode and its
// fail-loud shape checks.
func TestDecodeChainlinkDecimals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr string
	}{
		{"eight", bigInt256Hex(big.NewInt(8)), 8, ""},
		{"eighteen", bigInt256Hex(big.NewInt(18)), 18, ""},
		{"max uint8", bigInt256Hex(big.NewInt(255)), 255, ""},
		{"zero is a broken contract", bigInt256Hex(big.NewInt(0)), 0, "outside uint8 range"},
		{"256 overflows uint8", bigInt256Hex(big.NewInt(256)), 0, "outside uint8 range"},
		{"round-data shape is not a word", roundDataHex(big.NewInt(1), time.Unix(1, 0)), 0, "want 32 bytes"},
		{"empty", "0x", 0, "want 32 bytes"},
		{"not hex", "0xzz", 0, "decimals hex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeChainlinkDecimals(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// chainlinkSecretPath is the key-bearing part of a keyed RPC endpoint.
// Keyed providers (Alchemy, Infura, QuickNode) put the API key in the
// URL PATH, which is why config.go documents the whole rpc_url as a
// secret. Deliberately an obviously-fake, low-entropy string.
const chainlinkSecretPath = "/v2/not-a-real-key-ns12" // gitleaks:allow

// hangUpRPC returns a server that accepts the connection and then
// closes it without answering, so the real http.Client produces a real
// *url.Error — the exact shape a timeout / TLS failure / dropped
// connection against the operator's RPC endpoint produces in
// production. Hijacking (rather than pointing at a closed port) keeps
// the test hermetic: no reliance on a port staying unbound.
func hangUpRPC(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestChainlink_TransportError_RedactsKeyedEndpoint — NS12.
//
// r.rpcURL is a secret (same CHAINLINK_RPC_URL as the ingest poller;
// keyed providers embed the API key in the path). A transport failure
// yields a *url.Error whose Error() quotes the full request URL, so
// wrapping it raw put the operator's API key into the error string that
// Compare copies verbatim into Result.Failures. The error must name the
// endpoint host — that is the operator's whole diagnostic — with the
// path redacted.
func TestChainlink_TransportError_RedactsKeyedEndpoint(t *testing.T) {
	srv := hangUpRPC(t)
	ref := NewChainlinkReference(ChainlinkOptions{
		HTTPClient: srv.Client(),
		RPCURL:     srv.URL + chainlinkSecretPath,
	})

	_, err := priceOf(ref.LookupQuote(context.Background(), mustPair(t, "crypto:BTC", "fiat:USD"), time.Now()))
	if err == nil {
		t.Fatal("LookupQuote against a hung-up RPC: want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, chainlinkSecretPath) {
		t.Errorf("transport error leaks the keyed endpoint path: %s", msg)
	}
	if want := srv.URL + "/<redacted>"; !strings.Contains(msg, want) {
		t.Errorf("transport error = %q, want it to name the redacted endpoint %q", msg, want)
	}
	if !strings.Contains(msg, "chainlink: rpc transport: ") {
		t.Errorf("transport error = %q, want the rpc-transport prefix preserved", msg)
	}
}

// TestChainlink_RPCRedirectNotFollowed: the keyed endpoint URL would be
// re-sent as the next hop's Referer, so a 307 must end the call there,
// for the default client and an injected one alike.
func TestChainlink_RPCRedirectNotFollowed(t *testing.T) {
	var hops atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hops.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rpc", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	for name, hc := range map[string]*http.Client{"default client": nil, "injected client": origin.Client()} {
		t.Run(name, func(t *testing.T) {
			hops.Store(0)
			ref := NewChainlinkReference(ChainlinkOptions{HTTPClient: hc, RPCURL: origin.URL + chainlinkSecretPath})
			_, err := priceOf(ref.LookupQuote(context.Background(), mustPair(t, "crypto:BTC", "fiat:USD"), time.Now()))
			if n := hops.Load(); n != 0 {
				t.Fatalf("redirect target received %d request(s); want 0", n)
			}
			if err == nil || !strings.Contains(err.Error(), externalchainlink.ErrRedirectRefused.Error()) {
				t.Fatalf("LookupQuote err = %v; want %q", err, externalchainlink.ErrRedirectRefused)
			}
			if strings.Contains(err.Error(), chainlinkSecretPath) {
				t.Errorf("refusal error leaks the keyed endpoint path: %s", err)
			}
		})
	}
}

// slowRPC returns a server that sleeps past any short context deadline
// before answering, so a request against it produces a real
// context.DeadlineExceeded wrapped in the *url.Error http.Client.Do
// returns — the exact shape an RPC-timeout classification needs to
// see through the redactor.
func slowRPC(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestChainlink_TransportTimeout_ClassifiesAsTimeout_NotError — wrapping
// RedactURLError with "%s" (chainlink.go's ethCall) stringifies the
// error and discards the chain, so errors.Is matches nothing and a
// genuine RPC timeout would fall into errorOutcome's default case (OutcomeError) instead
// of OutcomeTimeout. The redacted error must still unwrap to
// context.DeadlineExceeded while never rendering the keyed endpoint.
func TestChainlink_TransportTimeout_ClassifiesAsTimeout_NotError(t *testing.T) {
	srv := slowRPC(t, 200*time.Millisecond)
	ref := NewChainlinkReference(ChainlinkOptions{
		HTTPClient: srv.Client(),
		RPCURL:     srv.URL + chainlinkSecretPath,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := priceOf(ref.LookupQuote(ctx, mustPair(t, "crypto:BTC", "fiat:USD"), time.Now()))
	if err == nil {
		t.Fatal("LookupQuote against a slow RPC past its context deadline: want error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded) to hold through the redactor", err)
	}
	if got := errorOutcome(err); got != OutcomeTimeout {
		t.Fatalf("errorOutcome(err) = %q, want %q", got, OutcomeTimeout)
	}
	if msg := err.Error(); strings.Contains(msg, chainlinkSecretPath) {
		t.Errorf("timeout error leaks the keyed endpoint path: %s", msg)
	}
}

// TestChainlink_BadRequestURL_RedactsKeyedEndpoint — NS12, second URL
// -bearing path in the same function: http.NewRequestWithContext runs
// url.Parse, and its failure is itself a *url.Error carrying the raw
// (secret) URL.
func TestChainlink_BadRequestURL_RedactsKeyedEndpoint(t *testing.T) {
	// A control character makes url.Parse fail without changing what
	// the operator configured in any other way.
	const badURL = "https://eth-mainnet.example.test" + chainlinkSecretPath + "\n"
	ref := NewChainlinkReference(ChainlinkOptions{RPCURL: badURL})

	_, err := priceOf(ref.LookupQuote(context.Background(), mustPair(t, "crypto:BTC", "fiat:USD"), time.Now()))
	if err == nil {
		t.Fatal("LookupQuote with an unparseable rpc_url: want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, chainlinkSecretPath) {
		t.Errorf("request-build error leaks the keyed endpoint path: %s", msg)
	}
	if !strings.Contains(msg, "chainlink: new request: ") {
		t.Errorf("request-build error = %q, want the new-request prefix preserved", msg)
	}
}

// TestChainlink_TransportError_SecretNeverReachesDivergenceCache —
// NS12 end to end, through the production entry point.
//
// This is the artifact the finding is about: RefreshPair's CachedResult
// is JSON-marshalled into the per-pair divergence key in Redis, which
// ha-plan.md documents as a single internal-bind, no-AUTH instance.
// Every refresh cycle re-writes it while the endpoint keeps erroring,
// so an ordinary timeout parked the operator's API key at rest with no
// attacker action at all. Assert on the raw cached bytes, not on the
// error, because the bytes are what an unauthenticated reader gets.
func TestChainlink_TransportError_SecretNeverReachesDivergenceCache(t *testing.T) {
	srv := hangUpRPC(t)
	ref := NewChainlinkReference(ChainlinkOptions{
		HTTPClient: srv.Client(),
		RPCURL:     srv.URL + chainlinkSecretPath,
	})

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc, err := NewService(ServiceOptions{
		References: []Reference{ref},
		Cache:      rdb,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	pair := mustPair(t, "crypto:BTC", "fiat:USD")
	ctx := context.Background()
	// Every reference failed, so RefreshPair reports the outage,
	// but it still writes the cache entry, which is the point of
	// this test.
	if err := svc.RefreshPair(ctx, pair, 65_000, time.Now()); err != nil && !errors.Is(err, ErrNoReferenceResponded) {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(ctx, cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	if strings.Contains(string(body), chainlinkSecretPath) {
		t.Fatalf("divergence cache entry carries the keyed rpc endpoint at rest: %s", body)
	}
	// Non-vacuous: the failure label must still be there (and name the
	// redacted endpoint), so this is not passing because the cache is
	// empty or the failure was swallowed. Decoded, because the encoder
	// escapes the angle brackets of "<redacted>" on the wire.
	var cached CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal cached result: %v", err)
	}
	label := cached.Failures[ChainlinkSourceName]
	if !strings.HasPrefix(label, "chainlink: rpc transport: ") {
		t.Fatalf("cached chainlink failure label = %q, want the rpc-transport prefix", label)
	}
	if want := srv.URL + "/<redacted>"; !strings.Contains(label, want) {
		t.Fatalf("cached chainlink failure label = %q, want the redacted endpoint %q", label, want)
	}
}
