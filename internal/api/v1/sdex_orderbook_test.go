package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const obUSDC = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

type stubSDEXOfferBookReader struct {
	offers []clickhouse.LiveOffer
	cursor uint32
}

func (s *stubSDEXOfferBookReader) LoadLiveOffers(context.Context) ([]clickhouse.LiveOffer, uint32, error) {
	return s.offers, s.cursor, nil
}

func (s *stubSDEXOfferBookReader) OfferChangesSince(_ context.Context, from uint32) ([]clickhouse.OfferChange, uint32, error) {
	return nil, from, nil
}

func (s *stubSDEXOfferBookReader) OfferRemovedAt(context.Context, []clickhouse.OfferRemovalRef) (map[string]struct{}, error) {
	return nil, nil
}

func orderBookServer(t *testing.T, load bool) *testServer {
	t.Helper()
	cache := v1.NewSDEXOrderBookCache(&stubSDEXOfferBookReader{
		offers: []clickhouse.LiveOffer{
			// Versions carry nonzero intra_ledger_seq — intra 0 would put
			// an offer in the version-tie quarantine (unserved until the
			// lake removal probe clears it; see the internal tests).
			// Ask: sell 10 XLM at 0.5 USDC/XLM.
			{KeyXDR: "k1", OfferID: 1, Amount: 100_000_000, Selling: "native", Buying: obUSDC, PriceN: 1, PriceD: 2, Version: 5<<32 | 1},
			// Bid: sell 6 USDC at 2 XLM/USDC → 0.5 USDC/XLM.
			{KeyXDR: "k2", OfferID: 2, Amount: 60_000_000, Selling: obUSDC, Buying: "native", PriceN: 2, PriceD: 1, Version: 5<<32 | 2},
			// Unrelated pair — must not leak into the requested book.
			{KeyXDR: "k3", OfferID: 3, Amount: 70_000_000, Selling: "native", Buying: "EURC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ", PriceN: 1, PriceD: 1, Version: 5<<32 | 3},
		},
		cursor: 63_400_000,
	}, nil)
	if load {
		if err := cache.Load(context.Background()); err != nil {
			t.Fatalf("cache load: %v", err)
		}
	}
	return httpTestServer(t, v1.New(v1.Options{SDEXOrderBook: cache}))
}

func TestSDEXOrderbook_Happy(t *testing.T) {
	ts := orderBookServer(t, true)

	resp := mustGet(t, ts.URL+"/v1/sdex/orderbook?selling=native&buying="+obUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=30" {
		t.Errorf("Cache-Control = %q, want public, max-age=30", cc)
	}

	var env struct {
		Data v1.SDEXOrderBookView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.Selling != "native" || d.Buying != obUSDC || d.AsOfLedger != 63_400_000 {
		t.Errorf("book identity wrong: %+v", d)
	}
	if d.AskOffers != 1 || d.BidOffers != 1 || len(d.Asks) != 1 || len(d.Bids) != 1 {
		t.Fatalf("sides = %d/%d asks/bids (levels %d/%d), want 1 each — the EURC offer must not leak in",
			d.AskOffers, d.BidOffers, len(d.Asks), len(d.Bids))
	}
	ask, bid := d.Asks[0], d.Bids[0]
	if ask.Price != "0.5000000" || ask.BaseAmount != "10.0000000" || ask.QuoteAmount != "5.0000000" {
		t.Errorf("ask = %+v, want 10 XLM at 0.5 (5 USDC)", ask)
	}
	// Bid offer sells 6 USDC at 2 XLM per USDC → level price 1/2
	// USDC-per-XLM, base 12 XLM, quote 6 USDC.
	if bid.Price != "0.5000000" || bid.PriceR.N != 1 || bid.PriceR.D != 2 {
		t.Errorf("bid price = %+v, want 0.5 (1/2)", bid)
	}
	if bid.BaseAmount != "12.0000000" || bid.QuoteAmount != "6.0000000" {
		t.Errorf("bid amounts = %s/%s, want 12/6", bid.BaseAmount, bid.QuoteAmount)
	}
	if d.Depth != 25 {
		t.Errorf("default depth = %d, want 25", d.Depth)
	}
}

func TestSDEXOrderbook_ProblemPaths(t *testing.T) {
	// Cold start: wired but not yet loaded → 503 problem, no-store.
	cold := orderBookServer(t, false)
	resp := mustGet(t, cold.URL+"/v1/sdex/orderbook?selling=native&buying="+obUSDC)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("cold status = %d, want 503", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cold Cache-Control = %q, want no-store (problems are never cacheable)", cc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("cold Content-Type = %q, want application/problem+json", ct)
	}

	// Not wired at all → 503.
	unwired := httpTestServer(t, v1.New(v1.Options{}))
	if resp := mustGet(t, unwired.URL+"/v1/sdex/orderbook?selling=native&buying="+obUSDC); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unwired status = %d, want 503", resp.StatusCode)
	}

	ts := orderBookServer(t, true)
	for name, q := range map[string]string{
		"missing selling":  "buying=" + obUSDC,
		"soroban asset":    "selling=CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA&buying=native",
		"same asset":       "selling=native&buying=native",
		"malformed asset":  "selling=not!an!asset&buying=native",
		"depth too large":  "selling=native&buying=" + obUSDC + "&depth=9999",
		"depth not an int": "selling=native&buying=" + obUSDC + "&depth=abc",
	} {
		resp := mustGet(t, ts.URL+"/v1/sdex/orderbook?"+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}
