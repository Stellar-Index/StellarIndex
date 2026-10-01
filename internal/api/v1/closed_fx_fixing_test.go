package v1_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// panickingCurrencies proves a closed surface never reads the live snapshot.
type panickingCurrencies struct{}

func (panickingCurrencies) Latest() *v1.CurrenciesSnapshot {
	panic("closed surface read the live FX snapshot")
}

func closedLegReader(price string, observedAt time.Time) *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmUSDKey: {AssetID: "native", Quote: "fiat:USD", Price: price, PriceType: "vwap", ObservedAt: v1.WireTime(observedAt), WindowSeconds: 60},
		},
		sources: map[string][]string{xlmUSDKey: {"sdex"}},
	}
}

type closedCrossRow struct {
	Price        string `json:"price"`
	ObservedAt   string `json:"observed_at"`
	FXRate       string `json:"fx_rate"`
	FXAsOf       string `json:"fx_as_of"`
	FXSource     string `json:"fx_source"`
	FXResolution string `json:"fx_resolution"`
	USDLeg       *struct {
		Price      string   `json:"price"`
		ObservedAt string   `json:"observed_at"`
		Sources    []string `json:"sources"`
	} `json:"usd_leg"`
}

func TestPriceClosedFXLegExact(t *testing.T) {
	bucketEnd := time.Now().UTC().Truncate(time.Minute)
	barEnd := bucketEnd.Add(-timescale.FXFixingLag).Truncate(time.Hour)
	srv := v1.New(v1.Options{
		Prices:     closedLegReader("3", bucketEnd),
		Currencies: panickingCurrencies{},
		FXFixings:  fixingsOf(hourlyFixing("BRL", "0.3", barEnd)),
	})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getCross(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var row closedCrossRow
	if err := json.Unmarshal(env.Data, &row); err != nil {
		t.Fatal(err)
	}
	// 3 × 0.3 in float64 is 0.8999999999999999; exact rationals give 0.9.
	if row.Price != "0.9" {
		t.Errorf("price = %q, want 0.9 byte-exact", row.Price)
	}
	if row.FXRate != "0.3" || row.FXSource != "massive" || row.FXResolution != timescale.FXResolutionHourly {
		t.Errorf("fx fields = %+v", row)
	}
	if row.FXAsOf != barEnd.Format(time.RFC3339) || row.ObservedAt != bucketEnd.Format(time.RFC3339) {
		t.Errorf("fx_as_of = %s, observed_at = %s; want the bar close and the USD bucket end", row.FXAsOf, row.ObservedAt)
	}
	if row.USDLeg == nil || row.USDLeg.Price != "3" || row.USDLeg.ObservedAt != row.ObservedAt || !slices.Equal(row.USDLeg.Sources, []string{"sdex"}) {
		t.Errorf("usd_leg = %+v", row.USDLeg)
	}
	if env.Flags.Stale {
		t.Error("a fresh USD bucket and a fixing within 4h of E − lag must not be stale")
	}
}

// The closed answer is a function of the bucket and the vendor series: a
// live rate change moves the tip and leaves /v1/price byte-identical.
func TestPriceClosedFXLegByteStableUnderLiveRateChange(t *testing.T) {
	currencies := brlCurrencies()
	srv := v1.New(v1.Options{
		Prices:     closedLegReader("0.2", time.Now().UTC().Truncate(time.Minute)),
		Currencies: currencies,
		FXFixings:  brlFixings(),
	})
	ts := startHTTPTest(t, srv.Handler())

	read := func(path string) string {
		status, env, body := getCross(t, ts.URL+path)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, status, body)
		}
		return string(env.Data)
	}
	closedBefore := read("/v1/price?asset=native&quote=fiat:BRL")
	tipBefore := read("/v1/price/tip?asset=native&quote=fiat:BRL")

	moved := *currencies.snap
	moved.Currencies = slices.Clone(moved.Currencies)
	moved.Currencies[0].RateUSD = 6.25
	currencies.snap = &moved

	if got := read("/v1/price?asset=native&quote=fiat:BRL"); got != closedBefore {
		t.Errorf("closed answer moved with the live rate:\nbefore %s\nafter  %s", closedBefore, got)
	}
	if read("/v1/price/tip?asset=native&quote=fiat:BRL") == tipBefore {
		t.Error("the tip must follow the live rate (the fixture's premise)")
	}
}

func TestPriceClosedFXLegMissWithholdsOnEveryPointSurface(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:     closedLegReader("0.2", time.Now().UTC().Truncate(time.Minute)),
		Currencies: panickingCurrencies{},
		FXFixings:  fixingsOf(),
	})
	ts := startHTTPTest(t, srv.Handler())

	for _, path := range []string{
		"/v1/price?asset=native&quote=fiat:BRL",
		"/v1/oracle/x_last_price?base=native&quote=fiat:BRL",
		"/v1/price?asset=fiat:EUR&quote=fiat:USD",
	} {
		status, body := getBody(t, ts.URL+path)
		if status != http.StatusNotFound || !strings.Contains(body, "errors/price-withheld") || !strings.Contains(body, "FX leg unavailable") {
			t.Errorf("%s: want a price-withheld 404 naming the FX leg, got %d: %s", path, status, body)
		}
	}
	status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:BRL")
	var env struct {
		Data     []json.RawMessage `json:"data"`
		Withheld []string          `json:"withheld"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &env) != nil {
		t.Fatalf("batch: status = %d: %s", status, body)
	}
	if len(env.Data) != 0 || !slices.Contains(env.Withheld, "native") {
		t.Errorf("batch: want the row omitted and listed as withheld, got %s", body)
	}
}

func TestPriceClosedFXReadFailureIsNotAMiss(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:     closedLegReader("0.2", time.Now().UTC().Truncate(time.Minute)),
		Currencies: panickingCurrencies{},
		FXFixings:  &stubFXFixings{err: errors.New("postgres down")},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "errors/price-unavailable") {
		t.Errorf("/v1/price: want 503 price-unavailable, got %d: %s", status, body)
	}
	status, body = getBody(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:BRL")
	if status != http.StatusOK || strings.Contains(body, `"withheld"`) || !strings.Contains(body, `"data":[]`) {
		t.Errorf("batch: want the row omitted and not listed as withheld, got %d: %s", status, body)
	}
}
