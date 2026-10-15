package divergence

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// OracleReader is the narrow served-tier read seam for the on-chain oracle references; it returns (nil, nil) when the source never observed any (baseKeys x quoteKeys) combination, mapped to ErrAssetUnsupported.
type OracleReader interface {
	// LatestOracleObservation returns the single most-recent
	// oracle_updates row for `source` whose asset matches ANY of
	// baseKeys AND whose quote matches ANY of quoteKeys. Returns
	// (nil, nil) when no row matches.
	LatestOracleObservation(ctx context.Context, source string, baseKeys, quoteKeys []string) (*canonical.OracleUpdate, error)
}

// Oracle source labels as stamped on oracle_updates rows (internal/canonical/oracle.go; Source is part of row identity); Reference.Name() reuses them so every surface speaks one vocabulary.
const (
	OracleSourceReflectorDEX = "reflector-dex"
	OracleSourceReflectorCEX = "reflector-cex"
	OracleSourceReflectorFX  = "reflector-fx"
	OracleSourceRedstone     = "redstone"
	OracleSourceBand         = "band"
	// OracleSourceChainlinkStellar is Chainlink's DataFeedsCache on Stellar, not the retired
	// Ethereum reference.
	OracleSourceChainlinkStellar = "chainlink-stellar"
)

// Staleness ceilings per oracle family: a frozen feed must read "reference unavailable", never agreement/divergence. Reflector publishes every ~5m (30m = five missed rounds + slack);
// Redstone and Band are sparse or deviation-triggered with daily-ish heartbeats (26h = one missed heartbeat + slack).
const (
	DefaultOracleMaxAgeReflector = 30 * time.Minute
	DefaultOracleMaxAgeRedstone  = 26 * time.Hour
	DefaultOracleMaxAgeBand      = 26 * time.Hour
	// Chainlink feeds publish on deviation or a 24h heartbeat.
	DefaultOracleMaxAgeChainlinkStellar = 26 * time.Hour
)

// OracleReference is a Reference over our OWN ingested `oracle_updates` rows, one per source label; reflector-dex prices the same SDEX book our VWAP aggregates, so NewService drops it (see CorrelatedWithOurVWAP). Prices scale the RAW integer via big.Rat (ADR-0003), collapsing to float64 only at the Reference boundary.
type OracleReference struct {
	source string
	reader OracleReader
	maxAge time.Duration
}

// OracleReferenceOptions configures [NewOracleReference].
type OracleReferenceOptions struct {
	// Source is the oracle_updates source label to read AND the
	// Name() this reference reports. Required. Use the
	// OracleSource* constants.
	Source string

	// Reader is the served-tier read seam. Required.
	Reader OracleReader

	// MaxAge is the staleness ceiling: an observation older than
	// this (relative to the comparison's observedAt) is rejected as
	// [ErrPriceUnavailable]. <= 0 falls back to the per-family
	// default for known sources, else DefaultOracleMaxAgeReflector.
	MaxAge time.Duration
}

// CorrelatedWithOurVWAP reports whether a reference's price discovery is the on-chain book our own VWAP aggregates, so it cannot corroborate or contradict it.
func CorrelatedWithOurVWAP(name string) bool {
	return name == OracleSourceReflectorDEX
}

// NewOracleReference constructs an on-chain oracle reference.
func NewOracleReference(opts OracleReferenceOptions) (*OracleReference, error) {
	if opts.Source == "" {
		return nil, errors.New("divergence: oracle reference Source is required")
	}
	if opts.Reader == nil {
		return nil, errors.New("divergence: oracle reference Reader is required")
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = defaultOracleMaxAge(opts.Source)
	}
	return &OracleReference{
		source: opts.Source,
		reader: opts.Reader,
		maxAge: maxAge,
	}, nil
}

// defaultOracleMaxAge maps a source label to its family default.
// Unknown sources get the tightest (Reflector) ceiling — safer to
// over-report price_unavailable than to serve a frozen feed.
func defaultOracleMaxAge(source string) time.Duration {
	switch source {
	case OracleSourceRedstone:
		return DefaultOracleMaxAgeRedstone
	case OracleSourceBand:
		return DefaultOracleMaxAgeBand
	case OracleSourceChainlinkStellar:
		return DefaultOracleMaxAgeChainlinkStellar
	default:
		return DefaultOracleMaxAgeReflector
	}
}

// Name implements [Reference].
func (r *OracleReference) Name() string { return r.source }

// LookupQuote implements Reference with AsOf the observation's ledger close time; both sides expand through the alias registry so XLM's three forms match, other assets match their canonical string exactly, and an unpublished pair is ErrAssetUnsupported (no inversion or triangulation).
func (r *OracleReference) LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error) {
	u, err := r.reader.LatestOracleObservation(ctx,
		r.source, canonical.AssetAliasStrings(pair.Base), canonical.AssetAliasStrings(pair.Quote))
	if err != nil {
		return Quote{}, fmt.Errorf("oracle %s: read latest observation for %s: %w",
			r.source, pair.String(), err)
	}
	if u == nil {
		return Quote{}, fmt.Errorf("%w: oracle %s has no observation for %s",
			ErrAssetUnsupported, r.source, pair.String())
	}
	// Defensive: a `raw:` row (unmapped oracle symbol, see canonical.AssetOracleRaw) can never match a mapped pair's exact keys; if a reader returns one anyway it is orientation-unknown and must never become a comparison.
	if !u.Asset.IsMapped() || !u.Quote.IsMapped() {
		return Quote{}, fmt.Errorf("%w: oracle %s observation for %s is an unmapped raw row (%s/%s)",
			ErrAssetUnsupported, r.source, pair.String(), u.Asset.String(), u.Quote.String())
	}

	// Staleness gate. observedAt is the comparison
	// timestamp Compare passes through; zero falls back to wall time
	// defensively.
	asOf := observedAt
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	if age := asOf.Sub(u.Timestamp); age > r.maxAge {
		return Quote{}, fmt.Errorf("%w: oracle %s observation for %s is stale (observed %s ago, max %s)",
			ErrPriceUnavailable, r.source, pair.String(), age.Truncate(time.Second), r.maxAge)
	}

	if u.Price.Sign() <= 0 {
		return Quote{}, fmt.Errorf("oracle %s: non-positive price for %s: %s",
			r.source, pair.String(), u.Price.String())
	}
	price, err := scaleOracleAmount(u.Price.BigInt(), int(u.Decimals))
	if err != nil {
		return Quote{}, err
	}
	return Quote{Price: price, AsOf: u.Timestamp}, nil
}

// scaleOracleAmount divides raw by 10^decimals via big.Rat and collapses to float64 only at the return (ADR-0003; the divergence threshold is percentage-based).
// The 38 ceiling covers every stored scale (Reflector 14, Band 9, Redstone 8) with room for an E18-class rescale.
func scaleOracleAmount(raw *big.Int, decimals int) (float64, error) {
	if raw == nil {
		return 0, errors.New("nil price")
	}
	if decimals < 0 || decimals > 38 {
		return 0, fmt.Errorf("decimals %d out of range [0, 38]", decimals)
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Rat).SetFrac(raw, div).Float64() // i128:ok reference price for a percentage cross-check; one correctly-rounded conversion of the exact ratio
	return f, nil
}
