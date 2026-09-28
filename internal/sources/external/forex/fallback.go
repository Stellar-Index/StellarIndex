package forex

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/ecb"
)

// RateProvider is a source of USD-base fiat rates for the worker.
//
// It exists so `massive` — a PAID feed — is not a single point of
// failure for every fiat-quoted pair on the platform. Measured
// 2026-08-27: massive was the only series in
// stellarindex_external_fx_last_quote_unix, so a subscription lapse, a
// 429, or an unset MASSIVE_API_KEY silently broke XLM/EUR, XLM/GBP and
// every other fiat cross once the 7-day forex-snap lookback expired.
// The staleness alert's own comment already anticipated a fallback
// ("re-enabling … as fallbacks naturally clears it"); nothing
// implemented one.
type RateProvider interface {
	// Name is the value stamped into fx_quotes.source and used as the
	// `source` metric label, so an operator can see WHICH feed is live.
	Name() string
	// LatestUSDRates returns units-of-ticker per 1 USD, plus the
	// upstream's published timestamp. Same contract as
	// [Client.LatestUSDRates], except that the CASE of the currency-code
	// keys is the provider's own: the primary emits lower-case, ECB emits
	// the UPPER-case codes it publishes. The worker's joins normalise the
	// case (see lowerKeyed), so a provider need not.
	LatestUSDRates(ctx context.Context) (map[string]float64, time.Time, error)
}

// ErrNoUSDAnchor is returned when a EUR-base upstream omits USD, which
// makes the EUR->USD rebase impossible. Fail rather than guess: a
// fabricated anchor would mis-price every fiat pair at once.
var ErrNoUSDAnchor = errors.New("forex: upstream has no USD rate to rebase on")

// ECBProvider adapts the ECB daily reference rates into the worker's
// USD-base contract.
//
// ECB is free, keyless, and authoritative — it is the reference the
// commercial feeds themselves derive from — which makes it the right
// standby. Two honest limitations, both preferable to no rates at all:
//
//   - COVERAGE is ~30 currencies, not massive's 111. Fiat pairs outside
//     ECB's list keep serving their last-good fx_quotes row via the
//     existing lookback rather than getting a fresh one.
//   - CADENCE is one publication per working day (~16:00 CET), so rates
//     do not move intraday and are absent on weekends/TARGET holidays.
//     The worker's staleness handling already tolerates this: the daily
//     file simply stops advancing, exactly like a quiet primary.
type ECBProvider struct {
	// Endpoint overrides the ECB daily-XML URL. Empty uses the
	// package default; tests point it at an httptest server.
	Endpoint string
}

// Name implements [RateProvider].
func (p ECBProvider) Name() string { return "ecb" }

// LatestUSDRates implements [RateProvider], rebasing ECB's EUR-quoted
// document onto USD.
//
// ECB publishes "1 EUR = X <currency>". The worker wants "1 USD = X
// <currency>", so every rate divides by ECB's own USD rate:
//
//	usdRate(X) = eurRate(X) / eurRate(USD)
//
// EUR needs adding by hand because ECB has no cube for its own base
// currency: units of EUR per 1 USD is simply 1 / eurRate(USD).
func (p ECBProvider) LatestUSDRates(ctx context.Context) (map[string]float64, time.Time, error) {
	eurRates, publishedAt, err := ecb.LatestEURRates(ctx, p.Endpoint)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("forex: ecb fallback: %w", err)
	}
	usdPerEUR, ok := publishedDecimal(eurRates["USD"])
	if !ok {
		return nil, time.Time{}, ErrNoUSDAnchor
	}

	out := make(map[string]float64, len(eurRates)+1)
	for code, eurRate := range eurRates {
		r, ok := publishedDecimal(eurRate)
		if !ok {
			continue
		}
		if code == "USD" {
			// Identity — carrying it keeps the map self-consistent for
			// callers that look USD up rather than special-casing it.
			out["USD"] = 1
			continue
		}
		out[code] = ratFloat(new(big.Rat).Quo(r, usdPerEUR))
	}
	out["EUR"] = ratFloat(new(big.Rat).Inv(usdPerEUR))
	return out, publishedAt, nil
}

// publishedDecimal recovers ECB's published decimal exactly: its rates
// carry at most six significant digits, which a float64's shortest
// representation round-trips, so the rebase divides the published values
// rather than their binary approximations. ok is false for a missing,
// non-positive or non-finite rate (ECB's parser admits "NaN" and "Inf").
func publishedDecimal(f float64) (*big.Rat, bool) {
	if !isFiniteFloat(f) || f <= 0 {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	return r, ok
}

func ratFloat(r *big.Rat) float64 {
	f, _ := r.Float64() // i128:ok the RateProvider contract is float64; one correctly rounded conversion of the exact rebased rate
	return f
}
