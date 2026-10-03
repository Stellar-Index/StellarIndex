package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	devUSDC  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	devToken = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

// devServer prices devToken against the declared USDC peg and, when
// pegPerXLM is non-empty, gives the peg an XLM book (USDC/native) beside
// a 0.10 USD XLM market, so the peg's own USD price is pegPerXLM × 0.10.
func devServer(t *testing.T, pegPerXLM string) *testServerImpl {
	t.Helper()
	usdc, err := canonical.ParseAsset(devUSDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	at := v1.WireTime(time.Unix(1745000000, 0).UTC())
	snap := func(base, quote, price string) v1.PriceSnapshot {
		return v1.PriceSnapshot{AssetID: base, Quote: quote, Price: price, PriceType: "vwap", ObservedAt: at, WindowSeconds: 60}
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			devToken + "/" + devUSDC: snap(devToken, devUSDC, "2.0000"),
			"native/fiat:USD":        snap("native", "fiat:USD", "0.10"),
		},
		sources: map[string][]string{
			devToken + "/" + devUSDC: {"sdex"},
			"native/fiat:USD":        {"binance"},
			devUSDC + "/native":      {"sdex"},
		},
	}
	if pegPerXLM != "" {
		reader.snapshots[devUSDC+"/native"] = snap(devUSDC, "native", pegPerXLM)
	}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	return startHTTPTest(t, srv.Handler())
}

func TestPrice_ProxyDeviation_Band(t *testing.T) {
	cases := []struct {
		name      string
		pegPerXLM string // peg USD price = pegPerXLM × 0.10
		want      bool
	}{
		{"depegged below", "9.5", true},
		{"depegged above", "10.5", true},
		{"on peg", "10.02", false},
		{"exactly at the band edge", "10.2", false},
		{"just past the band edge", "10.201", true},
		{"no peg market is not a flag", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := devServer(t, tc.pegPerXLM)
			resp := mustGet(t, ts.URL+"/v1/price?asset="+devToken+"&quote=fiat:USD")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body, _ := readAll(resp)
			if !strings.Contains(body, `"price":"2.0000"`) {
				t.Errorf("proxy price must be unchanged: %s", body)
			}
			if got := strings.Contains(body, `"proxy_deviation":true`); got != tc.want {
				t.Errorf("proxy_deviation present = %v, want %v: %s", got, tc.want, body)
			}
		})
	}
}

func TestPrice_ProxyDeviation_DeclaredPegOwnPrice(t *testing.T) {
	ts := devServer(t, "9.5")
	resp := mustGet(t, ts.URL+"/v1/price?asset="+devUSDC+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"proxy_deviation":true`) {
		t.Errorf("a depegged peg's own price must carry proxy_deviation: %s", body)
	}
}

func TestOracleLastPrice_ProxyDeviation(t *testing.T) {
	for _, path := range []string{"/v1/oracle/lastprice", "/v1/oracle/x_last_price"} {
		for _, tc := range []struct {
			pegPerXLM string
			want      bool
		}{{"9.5", true}, {"10.02", false}} {
			t.Run(path+"/peg_per_xlm_"+tc.pegPerXLM, func(t *testing.T) {
				ts := devServer(t, tc.pegPerXLM)
				url := ts.URL + path + "?asset=" + devToken
				if strings.HasSuffix(path, "x_last_price") {
					url = ts.URL + path + "?base=" + devToken + "&quote=fiat:USD"
				}
				resp := mustGet(t, url)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				body, _ := readAll(resp)
				if !strings.Contains(body, `"triangulated":true`) {
					t.Fatalf("fixture must be served through the peg: %s", body)
				}
				if got := strings.Contains(body, `"proxy_deviation":true`); got != tc.want {
					t.Errorf("proxy_deviation present = %v, want %v: %s", got, tc.want, body)
				}
			})
		}
	}
}

func TestPrice_ProxyDeviation_BatchAndTip(t *testing.T) {
	ts := devServer(t, "9.5")
	for _, path := range []string{
		"/v1/price/batch?asset_ids=" + devToken + "&quote=fiat:USD",
		"/v1/price/tip?asset=" + devToken + "&quote=fiat:USD",
	} {
		resp := mustGet(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		body, _ := readAll(resp)
		if !strings.Contains(body, `"proxy_deviation":true`) {
			t.Errorf("%s missing proxy_deviation: %s", path, body)
		}
	}
}

// devPointStub answers PriceAt for the listed pairs at one minute before the
// requested instant, and misses on the rest.
type devPointStub map[string]string

func (s devPointStub) PriceAt(_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	if v, ok := s[pair.Base.String()+"/"+pair.Quote.String()]; ok {
		return v, ts.Add(-time.Minute), 60, nil
	}
	return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
}

// devPointServer serves devToken/fiat:USD only through the USDC peg on every
// point surface: CAGG buckets for /v1/price/at and /v1/price/changes, raw
// trades for /v1/vwap, /v1/twap and single-bar /v1/ohlc.
func devPointServer(t *testing.T, pegPerXLM string) *testServerImpl {
	t.Helper()
	return devPointServerSplit(t, pegPerXLM, pegPerXLM)
}

// devPointServerSplit is devPointServer with the peg's live market
// (livePegPerXLM) and its market at the requested instant (atPegPerXLM)
// set independently.
func devPointServerSplit(t *testing.T, livePegPerXLM, atPegPerXLM string) *testServerImpl {
	t.Helper()
	usdc, err := canonical.ParseAsset(devUSDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	token, err := canonical.ParseAsset(devToken)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	pair, err := canonical.NewPair(token, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := sacReachDay(0)
	at := v1.WireTime(time.Unix(1745000000, 0).UTC())
	snap := func(base, quote, price string) v1.PriceSnapshot {
		return v1.PriceSnapshot{AssetID: base, Quote: quote, Price: price, PriceType: "vwap", ObservedAt: at, WindowSeconds: 60}
	}
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				devUSDC + "/native": snap(devUSDC, "native", livePegPerXLM),
				"native/fiat:USD":   snap("native", "fiat:USD", "0.10"),
			},
			sources: map[string][]string{devUSDC + "/native": {"sdex"}, "native/fiat:USD": {"binance"}},
		},
		PriceAt: devPointStub{
			devToken + "/" + devUSDC: "2.0",
			devUSDC + "/native":      atPegPerXLM,
			"native/fiat:USD":        "0.10",
		},
		History: &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(pair): {
				fiatParityTrade(pair, 1, t0.Add(time.Minute), 1000, 2000),
				fiatParityTrade(pair, 2, t0.Add(2*time.Minute), 1000, 2000),
			},
		}},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	return startHTTPTest(t, srv.Handler())
}

func TestPointSurfaces_ProxyDeviation(t *testing.T) {
	t0 := sacReachDay(0)
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	paths := map[string]string{
		"price_at": "/v1/price/at?asset=" + devToken + "&quote=fiat:USD&ts=" +
			time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"price_changes": "/v1/price/changes?asset=" + devToken + "&quote=fiat:USD",
		"vwap":          "/v1/vwap?base=" + devToken + "&quote=fiat:USD" + win,
		"twap":          "/v1/twap?base=" + devToken + "&quote=fiat:USD" + win,
		"ohlc":          "/v1/ohlc?base=" + devToken + "&quote=fiat:USD" + win,
	}
	for name, path := range paths {
		for _, tc := range []struct {
			pegPerXLM string
			want      bool
		}{{"9.5", true}, {"10.02", false}} {
			t.Run(name+"/peg_per_xlm_"+tc.pegPerXLM, func(t *testing.T) {
				ts := devPointServer(t, tc.pegPerXLM)
				resp := mustGet(t, ts.URL+path)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				body, _ := readAll(resp)
				if !strings.Contains(body, `"triangulated":true`) {
					t.Fatalf("fixture must be served through the peg: %s", body)
				}
				if got := strings.Contains(body, `"proxy_deviation":true`); got != tc.want {
					t.Errorf("proxy_deviation present = %v, want %v: %s", got, tc.want, body)
				}
			})
		}
	}
}

// The flag on a trade window describes the window's own peg market, not the
// live one: a depeg today must not flag an on-peg past window, and a past
// depeg must not be hidden by a peg that has since recovered.
func TestWindowSurfaces_ProxyDeviationFollowsWindow(t *testing.T) {
	t0 := sacReachDay(0)
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	paths := map[string]string{
		"vwap": "/v1/vwap?base=" + devToken + "&quote=fiat:USD" + win,
		"twap": "/v1/twap?base=" + devToken + "&quote=fiat:USD" + win,
		"ohlc": "/v1/ohlc?base=" + devToken + "&quote=fiat:USD" + win,
	}
	for name, path := range paths {
		for _, tc := range []struct {
			label, live, at string
			want            bool
		}{
			{"live depegged, window on peg", "9.5", "10.02", false},
			{"live on peg, window depegged", "10.02", "9.5", true},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				ts := devPointServerSplit(t, tc.live, tc.at)
				resp := mustGet(t, ts.URL+path)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				body, _ := readAll(resp)
				if got := strings.Contains(body, `"proxy_deviation":true`); got != tc.want {
					t.Errorf("proxy_deviation present = %v, want %v: %s", got, tc.want, body)
				}
			})
		}
	}
}
