package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestChart_MarketCap_XLMSpellingsReadTheSameSupply: crypto:XLM is the same asset as native, so
// its market-cap series is the same series and not an empty one.
func TestChart_MarketCap_XLMSpellingsReadTheSameSupply(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	opts := v1.Options{
		History: &stubHistoryReader{points: []v1.HistoryPoint{{Bucket: d(1), VWAP: "0.10"}, {Bucket: d(2), VWAP: "0.20"}}},
		Supply: &nativeKeySupplyLooker{stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)},
		}}},
		VerifiedCurrencies: newTestCatalogue(t),
	}
	for _, asset := range []string{"native", "crypto:XLM", canonical.XLMSacContractID} {
		got := getChartOpts(t, opts, "asset="+asset+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
		if len(got.Points) != 2 || got.Points[0].P != "100.00" {
			t.Errorf("asset=%s: points = %+v, want the native series (100.00, 200.00)", asset, got.Points)
		}
	}
}

// nativeKeySupplyLooker holds a supply series under the native key only, as the store does.
type nativeKeySupplyLooker struct{ stubSupplyLooker }

func (s *nativeKeySupplyLooker) DailyCirculatingSupply(ctx context.Context, key string, from, to time.Time) ([]timescale.SupplyDayPoint, error) {
	if want, _ := supply.AssetKey(canonical.NativeAsset()); key != want {
		return nil, nil
	}
	return s.stubSupplyLooker.DailyCirculatingSupply(ctx, key, from, to)
}

func oraclePricesGet(t *testing.T, reader *stubPriceReader, pegs ...canonical.Asset) (int, []v1.SEP40Price) {
	t.Helper()
	status, data, _ := oraclePricesGetFlags(t, reader, pegs...)
	return status, data
}

func oraclePricesGetFlags(t *testing.T, reader *stubPriceReader, pegs ...canonical.Asset) (int, []v1.SEP40Price, v1.Flags) {
	t.Helper()
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader, USDPeggedClassics: pegs}).Handler())
	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, v1.Flags{}
	}
	var body struct {
		Data  []v1.SEP40Price `json:"data"`
		Flags v1.Flags        `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, body.Data, body.Flags
}

// TestOraclePrices_PegLegFailureIsNotAnEmptyHistory: when no leg serves and one failed, the
// answer is an error the client retries, never 200 [].
func TestOraclePrices_PegLegFailureIsNotAnEmptyHistory(t *testing.T) {
	usdc, _ := oraclePegs(t)
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"db error": {errors.New("db down"), http.StatusInternalServerError},
		"deadline": {context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		reader := &stubPriceReader{errByPair: map[string]error{"native/" + usdc.String(): tc.err}}
		if status, _ := oraclePricesGet(t, reader, usdc); status != tc.want {
			t.Errorf("%s: status = %d, want %d", name, status, tc.want)
		}
	}
}

// TestOraclePrices_ServesTheNewestSeries: a dormant leg is non-empty forever, so the leg with the
// newest bucket is served, whatever the walk order.
func TestOraclePrices_ServesTheNewestSeries(t *testing.T) {
	usdc, usdt := oraclePegs(t)
	series := func(quote, price string, age time.Duration) []v1.PriceSnapshot {
		return []v1.PriceSnapshot{{
			AssetID: "native", Quote: quote, Price: price, PriceType: "vwap",
			ObservedAt: v1.WireTime(time.Now().Add(-age).UTC()),
		}}
	}
	old := 300 * 24 * time.Hour

	reader := &stubPriceReader{recent: map[string][]v1.PriceSnapshot{
		"native/" + usdc.String(): series(usdc.String(), "0.0900", old),
		"native/" + usdt.String(): series(usdt.String(), "0.1631", 2*time.Minute),
	}}
	if _, data := oraclePricesGet(t, reader, usdc, usdt); len(data) != 1 || data[0].Price != "0.1631" {
		t.Errorf("dormant first peg: data = %+v, want the live 0.1631 series", data)
	}

	reader = &stubPriceReader{recent: map[string][]v1.PriceSnapshot{
		"native/fiat:USD":         series("fiat:USD", "0.0800", old),
		"native/" + usdc.String(): series(usdc.String(), "0.1630", 2*time.Minute),
	}}
	_, data, flags := oraclePricesGetFlags(t, reader, usdc)
	if len(data) != 1 || data[0].Price != "0.1630" || !flags.Stale || !flags.Triangulated {
		t.Errorf("dormant direct series: data = %+v flags = %+v, want the live peg series, stale and triangulated", data, flags)
	}

	// Neither live, a minute apart: the earlier leg in the walk (the deeper book) is kept.
	reader = &stubPriceReader{recent: map[string][]v1.PriceSnapshot{
		"native/fiat:USD":         series("fiat:USD", "0.1600", 12*time.Minute),
		"native/" + usdc.String(): series(usdc.String(), "0.1700", 11*time.Minute),
	}}
	_, data, flags = oraclePricesGetFlags(t, reader, usdc)
	if len(data) != 1 || data[0].Price != "0.1600" || flags.Triangulated {
		t.Errorf("near-tie: data = %+v flags = %+v, want the direct series kept", data, flags)
	}
}
