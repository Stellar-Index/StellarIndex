// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// standbyCurrencies is a forex snapshot served while the primary is down:
// EUR and BRL came from the ECB standby, NGN is held from the primary.
func standbyCurrencies() *stubCurrenciesReader {
	now := time.Now().UTC()
	return &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
		Currencies: []v1.CurrencyEntry{
			{Ticker: "BRL", RateUSD: brlRateUSD, UpdatedAt: now, Source: "ecb"},
			{Ticker: "EUR", RateUSD: 0.92, UpdatedAt: now, Source: "ecb"},
			{Ticker: "NGN", RateUSD: 1500, UpdatedAt: now, Source: "massive"},
		},
		PublishedAt: now,
	}}
}

// standbyFixings is the closed surfaces' view of the same feeds.
func standbyFixings() *stubFXFixings {
	barEnd := time.Now().UTC().Add(-timescale.FXFixingLag).Truncate(time.Hour)
	f := fixingsOf(
		hourlyFixing("BRL", "5.1837", barEnd),
		hourlyFixing("EUR", "0.92", barEnd),
		hourlyFixing("NGN", "1500", barEnd),
	)
	for _, t := range []string{"BRL", "EUR"} {
		b := f.bindings[t]
		b.Source = "ecb"
		f.bindings[t] = b
	}
	return f
}

// sources[] must name the feed that published each FX rate, not a fixed
// primary: an auditor of a price served during a failover needs to see
// the standby behind it.
func TestFiatCrossSourcesNameThePublishingFeed(t *testing.T) {
	cases := []struct {
		path string
		opts v1.Options
		want []string
	}{
		{
			path: "/v1/price?asset=fiat:EUR&quote=fiat:USD",
			opts: v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}},
			want: []string{"ecb"},
		},
		{
			path: "/v1/price?asset=fiat:EUR&quote=fiat:NGN",
			opts: v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}},
			want: []string{"ecb", "massive"},
		},
		{
			path: "/v1/price?asset=native&quote=fiat:BRL",
			opts: v1.Options{Prices: brlLegReader("binance", "sdex")},
			want: []string{"binance", "ecb", "sdex"},
		},
		{
			path: "/v1/price/tip?asset=native&quote=fiat:BRL",
			opts: v1.Options{Prices: brlLegReader("binance", "sdex")},
			want: []string{"binance", "ecb", "sdex"},
		},
		{
			// A frozen leg's held value is still converted at the standby's rate.
			path: "/v1/price?asset=native&quote=fiat:BRL",
			opts: v1.Options{
				Prices:       brlLegReader("binance", "sdex"),
				Freeze:       frozenPairs{xlmUSDKey: true},
				Triangulated: lkgPairs{xlmUSDKey + "/300": legHeld},
			},
			want: []string{"ecb"},
		},
	}
	for _, tc := range cases {
		tc.opts.Currencies = standbyCurrencies()
		tc.opts.FXFixings = standbyFixings()
		ts := startHTTPTest(t, v1.New(tc.opts).Handler())
		status, env, body := getCross(t, ts.URL+tc.path)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", tc.path, status, body)
		}
		if !slices.Equal(env.Sources, tc.want) {
			t.Errorf("%s: sources = %v, want %v", tc.path, env.Sources, tc.want)
		}
	}
}

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
