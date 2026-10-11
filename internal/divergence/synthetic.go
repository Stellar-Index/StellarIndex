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
// Motivation: EUR/GBP-quoted pairs have exactly ONE direct reference
// (CoinGecko), below the divergence trust floor (divergenceMinSources), so
// their freezes can never auto-release and every one ends in an operator page.
// The on-chain oracles price the BASE in USD (reflector-cex, chainlink,
// redstone, band) and reflector-fx prices FIAT codes in USD; this reference
// composes them into a second, independent reading.
//
// Independence: the synthetic counts as one source in Compare's median and
// SuccessCount. Its legs do not answer non-USD-fiat-quoted pairs directly, so
// the same feed cannot contribute twice; if a leg source ever learns to, revisit
// before keeping both. The two legs must also come from different publishers
// (see [referencePublisher]): a cross of one publisher's feeds shares its
// failure modes, so with no independent leg pair the cross is unavailable.
//
// Both legs must be fresh (each leg's own MaxAge applies; no caching here),
// positive, and finite. A leg failure degrades to the sentinel that keeps
// Compare's bookkeeping honest: unsupported when no leg CAN answer, unavailable
// when a leg SHOULD have answered but didn't.
type SyntheticCrossReference struct {
	usdLegs []Reference // ordered candidates for base → fiat:USD
	fxLegs  []Reference // ordered candidates for fiat:X → fiat:USD
	usd     canonical.Asset
}

// SyntheticCrossOptions configures NewSyntheticCrossReference.
type SyntheticCrossOptions struct {
	// USDLegs are tried in order for the base-in-USD leg; the first
	// that answers and has an independent FX leg wins. Typically the on-chain oracle references
	// (reflector-cex, chainlink, redstone, band).
	USDLegs []Reference
	// FXLegs are tried in order for the fiat-in-USD leg, skipping any
	// from the base leg's publisher. Typically
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

// LookupQuote implements [Reference]. Only pairs quoted in a non-USD
// fiat are in scope; everything else is ErrAssetUnsupported (USD-quoted
// pairs already have the direct oracle references — a synthetic reading
// there would double-count the very feeds it is built from).
//
// Each leg is held to its own pair's [MaxComparableAge], and the cross
// is as old as its base leg: the FX leg's age is bounded by the FX
// ceiling, and FX moves far less per day than the divergence threshold.
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

// legFailures preserves Compare's unsupported-vs-degraded distinction for
// an exhausted leg: if ANY candidate failed transiently the leg is
// "unavailable" (a reading should have existed); only when every
// candidate reports unsupported is the leg unsupported for the pair.
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
		// Deliberately NOT %w on lastErr (errorlint appeased via
		// .Error()): the leg's last error may itself wrap
		// ErrAssetUnsupported, and double-wrapping would make this
		// error match BOTH sentinels — Compare's unsupported-vs-
		// degraded classification must see exactly ErrPriceUnavailable.
		return fmt.Errorf("%w: %s", ErrPriceUnavailable, f.lastErr.Error())
	}
	return fmt.Errorf("%w: no leg lists %s", ErrAssetUnsupported, pair.String())
}

// isUsablePrice rejects the values that would poison a division or a
// median: zero, negatives, NaN, ±Inf.
func isUsablePrice(p float64) bool {
	return p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0)
}
