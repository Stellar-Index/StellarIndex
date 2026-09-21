package v1_test

import (
	"net/http"
	"slices"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestListingsWithholdScamFlaggedLastPrice pins the listing surfaces to
// the same withholding decision /v1/price, /v1/vwap and /v1/twap reach:
// a market with a directory-flagged issuer on EITHER leg still lists
// (activity is not a price) but ships no last_price. Before the gate,
// /v1/markets, /v1/pools and /v1/pairs each served the flagged market's
// last_price verbatim — the number the detail endpoints had just
// withheld, one listing page away.
func TestListingsWithholdScamFlaggedLastPrice(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	flaggedPrice, quoteLegPrice, cleanPrice := "1.5", "0.25", "2.5"
	rows := []v1.Market{
		{Base: fallbackFlaggedAsset, Quote: "native", LastPrice: &flaggedPrice, TradeCount24h: 3},
		{Base: "native", Quote: fallbackFlaggedAsset, LastPrice: &quoteLegPrice, TradeCount24h: 4},
		{Base: "native", Quote: usdc, LastPrice: &cleanPrice, TradeCount24h: 5},
	}
	gate := &fallbackScamGate{withheld: map[string]bool{fallbackFlaggedAsset: true}}
	reader := &stubMarketsReader{pairs: rows, pair: rows[0], pairFound: true}
	srv := v1.New(v1.Options{Markets: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	check := func(t *testing.T, path string, wantRows int) {
		t.Helper()
		resp := mustGet(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		var env struct {
			Data []v1.Market `json:"data"`
		}
		mustDecode(t, resp, &env)
		if len(env.Data) != wantRows {
			t.Fatalf("%s: got %d rows, want %d — withholding hides the price, never the market", path, len(env.Data), wantRows)
		}
		for _, m := range env.Data {
			flagged := m.Base == fallbackFlaggedAsset || m.Quote == fallbackFlaggedAsset
			switch {
			case flagged && m.LastPrice != nil:
				t.Errorf("%s: %s/%s last_price = %q, want withheld (null) — issuer is directory-flagged", path, m.Base, m.Quote, *m.LastPrice)
			case !flagged && (m.LastPrice == nil || *m.LastPrice != cleanPrice):
				t.Errorf("%s: %s/%s last_price = %v, want %q — an unflagged market must keep its price", path, m.Base, m.Quote, m.LastPrice, cleanPrice)
			}
			if m.TradeCount24h == 0 {
				t.Errorf("%s: %s/%s trade_count_24h = 0 — activity figures are not prices and must survive", path, m.Base, m.Quote)
			}
		}
	}
	t.Run("markets", func(t *testing.T) { check(t, "/v1/markets", 3) })
	t.Run("pools", func(t *testing.T) { check(t, "/v1/pools", 3) })
	t.Run("pairs", func(t *testing.T) {
		check(t, "/v1/pairs?base="+fallbackFlaggedAsset+"&quote=native", 1)
	})
	for _, surface := range []string{"markets", "pools", "pairs"} {
		if !slices.Contains(gate.surfaces, surface) {
			t.Errorf("scam gate was never consulted with surface %q (asked: %v)", surface, gate.surfaces)
		}
	}
}
