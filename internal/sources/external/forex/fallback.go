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

// RateProvider is a source of USD-base fiat rates for the worker. It keeps `massive`, a paid feed,
// from being a single point of failure: a lapse, a 429 or an unset MASSIVE_API_KEY would otherwise
// silently break every fiat cross once the 7-day forex-snap lookback expired.
type RateProvider interface {
	// Name is the value stamped into fx_quotes.source and used as the
	// `source` metric label, so an operator can see WHICH feed is live.
	Name() string
	// LatestUSDRates returns units-of-ticker per 1 USD plus the upstream's published timestamp,
	// as [Client.LatestUSDRates] does, except key case is the provider's own (primary lower-case,
	// ECB upper-case). The worker's joins normalise case (see lowerKeyed), so a provider need not.
	LatestUSDRates(ctx context.Context) (map[string]float64, time.Time, error)
}

// ErrNoUSDAnchor is returned when a EUR-base upstream omits USD, which
// makes the EUR->USD rebase impossible. Fail rather than guess: a
// fabricated anchor would mis-price every fiat pair at once.
var ErrNoUSDAnchor = errors.New("forex: upstream has no USD rate to rebase on")

// ECBProvider adapts the ECB daily reference rates into the worker's USD-base contract. ECB is
// free, keyless and authoritative, so it is the standby. Coverage is ~30 currencies, not massive's
// 111, so other pairs keep serving their last-good fx_quotes row via the lookback. Cadence is one
// publication per working day (~16:00 CET), static intraday and absent on weekends/TARGET
// holidays, which the worker's staleness handling tolerates.
type ECBProvider struct {
	// Endpoint overrides the ECB daily-XML URL. Empty uses the
	// package default; tests point it at an httptest server.
	Endpoint string
}

// Name implements [RateProvider].
func (p ECBProvider) Name() string { return "ecb" }

// LatestUSDRates implements [RateProvider], rebasing ECB's EUR-quoted document onto USD. ECB
// publishes "1 EUR = X <currency>" but the worker wants "1 USD = X <currency>", so every rate
// divides by ECB's own USD rate: usdRate(X) = eurRate(X) / eurRate(USD). EUR is added by hand
// because ECB has no cube for its own base: EUR per USD is 1 / eurRate(USD).
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

// publishedDecimal recovers ECB's published decimal exactly: rates carry at most six significant
// digits, which a float64's shortest representation round-trips, so the rebase divides published
// values rather than binary approximations. ok is false for a missing, non-positive or non-finite
// rate (ECB's parser admits "NaN" and "Inf").
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
