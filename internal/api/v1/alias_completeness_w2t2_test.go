package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// w2t2 batch: the four api-v1 pair/rate reads that were NOT walking the
// XLM dual-form alias loop the price/OHLC series paths already run, so a
// valid-but-aliased input (?base=crypto:XLM against native-keyed depth,
// and vice-versa) 404'd / undercounted while the sibling form served.
// Each test stores data under ONE alias form and queries the OTHER;
// pre-fix each is RED (empty / not-found), post-fix GREEN (served value).

const w2t2USDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// ── target 1: /v1/vwap (tradesInRangeWithStablecoinFallback non-fiat) ──

// TestVWAP_NonFiatQuoteAliasFirstHit — depth lives under native/<USDC>;
// a ?base=crypto:XLM query must resolve it via the alias loop instead of
// 404ing on the literal crypto:XLM/<USDC> pair.
func TestVWAP_NonFiatQuoteAliasFirstHit(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	native, _ := canonical.ParseAsset("native")
	nativePair, _ := canonical.NewPair(native, usdc)

	// One trade at 16/100 = 0.16, keyed under the native form only.
	trade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        nativePair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {trade},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// Aliased input: base=crypto:XLM must fall through to native/<USDC>.
	resp := mustGet(t, ts.URL+"/v1/vwap?base=crypto:XLM&quote="+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aliased base status = %d, want 200 (alias loop must serve native-keyed depth)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1600000000"`) {
		t.Errorf("body missing served VWAP 0.16: %s", body)
	}

	// Primary form unchanged: base=native still serves the same rate.
	respNative := mustGet(t, ts.URL+"/v1/vwap?base=native&quote="+usdc.String())
	if respNative.StatusCode != http.StatusOK {
		t.Fatalf("primary base=native status = %d, want 200", respNative.StatusCode)
	}
	bodyNative, _ := readAll(respNative)
	if !strings.Contains(bodyNative, `"price":"0.1600000000"`) {
		t.Errorf("primary form body missing VWAP 0.16: %s", bodyNative)
	}
}

// ── target 2: /v1/history/since-inception (HistoryPoints) ──

// TestHistorySinceInception_AssetAliasFirstHit — the series is keyed
// under crypto:XLM/<USDC>; a ?asset=native query must resolve it via the
// alias loop instead of returning an empty points array.
func TestHistorySinceInception_AssetAliasFirstHit(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	cryptoPair, _ := canonical.NewPair(cryptoXLM, usdc)

	reader := &stubHistoryReader{
		pointsByPair: map[string][]v1.HistoryPoint{
			cryptoPair.String(): {{
				Bucket: time.Unix(1_772_000_000, 0).UTC(),
				VWAP:   "0.1600000000",
			}},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+usdc.String()+"&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"0.1600000000"`) {
		t.Errorf("body missing crypto:XLM-keyed history point 0.16 (alias loop should surface it): %s", body)
	}
}

// ── target 3: /v1/pairs (PairMarket) ──

type pairKeyedMarketsReader struct {
	stubMarketsReader
	byPair map[string]v1.Market
}

func (r *pairKeyedMarketsReader) PairMarket(_ context.Context, base, quote canonical.Asset) (v1.Market, bool, error) {
	m, ok := r.byPair[base.String()+"/"+quote.String()]
	return m, ok, nil
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

// target 4 (/v1/markets?asset=) is fixed one layer down, in
// internal/storage/timescale distinctPairsCommon's ANY-membership binding
// — the api handler's AssetMarkets call signature is deliberately
// unchanged (so TestMarkets_AssetFilterIsCanonicalised still sees the
// single canonical spelling). See TestBuildDistinctPairsQuery_AssetFilter*
// in internal/storage/timescale/markets_asset_alias_test.go.
