package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// stubAssetsReaderExt implements v1.AssetsReader for the asset-extension
// overlay test path. Each method returns the canned value its caller
// expects; everything else returns sql.ErrNoRows so behaviour
// degrades cleanly.
type stubAssetsReaderExt struct {
	row        timescale.AssetRow
	rowErr     error
	topMarkets []timescale.AssetTopMarket
	hist24     []timescale.AssetPricePoint
	hist7d     []timescale.AssetPricePoint
	marketsN   int64
	tradeN     int64
	ath        *timescale.AssetATH
}

func (s *stubAssetsReaderExt) ListAssetsExt(_ context.Context, _ timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetBySlug(_ context.Context, _ string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (s *stubAssetsReaderExt) GetAssetByAssetID(_ context.Context, _ string) (timescale.AssetRow, error) {
	return s.row, s.rowErr
}

func (s *stubAssetsReaderExt) GetNativeAssetRow(_ context.Context) (timescale.AssetRow, error) {
	return s.row, s.rowErr
}

func (s *stubAssetsReaderExt) GetAssetTopMarkets(_ context.Context, _ string, _ int) ([]timescale.AssetTopMarket, error) {
	return s.topMarkets, nil
}

func (s *stubAssetsReaderExt) GetAssetPriceHistory24h(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return s.hist24, nil
}

func (s *stubAssetsReaderExt) GetAssetPriceHistory7d(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return s.hist7d, nil
}

func (s *stubAssetsReaderExt) GetAssetsPriceHistory24hBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetsPriceHistory7dBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetMarketsCount(_ context.Context, _ string) (int64, error) {
	return s.marketsN, nil
}

func (s *stubAssetsReaderExt) GetAssetATH(_ context.Context, _ string) (*timescale.AssetATH, error) {
	return s.ath, nil
}

func (s *stubAssetsReaderExt) GetAssetsATHBatch(_ context.Context, _ []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetTradeCount24h(_ context.Context, _ string) (int64, error) {
	return s.tradeN, nil
}

// ptr is a tiny helper.
func sptr(s string) *string { return &s }

func TestAssetGet_AssetExtension_Populates(t *testing.T) {
	price := sptr("1.0008")
	ch1h := sptr("+0.11")
	ch7d := sptr("-0.04")
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug:        "USDC",
			AssetID:     "USDC-" + testUSDCIssuer,
			Code:        "USDC",
			PriceUSD:    price,
			Change1hPct: ch1h,
			Change7dPct: ch7d,
		},
		topMarkets: []timescale.AssetTopMarket{
			{Counterparty: "native", Side: "quote", Volume24hUSD: sptr("1000.0"), TradeCount24h: 5},
		},
		hist24:   []timescale.AssetPricePoint{{T: "2026-05-11T00:00:00Z", P: sptr("1.0001")}},
		hist7d:   []timescale.AssetPricePoint{{T: "2026-05-05T00:00:00Z", P: sptr("1.0010")}},
		marketsN: 12,
		tradeN:   456,
		ath:      &timescale.AssetATH{USD: "6.39", At: "2026-05-04T00:00:00Z"},
	}
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.PriceUSD == nil || *d.PriceUSD != "1.0008" {
		t.Errorf("price_usd: got %v want 1.0008", d.PriceUSD)
	}
	if d.Change1hPct == nil || *d.Change1hPct != "+0.11" {
		t.Errorf("change_1h_pct: got %v", d.Change1hPct)
	}
	if d.Change7dPct == nil || *d.Change7dPct != "-0.04" {
		t.Errorf("change_7d_pct: got %v", d.Change7dPct)
	}
	if len(d.TopMarkets) != 1 || d.TopMarkets[0].Counterparty != "native" {
		t.Errorf("top_markets unexpected: %+v", d.TopMarkets)
	}
	if len(d.PriceHistory24h) != 1 {
		t.Errorf("price_history_24h: %d points want 1", len(d.PriceHistory24h))
	}
	if len(d.PriceHistory7d) != 1 {
		t.Errorf("price_history_7d: %d points want 1", len(d.PriceHistory7d))
	}
	if d.MarketsCount == nil || *d.MarketsCount != 12 {
		t.Errorf("markets_count: %v", d.MarketsCount)
	}
	if d.TradeCount24h == nil || *d.TradeCount24h != 456 {
		t.Errorf("trade_count_24h: %v", d.TradeCount24h)
	}
	if d.ATH == nil || d.ATH.USD != "6.39" {
		t.Errorf("ath: %+v", d.ATH)
	}
}

func TestAssetGet_AssetExtension_NoAssetReader_NoOp(t *testing.T) {
	// No AssetsReader wired — asset-extension fields stay nil; rest
	// of the response is unchanged.
	usdc, _ := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data.PriceUSD != nil || len(env.Data.TopMarkets) != 0 {
		t.Errorf("expected nil extension fields, got %+v", env.Data)
	}
}

func TestAssetGet_AssetExtension_FiatAsset_Skipped(t *testing.T) {
	// fiat:* assets have no asset-catalogue row; the extension is a no-op even
	// when an AssetsReader is wired.
	assetsReader := &stubAssetsReaderExt{
		row:        timescale.AssetRow{PriceUSD: sptr("999")}, // would leak through if not gated
		topMarkets: []timescale.AssetTopMarket{{Counterparty: "should_not_appear"}},
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usd.String(): {AssetID: "fiat:USD", Type: "fiat", Code: "USD"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data.PriceUSD != nil {
		t.Errorf("fiat asset should NOT get asset extension; got price_usd=%v", env.Data.PriceUSD)
	}
	if len(env.Data.TopMarkets) != 0 {
		t.Errorf("fiat asset should NOT get top_markets; got %+v", env.Data.TopMarkets)
	}
}
