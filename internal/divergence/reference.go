package divergence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Reference is a pluggable external price reference; implementations MUST be safe for concurrent LookupQuote calls, SHOULD set a per-call timeout via ctx, and SHOULD return ErrAssetUnsupported for an unlisted asset.
type Reference interface {
	// Name returns a stable, lowercase, hyphenated label for Prometheus labels and JSON keys (e.g. "coingecko", "reflector-cex"); renaming
	// is a wire break against the divergence-warning alerts.
	Name() string

	// LookupQuote returns the source's price as `1 base = N quote` and the instant its upstream observed it. The quote's AsOf is required: [Compare] refuses a quote it cannot age (see [MaxComparableAge]).
	LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error)
}

// Quote is one reference's price and when its upstream observed it.
type Quote struct {
	//floatmoney:ok external reference price used only for divergence comparison against our own VWAP, never stored or served as canonical money
	Price float64
	AsOf  time.Time
}

// ErrTooStaleToCompare means the reference answered but its price was observed longer than MaxComparableAge before the comparison, describing a different market than the short-window VWAP.
var ErrTooStaleToCompare = errors.New("divergence: reference too stale to compare")

// Comparability ceilings for [MaxComparableAge]. Non-FX pairs get 1h (the longest heartbeat of a live-market reference; older quotes cannot describe a minutes-wide VWAP's market).
// FX pairs get the FX liveness budget, which already bounds their error since FX quotes pause over closes and move far less than the threshold per day.
const (
	DefaultMaxComparableAge   = time.Hour
	DefaultMaxComparableAgeFX = 76 * time.Hour // a 24h FX heartbeat plus a weekend close
)

// MaxComparableAge is the oldest a reference quote for pair may be, relative to the comparison time, and still vote in Compare.
func MaxComparableAge(pair canonical.Pair) time.Duration {
	if pair.Base.Type == canonical.AssetFiat && pair.Quote.Type == canonical.AssetFiat {
		return DefaultMaxComparableAgeFX
	}
	return DefaultMaxComparableAge
}

// checkComparable rejects a quote with no observation time or one older than
// MaxComparableAge(pair) at observedAt (wall time when zero).
func checkComparable(q Quote, pair canonical.Pair, observedAt time.Time) error {
	if q.AsOf.IsZero() {
		return fmt.Errorf("%w: quote carries no observation time", ErrTooStaleToCompare)
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if age, ceiling := observedAt.Sub(q.AsOf), MaxComparableAge(pair); age > ceiling {
		return fmt.Errorf("%w: observed %s before the comparison, max %s",
			ErrTooStaleToCompare, age.Truncate(time.Second), ceiling)
	}
	return nil
}

// Sentinel errors returned by [Reference] implementations; Compare keeps "asset not listed on this source" (information) apart from transport failures (a transient outage signal).
var (
	// ErrAssetUnsupported — the source does not list the asset
	// (or the pair). Caller treats this as "no reference for this
	// pair on this source", not as a degradation.
	ErrAssetUnsupported = errors.New("divergence: asset unsupported by reference")

	// ErrPriceUnavailable — the source supports the asset but
	// has no recent price (vendor outage, stale feed). Treated
	// as a transient failure; surfaces in [Result.Failures].
	ErrPriceUnavailable = errors.New("divergence: price unavailable")
)
