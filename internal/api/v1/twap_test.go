package v1_test

import (
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func mkTWAPTrade(base, quote int64, ts time.Time) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: uint32(ts.Unix()),
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

func TestTWAP_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestTWAP_404WhenNoTrades(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTWAP_TimeWeightsCorrectly(t *testing.T) {
	// Price 100 active 0..10s, price 200 active 10..40s. windowEnd = now.
	// TWAP = (100×10 + 200×30) / 40 = 175.
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	trades := []canonical.Trade{
		mkTWAPTrade(1, 100, t0),
		mkTWAPTrade(1, 200, t0.Add(10*time.Second)),
	}
	reader := &stubHistoryReader{trades: trades}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	to := t0.Add(40 * time.Second).Format(time.RFC3339)
	from := t0.Format(time.RFC3339)
	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.TWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Price != "175.0000000000" {
		t.Errorf("Price = %q, want 175.0000000000", env.Data.Price)
	}
	if env.Data.TradeCount != 2 {
		t.Errorf("TradeCount = %d, want 2", env.Data.TradeCount)
	}
}

// ─── error-path coverage ────────────────────────────────────

func TestTWAP_InvalidTime400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD&from=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTWAP_InvalidPair400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	// base == quote — NewPair rejects with invalid-pair.
	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTWAP_ReaderError500(t *testing.T) {
	reader := &stubHistoryReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestTWAP_StablecoinFiatProxyFallback — when the literal X/fiat:USD
// pair has zero trades but the operator declared a USDC peg, the TWAP
// handler retries against X/<USDC-classic> and serves the resulting
// time-weighted average with flags.triangulated=true. Mirrors #1217 /
// #1218 for the /v1/twap surface — without it, every fresh
// deployment 404s on the canonical XLM/USD TWAP query.
func TestTWAP_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdcClassic)

	pegTrade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().Add(-30 * time.Minute).UTC(),
		Pair:        classicPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdcClassic.String(): {pegTrade},
		},
	}
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stablecoin-fiat fallback should serve)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"triangulated":true`) {
		t.Errorf("body missing triangulated flag: %s", body)
	}
}

// twapFill is mkTWAPTrade with a distinct tx hash, so a ledger can carry
// several fills.
func twapFill(base, quote int64, ts time.Time, tx byte) canonical.Trade {
	tr := mkTWAPTrade(base, quote, ts)
	tr.TxHash = strings.Repeat(string("0123456789abcdef"[tx%16]), 64)
	return tr
}

// twapWire is the /v1/twap body decoded by field name, so the assertions
// pin the wire contract rather than the Go struct.
type twapWire struct {
	Price            string `json:"price"`
	TradeCount       int    `json:"trade_count"`
	OutliersFiltered int    `json:"outliers_filtered"`
}

func getTWAP(t *testing.T, trades []canonical.Trade, from, to time.Time, extra string) twapWire {
	t.Helper()
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: trades}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD&from="+
		from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339)+extra)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data twapWire `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

// TestTWAP_DustFillInLedgerDoesNotCarryInterval: a 2-stroop fill at 7.5
// shares a ledger with a real fill at 1. The ledger's price is the fills'
// Σquote/Σbase, so the dust moves it by its size, whichever fill sorts last.
// trade_count counts only weight-carrying trades — not the unpriced one.
func TestTWAP_DustFillInLedgerDoesNotCarryInterval(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	trades := []canonical.Trade{
		twapFill(999_999_998, 999_999_998, t0, 1),
		twapFill(2, 15, t0, 2), // dust, sorts last in its ledger
		twapFill(1_000_000, 1_000_000, t0.Add(10*time.Second), 3),
		twapFill(0, 5, t0.Add(10*time.Second), 4), // unpriced
	}
	got := getTWAP(t, trades, t0, t0.Add(20*time.Second), "&outlier_sigma=0")
	if got.Price != "1.0000000065" {
		t.Errorf("Price = %q, want 1.0000000065 (dust must not carry its ledger's interval)", got.Price)
	}
	if got.TradeCount != 3 {
		t.Errorf("TradeCount = %d, want 3 weight-carrying trades", got.TradeCount)
	}
}

// TestTWAP_OutlierFilterOnByDefault: a lone dust print at 7.5 in its own
// ledger would hold the price for 90 of 120 seconds. /v1/twap filters it
// at /v1/ohlc's default sigma, and outlier_sigma=0 turns the filter off.
func TestTWAP_OutlierFilterOnByDefault(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	trades := []canonical.Trade{
		twapFill(10_000_000, 10_000_000, t0, 1),
		twapFill(10_000_000, 10_100_000, t0.Add(10*time.Second), 2),
		twapFill(10_000_000, 9_900_000, t0.Add(20*time.Second), 3),
		twapFill(2, 15, t0.Add(30*time.Second), 4),
	}
	to := t0.Add(120 * time.Second)

	got := getTWAP(t, trades, t0, to, "")
	// 1×10s + 1.01×10s + 0.99×100s over 120s.
	if got.Price != "0.9925000000" {
		t.Errorf("default Price = %q, want 0.9925000000 (dust print filtered)", got.Price)
	}
	if got.OutliersFiltered != 1 || got.TradeCount != 3 {
		t.Errorf("default outliers_filtered=%d trade_count=%d, want 1 and 3",
			got.OutliersFiltered, got.TradeCount)
	}

	raw := getTWAP(t, trades, t0, to, "&outlier_sigma=0")
	// 1×10 + 1.01×10 + 0.99×10 + 7.5×90 over 120s.
	if raw.Price != "5.8750000000" || raw.OutliersFiltered != 0 {
		t.Errorf("outlier_sigma=0 Price=%q outliers_filtered=%d, want 5.8750000000 and 0",
			raw.Price, raw.OutliersFiltered)
	}
}

func TestTWAP_InvalidSigma400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD&outlier_sigma=-1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
