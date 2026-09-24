package divergence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Reference is a pluggable external price reference. Each
// implementation maps to one external oracle / aggregator
// (CoinGecko, CMC, Reflector, Band, etc.) and exposes one method
// to fetch the price the source reports for a given pair.
//
// Implementations:
//
//   - MUST be safe for concurrent LookupQuote calls
//   - SHOULD set a per-call timeout via the supplied ctx (caller
//     also bounds it but the implementation is the closest layer
//     to the network)
//   - SHOULD return [ErrAssetUnsupported] when the source doesn't
//     list the asset (so [Compare] can record the gap without
//     treating it as a transport failure)
type Reference interface {
	// Name returns a stable, lowercase, hyphenated label suitable
	// for Prometheus labels and JSON keys (e.g. "coingecko",
	// "coinmarketcap", "reflector-cex"). Stable across versions —
	// renaming is a wire break against the divergence-warning alerts.
	Name() string

	// LookupQuote returns the source's reported price for the pair,
	// denominated as `1 base = N quote`, and the instant its upstream
	// observed that price. observedAt is the comparison time; the
	// quote's AsOf is required, because [Compare] refuses a quote it
	// cannot age (see [MaxComparableAge]).
	LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error)
}

// Quote is one reference's price and when its upstream observed it.
type Quote struct {
	Price float64
	AsOf  time.Time
}

// ErrTooStaleToCompare — the reference answered, but its price was
// observed longer ago than [MaxComparableAge] before the comparison:
// live by its own budget, yet describing a different market than the
// short-window VWAP it would be compared with.
var ErrTooStaleToCompare = errors.New("divergence: reference too stale to compare")

// Comparability ceilings for [MaxComparableAge].
//
//   - Non-FX pairs: 1h, the longest heartbeat of any reference meant to
//     track a live market (Chainlink crypto/USD, ≤1h). An older quote is
//     from a feed that is overdue or only heartbeats daily (Redstone,
//     Band: 26h liveness), and cannot be assumed to describe the market
//     a minutes-wide VWAP measures.
//   - FX pairs (fiat/fiat): the FX liveness budget. FX quotes pause over
//     market closes and move far less than the divergence threshold per
//     day, so their liveness budget already bounds their error.
const (
	DefaultMaxComparableAge   = time.Hour
	DefaultMaxComparableAgeFX = defaultChainlinkMaxAgeFX
)

// MaxComparableAge is the oldest a reference quote for pair may be,
// relative to the comparison time, and still vote in [Compare].
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

// Sentinel errors returned by [Reference] implementations.
//
// Compare distinguishes these from generic transport errors so
// "asset not listed on this source" doesn't pollute the
// degradation signal. An asset that genuinely isn't listed on
// CoinGecko is information for the operator (consider adding the
// pair to the source's supported list, or accept a smaller
// reference universe for that pair); a transport failure is a
// transient outage signal.
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
