package divergence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SyntheticCrossName is the stable source label for the USD-cross reference; it appears in the Sources map, Prometheus labels and operator dumps, so renaming is a wire break.
const SyntheticCrossName = "synthetic-usd-cross"

// SyntheticCrossReference derives a non-USD-fiat pair as (base / fiat:USD) / (fiat:X / fiat:USD); it counts as one source, the legs must come from different publishers, and both must be fresh, positive and finite.
type SyntheticCrossReference struct {
	usdLegs []Reference // ordered candidates for base → fiat:USD
	fxLegs  []Reference // ordered candidates for fiat:X → fiat:USD
	usd     canonical.Asset
}

// SyntheticCrossOptions configures NewSyntheticCrossReference.
type SyntheticCrossOptions struct {
	// USDLegs are tried in order for the base-in-USD leg; the first
	// that answers and has an independent FX leg wins. Typically the on-chain oracle references
	// (reflector-cex, redstone, band, chainlink-stellar).
	USDLegs []Reference
	// FXLegs are tried in order for the fiat-in-USD leg, skipping any
	// from the base leg's publisher. Typically
	// reflector-fx (on-chain rows, no extra RPC).
	FXLegs []Reference
}

// NewSyntheticCrossReference validates the leg sets; both must be non-empty, since a cross with a missing leg can never answer and would only add a permanent failure row.
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

// LookupQuote implements Reference for pairs quoted in a non-USD fiat only (USD pairs have direct oracles, and a synthetic there would double-count); each leg is held to its own MaxComparableAge.
func (s *SyntheticCrossReference) LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error) {
	if pair.Quote.Type != canonical.AssetFiat || pair.Quote.Code == "USD" {
		return Quote{}, fmt.Errorf("%w: %s: synthetic cross covers non-USD-fiat quotes only",
			ErrAssetUnsupported, SyntheticCrossName)
	}

	basePair := canonical.Pair{Base: pair.Base, Quote: s.usd}
	fx := newLegCandidates(s.fxLegs, canonical.Pair{Base: pair.Quote, Quote: s.usd})
	var baseFailures legFailures
	baseAnswered := false
	for _, baseRef := range s.usdLegs {
		baseUSD, err := lookupLeg(ctx, baseRef, basePair, observedAt)
		if err != nil {
			baseFailures.record(err)
			continue
		}
		baseAnswered = true
		fiatUSD, ok := fx.firstIndependentOf(ctx, referencePublisher(baseRef.Name()), observedAt)
		if !ok {
			continue
		}
		price := baseUSD.Price / fiatUSD.Price
		if !isUsablePrice(price) {
			return Quote{}, fmt.Errorf("%w: %s: cross %v/%v is not a usable price",
				ErrPriceUnavailable, SyntheticCrossName, baseUSD.Price, fiatUSD.Price)
		}
		return Quote{Price: price, AsOf: baseUSD.AsOf}, nil
	}

	switch {
	case !baseAnswered:
		return Quote{}, fmt.Errorf("%s: base leg %s/USD: %w", SyntheticCrossName, pair.Base.String(), baseFailures.err(basePair))
	case fx.sawDependent:
		return Quote{}, fmt.Errorf("%w: %s: every usable leg pair for %s shares one publisher",
			ErrPriceUnavailable, SyntheticCrossName, pair.String())
	default:
		return Quote{}, fmt.Errorf("%s: fx leg %s/USD: %w", SyntheticCrossName, pair.Quote.String(), fx.failures.err(fx.pair))
	}
}

// referencePublisher maps a reference label to the organisation that
// publishes it. The Reflector variants are one publisher's contracts and
// share its operator, so two of them are not independent readings.
func referencePublisher(name string) string {
	switch name {
	case OracleSourceReflectorDEX, OracleSourceReflectorCEX, OracleSourceReflectorFX:
		return "reflector"
	default:
		return name
	}
}

// legCandidates evaluates one leg's candidates lazily and at most once
// each, since several base legs may be paired against the same FX legs.
type legCandidates struct {
	refs         []Reference
	pair         canonical.Pair
	quotes       []Quote
	errs         []error
	done         []bool
	failures     legFailures
	sawDependent bool // a usable candidate was skipped as same-publisher
}

func newLegCandidates(refs []Reference, pair canonical.Pair) *legCandidates {
	return &legCandidates{
		refs:   refs,
		pair:   pair,
		quotes: make([]Quote, len(refs)),
		errs:   make([]error, len(refs)),
		done:   make([]bool, len(refs)),
	}
}

// firstIndependentOf returns the first usable candidate whose publisher
// differs from publisher.
func (c *legCandidates) firstIndependentOf(ctx context.Context, publisher string, observedAt time.Time) (Quote, bool) {
	for i, ref := range c.refs {
		if !c.done[i] {
			c.quotes[i], c.errs[i] = lookupLeg(ctx, ref, c.pair, observedAt)
			c.done[i] = true
			if c.errs[i] != nil {
				c.failures.record(c.errs[i])
			}
		}
		if c.errs[i] != nil {
			continue
		}
		if referencePublisher(ref.Name()) == publisher {
			c.sawDependent = true
			continue
		}
		return c.quotes[i], true
	}
	return Quote{}, false
}

// lookupLeg asks one candidate for a leg; an unusable price or one older
// than the leg pair's [MaxComparableAge] is a transient failure, so a
// stale candidate falls through to the next.
func lookupLeg(ctx context.Context, ref Reference, pair canonical.Pair, observedAt time.Time) (Quote, error) {
	q, err := ref.LookupQuote(ctx, pair, observedAt)
	if err != nil {
		return Quote{}, err
	}
	if !isUsablePrice(q.Price) {
		return Quote{}, fmt.Errorf("%s returned unusable %v", ref.Name(), q.Price)
	}
	if err := checkComparable(q, pair, observedAt); err != nil {
		return Quote{}, fmt.Errorf("%s: %w", ref.Name(), err)
	}
	return q, nil
}

// legFailures preserves Compare's unsupported-vs-degraded distinction for an exhausted leg: any transient candidate failure makes the leg
// "unavailable"; it is unsupported only when every candidate reports unsupported.
type legFailures struct {
	sawTransient bool
	lastErr      error
}

func (f *legFailures) record(err error) {
	f.lastErr = err
	if !errors.Is(err, ErrAssetUnsupported) {
		f.sawTransient = true
	}
}

func (f *legFailures) err(pair canonical.Pair) error {
	if f.sawTransient {
		// Deliberately NOT %w on lastErr: it may itself wrap ErrAssetUnsupported, and double-wrapping would match BOTH sentinels, but Compare must see exactly ErrPriceUnavailable.
		return fmt.Errorf("%w: %s", ErrPriceUnavailable, f.lastErr.Error())
	}
	return fmt.Errorf("%w: no leg lists %s", ErrAssetUnsupported, pair.String())
}

// isUsablePrice rejects the values that would poison a division or a
// median: zero, negatives, NaN, ±Inf.
func isUsablePrice(p float64) bool {
	return p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0)
}
