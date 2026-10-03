package poloniex

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// Two daily rows in Poloniex's positional layout.
const fixture = `[
["0.00000700","0.00000820","0.00000713","0.00000815","46.605","5719000.5","0","0","12","1443571200000","0.0000081","DAY_1","1443571200000","1443657599999"],
["0.00000800","0.00000900","0.00000815","0.00000850","10","1200000","0","0","3","1443657600000","0.0000083","DAY_1","1443657600000","1443743999999"]
]`

type btcLeg struct{ trades []canonical.Trade }

func (btcLeg) Name() string          { return "bitstamp" }
func (btcLeg) Class() external.Class { return external.ClassExchange }
func (l btcLeg) Backfill(context.Context, canonical.Pair, time.Time, time.Time, time.Duration) ([]canonical.Trade, error) {
	return l.trades, nil
}

func pairOf(t *testing.T, base string) canonical.Pair {
	t.Helper()
	b, err := canonical.NewCryptoAsset(base)
	if err != nil {
		t.Fatal(err)
	}
	u, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	p, err := canonical.NewPair(b, u)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func btcTrade(day time.Time, btc, usd int64) canonical.Trade {
	return canonical.Trade{
		Source:      "bitstamp",
		Timestamp:   day.Add(24*time.Hour - time.Second),
		BaseAmount:  canonical.NewAmount(big.NewInt(btc * 1e8)),
		QuoteAmount: canonical.NewAmount(big.NewInt(usd * 1e8)),
	}
}

func TestBackfill_DerivesXLMUSDThroughBTC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/XLM_BTC/candles" || r.URL.Query().Get("interval") != "DAY_1" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	d1 := time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)
	b := &Backfiller{
		Endpoint: srv.URL,
		// 230 USD/BTC on day one; day two has no BTC leg and must be
		// omitted, not interpolated.
		BTCLeg: btcLeg{trades: []canonical.Trade{btcTrade(d1, 1000, 230000)}},
	}
	got, err := b.Backfill(context.Background(), pairOf(t, "XLM"), d1, d1.AddDate(0, 0, 3), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d trades, want 1 (day two has no BTC leg)", len(got))
	}
	tr := got[0]
	if tr.Source != SourceName {
		t.Errorf("source = %q, want %q", tr.Source, SourceName)
	}
	if tr.BaseAmount.String() != "571900050000000" {
		t.Errorf("base = %s, want 571900050000000 (5719000.5 XLM at 1e8)", tr.BaseAmount)
	}
	// 5719000.5 XLM x (0.00000815 BTC x 230 USD) = 10720.26643725 USD.
	if tr.QuoteAmount.String() != "1072026643725" {
		t.Errorf("quote = %s, want 1072026643725", tr.QuoteAmount)
	}
	if want := d1.Add(24*time.Hour - time.Second); !tr.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", tr.Timestamp, want)
	}
}

func TestBackfill_RejectsOtherGranularityAndPair(t *testing.T) {
	b := &Backfiller{BTCLeg: btcLeg{}}
	d := time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)
	if _, err := b.Backfill(context.Background(), pairOf(t, "XLM"), d, d.AddDate(0, 0, 1), time.Hour); err == nil {
		t.Error("1h granularity accepted, want error")
	}
	if _, err := b.Backfill(context.Background(), pairOf(t, "ETH"), d, d.AddDate(0, 0, 1), 24*time.Hour); err == nil {
		t.Error("ETH/USD accepted, want error")
	}
}

func TestCheckRange(t *testing.T) {
	d := func(y int, m time.Month, dd int) time.Time { return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name     string
		from, to time.Time
		ok       bool
	}{
		{"whole span", SpanStart, SpanEnd, true},
		{"inside", d(2016, 1, 1), d(2016, 6, 1), true},
		{"starts early", d(2015, 9, 29), d(2016, 1, 1), false},
		{"runs into fill-derived data", d(2016, 1, 1), d(2017, 1, 18), false},
	}
	for _, c := range cases {
		if err := CheckRange(c.from, c.to); (err == nil) != c.ok {
			t.Errorf("%s: CheckRange err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestParseCandles_ShortRowIsAnError(t *testing.T) {
	if _, err := parseCandles([]byte(`[["1","2"]]`)); err == nil {
		t.Error("short candle row accepted, want error")
	}
}
