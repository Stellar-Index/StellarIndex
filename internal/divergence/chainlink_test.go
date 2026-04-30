package divergence

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// fakeChainlinkRPC returns a server that responds to eth_call with
// the supplied 32-byte int256 answer (hex string with 0x prefix).
func fakeChainlinkRPC(t *testing.T, answerHex string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + answerHex + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
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

func TestChainlink_LookupPrice_HappyPath_BTC_USD(t *testing.T) {
	// Chainlink feed answer: 65,432.10 USD * 10^8 = 6,543,210,000,000.
	// Hex of 6543210000000 = 0x5F3115DBE80, padded to 32 bytes =
	// 0x0000000000000000000000000000000000000000000000000000005F3115DBE80
	// Wait — 6543210000000 = 0x5F3115DBE80 (44 bits). Pad to 64 hex chars.
	answer := big.NewInt(6_543_210_000_000)
	hexStr := bigInt256Hex(answer)

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
	got, err := ref.LookupPrice(context.Background(), pair, time.Now())
	if err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	want := 65432.10
	if abs(got-want) > 0.001 {
		t.Errorf("got %.4f, want %.4f", got, want)
	}
}

func TestChainlink_LookupPrice_Inverted(t *testing.T) {
	// Feed publishes EUR/USD = 1.08. Operator wants USD/EUR.
	// Raw answer: 108,000,000 (1.08 × 10^8). Inverted = 0.9259...
	answer := big.NewInt(108_000_000)
	hexStr := bigInt256Hex(answer)

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
	got, err := ref.LookupPrice(context.Background(), pair, time.Now())
	if err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	want := 1.0 / 1.08
	if abs(got-want) > 0.001 {
		t.Errorf("inverted got %.4f, want %.4f", got, want)
	}
}

func TestChainlink_LookupPrice_UnsupportedAsset(t *testing.T) {
	srv := fakeChainlinkRPC(t, "0x"+strings.Repeat("0", 64))
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL:  srv.URL,
		FeedMap: map[string]ChainlinkFeed{}, // empty
	})
	pair := mustPair(t, "native", "fiat:USD")
	_, err := ref.LookupPrice(context.Background(), pair, time.Now())
	if err == nil {
		t.Fatal("expected ErrAssetUnsupported")
	}
	if !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("err=%v not wrapping ErrAssetUnsupported", err)
	}
}

func TestChainlink_LookupPrice_RPCError(t *testing.T) {
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
	_, err := ref.LookupPrice(context.Background(), pair, time.Now())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "rpc status 500") {
		t.Errorf("err=%v missing 'rpc status 500'", err)
	}
}

func TestChainlink_DecodeInt256_Negative(t *testing.T) {
	// -1 in two's complement int256 = all ones (0xFFF...FF).
	hexStr := "0x" + strings.Repeat("f", 64)
	got, err := decodeChainlinkInt256(hexStr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Cmp(big.NewInt(-1)) != 0 {
		t.Errorf("decoded -1 as %s", got.String())
	}
}

func TestChainlink_DecodeInt256_Positive(t *testing.T) {
	// 100 = 0x64
	hexStr := "0x" + strings.Repeat("0", 62) + "64"
	got, err := decodeChainlinkInt256(hexStr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("decoded 100 as %s", got.String())
	}
}

func TestChainlink_ScaleAnswer(t *testing.T) {
	// 12_345_678 * 10^8 should scale to 0.12345678
	got, err := scaleChainlinkAnswer(big.NewInt(12_345_678), 8)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	want := 0.12345678
	if abs(got-want) > 1e-9 {
		t.Errorf("got %.10f, want %.10f", got, want)
	}
}

func TestChainlink_Name(t *testing.T) {
	r := NewChainlinkReference(ChainlinkOptions{})
	if r.Name() != "chainlink" {
		t.Errorf("Name=%q want chainlink", r.Name())
	}
}

// ─── Helpers ─────────────────────────────────────────────────────

// bigInt256Hex pads a positive big.Int to 32-byte 0x-prefixed hex.
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
