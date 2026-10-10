package v1_test

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubFXFixings binds each ticker to one fixed fixing whatever the bucket
// end; the binding rule itself is covered against Postgres in
// test/integration/pg_pricing_fx_oracle_test.go.
type stubFXFixings struct {
	bindings map[string]timescale.FXFixingBinding
	err      error
}

func (f *stubFXFixings) FXFixingAtOrBefore(_ context.Context, tickers []string, _ time.Time, _ time.Duration) (map[string]timescale.FXFixingBinding, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]timescale.FXFixingBinding, len(tickers))
	for _, t := range tickers {
		if b, ok := f.bindings[t]; ok {
			out[t] = b
		}
	}
	return out, nil
}

// LoadFXFixingWindow holds nothing, so every bind reads FXFixingAtOrBefore.
func (f *stubFXFixings) LoadFXFixingWindow(context.Context, time.Duration, time.Time) ([]timescale.FXFixing, time.Time, error) {
	return nil, time.Unix(0, 0), nil
}

func hourlyFixing(ticker, rate string, barEnd time.Time) timescale.FXFixingBinding {
	return timescale.FXFixingBinding{
		FXFixing: timescale.FXFixing{
			Ticker: ticker, Grain: timescale.FXGrainHour,
			BarStart: barEnd.Add(-time.Hour), BarEnd: barEnd,
			RateUSD: rate, Source: "massive",
		},
		Resolution: timescale.FXResolutionHourly,
	}
}

func fixingsOf(bindings ...timescale.FXFixingBinding) *stubFXFixings {
	m := make(map[string]timescale.FXFixingBinding, len(bindings))
	for _, b := range bindings {
		m[b.Ticker] = b
	}
	return &stubFXFixings{bindings: m}
}

// brlFixings mirrors brlCurrencies' rates as fixings.
func brlFixings() *stubFXFixings {
	barEnd := time.Now().UTC().Add(-timescale.FXFixingLag).Truncate(time.Hour)
	return fixingsOf(hourlyFixing("BRL", "5.1837", barEnd), hourlyFixing("JPY", "147.2", barEnd))
}
