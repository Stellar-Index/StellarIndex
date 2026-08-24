package divergence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SyntheticCrossName is the stable source label for the USD-cross
// reference. Appears in the divergence result's Sources map, in
// Prometheus labels, and in operator dumps — renaming is a wire break.
const SyntheticCrossName = "synthetic-usd-cross"

// SyntheticCrossReference derives a reference price for a pair quoted
// in a non-USD fiat by crossing two USD-quoted legs:
//
//	base/fiat:X  :=  (base / fiat:USD)  ÷  (fiat:X / fiat:USD)
//
// Motivation (2026-08-24, the corroborated-release amendment's
// operational cost): EUR/GBP-quoted pairs have exactly ONE direct
// reference (CoinGecko), which is below the divergence trust floor
// (divergenceMinSources), so their freezes can never auto-release —
// every one ends in an operator page. The components for a second,
// independent reading already exist in the reference set: the on-chain
// oracles price the BASE in USD (reflector-cex, chainlink, redstone,
// band) and reflector-fx prices FIAT codes in USD. This reference
// composes them.
//
// Independence: the synthetic counts as one source in Compare's median
// and SuccessCount. Its legs (USD-quoted oracle feeds) do not answer
// non-USD-fiat-quoted pairs directly — that gap is precisely why this
// reference exists — so the same underlying feed cannot contribute
// twice to one pair's reference set. If a leg source ever learns to
// answer such pairs directly, revisit this before keeping both (a
// doubled source would overweight it in the median).
//
// The composite is only as good as its weaker leg: both legs must be
// fresh (each leg's own MaxAge discipline applies — this type adds no
// caching), positive, and finite. Any leg failure degrades to the
// sentinel that keeps Compare's bookkeeping honest: unsupported when no
// leg CAN answer, unavailable when a leg SHOULD have answered but
// didn't.
type SyntheticCrossReference struct {
	usdLegs []Reference // ordered candidates for base → fiat:USD
	fxLegs  []Reference // ordered candidates for fiat:X → fiat:USD
	usd     canonical.Asset
}

// SyntheticCrossOptions configures NewSyntheticCrossReference.
type SyntheticCrossOptions struct {
	// USDLegs are tried in order for the base-in-USD leg; the first
	// that answers wins. Typically the on-chain oracle references
	// (reflector-cex, chainlink, redstone, band).
	USDLegs []Reference
	// FXLegs are tried in order for the fiat-in-USD leg. Typically
	// reflector-fx first (on-chain rows, no extra RPC) with chainlink's
	// direct fiat/USD feeds as fallback — the proven GBP/USD source.
	FXLegs []Reference
}

// NewSyntheticCrossReference validates the leg sets. Both must be
// non-empty — a cross with a missing leg can never answer and would
// only add a permanent failure row to every result.
func NewSyntheticCrossReference(opts SyntheticCrossOptions) (*SyntheticCrossReference, error) {
	if len(opts.USDLegs) == 0 || len(opts.FXLegs) == 0 {
		return nil, errors.New("divergence: synthetic cross needs at least one USD leg and one FX leg")
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil, fmt.Errorf("divergence: synthetic cross: %w", err)
	}
	return &SyntheticCrossReference{
		usdLegs: opts.USDLegs,
		fxLegs:  opts.FXLegs,
		usd:     usd,
	}, nil
}

// Name implements [Reference].
func (s *SyntheticCrossReference) Name() string { return SyntheticCrossName }

// LookupPrice implements [Reference]. Only pairs quoted in a non-USD
// fiat are in scope; everything else is ErrAssetUnsupported (USD-quoted
// pairs already have the direct oracle references — a synthetic reading
// there would double-count the very feeds it is built from).
func (s *SyntheticCrossReference) LookupPrice(ctx context.Context, pair canonical.Pair, observedAt time.Time) (float64, error) {
	if pair.Quote.Type != canonical.AssetFiat || pair.Quote.Code == "USD" {
		return 0, fmt.Errorf("%w: %s: synthetic cross covers non-USD-fiat quotes only",
			ErrAssetUnsupported, SyntheticCrossName)
	}

	baseUSD, err := s.lookupLeg(ctx, s.usdLegs, canonical.Pair{Base: pair.Base, Quote: s.usd}, observedAt)
	if err != nil {
		return 0, fmt.Errorf("%s: base leg %s/USD: %w", SyntheticCrossName, pair.Base.String(), err)
	}
	fiatUSD, err := s.lookupLeg(ctx, s.fxLegs, canonical.Pair{Base: pair.Quote, Quote: s.usd}, observedAt)
	if err != nil {
		return 0, fmt.Errorf("%s: fx leg %s/USD: %w", SyntheticCrossName, pair.Quote.String(), err)
	}

	price := baseUSD / fiatUSD
	if !isUsablePrice(price) {
		return 0, fmt.Errorf("%w: %s: cross %v/%v is not a usable price",
			ErrPriceUnavailable, SyntheticCrossName, baseUSD, fiatUSD)
	}
	return price, nil
}

// lookupLeg tries each candidate in order and returns the first usable
// answer. Error semantics preserve Compare's unsupported-vs-degraded
// distinction: if ANY leg failed transiently the leg is "unavailable"
// (a reading should have existed); only when every candidate reports
// unsupported is the leg — and so the cross — unsupported for the pair.
func (s *SyntheticCrossReference) lookupLeg(ctx context.Context, legs []Reference, pair canonical.Pair, observedAt time.Time) (float64, error) {
	sawTransient := false
	var lastErr error
	for _, ref := range legs {
		price, err := ref.LookupPrice(ctx, pair, observedAt)
		if err == nil {
			if !isUsablePrice(price) {
				sawTransient = true
				lastErr = fmt.Errorf("%s returned unusable %v", ref.Name(), price)
				continue
			}
			return price, nil
		}
		if errors.Is(err, ErrAssetUnsupported) {
			lastErr = err
			continue
		}
		sawTransient = true
		lastErr = err
	}
	if sawTransient {
		return 0, fmt.Errorf("%w: %v", ErrPriceUnavailable, lastErr)
	}
	return 0, fmt.Errorf("%w: no leg lists %s", ErrAssetUnsupported, pair.String())
}

// isUsablePrice rejects the values that would poison a division or a
// median: zero, negatives, NaN, ±Inf.
func isUsablePrice(p float64) bool {
	return p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0)
}
