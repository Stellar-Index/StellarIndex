package v1_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestPairs_MissingBase400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPairs_MissingQuote400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPairs_InvalidAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	for _, q := range []string{
		"/v1/pairs?base=garbage&quote=fiat:USD",
		"/v1/pairs?base=native&quote=garbage",
	} {
		resp := mustGet(t, ts.URL+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestPairs_IdentityPair400(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPairs_EmptyWhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !bytes.Contains([]byte(body), []byte(`"data":[]`)) {
		t.Errorf("expected \"data\":[], got: %s", body)
	}
}

func TestPairs_NotFoundReturnsEmptyArray(t *testing.T) {
	// A pair with no trades returns 200 + empty data — NOT a 404.
	// Spec: PairsEnvelope.data is `type: array`; "no data" is an
	// empty array, not a 404, so clients can distinguish "no such
	// pair" from a malformed request without branching on status.
	reader := &stubMarketsReader{pairFound: false}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !bytes.Contains([]byte(body), []byte(`"data":[]`)) {
		t.Errorf("expected \"data\":[], got: %s", body)
	}
}

func TestPairs_FoundReturnsSingleElement(t *testing.T) {
	ts1 := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubMarketsReader{
		pair:      v1.Market{Base: "native", Quote: "fiat:USD", LastTradeAt: v1.WireTime(ts1), TradeCount24h: 99},
		pairFound: true,
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Market `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d markets, want 1", len(env.Data))
	}
	if env.Data[0].TradeCount24h != 99 {
		t.Errorf("trade_count_24h = %d, want 99", env.Data[0].TradeCount24h)
	}
	if env.Data[0].Base != "native" || env.Data[0].Quote != "fiat:USD" {
		t.Errorf("pair = %s/%s, want native/fiat:USD", env.Data[0].Base, env.Data[0].Quote)
	}
}

func TestPairs_ReaderError500(t *testing.T) {
	reader := &stubMarketsReader{pairErr: errors.New("boom")}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestPairs_AliasFirstHit — the market row lives under native/<USDC>; a
// ?base=crypto:XLM query must resolve it via the alias loop rather than
// returning an empty list.
func TestPairs_AliasFirstHit(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	lp := "0.1600000000"
	reader := &pairKeyedMarketsReader{
		byPair: map[string]v1.Market{
			"native/" + usdc.String(): {
				Base:          "native",
				Quote:         usdc.String(),
				TradeCount24h: 7,
				LastPrice:     &lp,
			},
		},
	}
	srv := v1.New(v1.Options{Markets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/pairs?base=crypto:XLM&quote="+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Market `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("want 1 market via alias loop, got %d", len(env.Data))
	}
	if env.Data[0].Base != "native" || env.Data[0].TradeCount24h != 7 {
		t.Errorf("unexpected market row: %+v", env.Data[0])
	}
}

// TestPairs_NonstandardDecimals_NormalizesLastPrice — /v1/pairs shares
// the same Market wire shape; PairMarket's last_price is the same raw
// prices_1m ratio.
func TestPairs_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	reader := &stubMarketsReader{
		pair:      v1.Market{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
		pairFound: true,
	}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/pairs?base="+flaggedAsset+"&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged pair row not normalized: %s", body)
	}
}

const w2t2USDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

type pairKeyedMarketsReader struct {
	stubMarketsReader
	byPair map[string]v1.Market
}

func (r *pairKeyedMarketsReader) PairMarket(_ context.Context, base, quote canonical.Asset) (v1.Market, bool, error) {
	m, ok := r.byPair[base.String()+"/"+quote.String()]
	return m, ok, nil
}
