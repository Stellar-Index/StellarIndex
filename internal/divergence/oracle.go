package divergence

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// OracleReader is the narrow served-tier read seam the on-chain
// oracle references consume. Production wires
// `internal/storage/timescale.(*Store)` (method
// LatestOracleObservation); tests substitute a fake.
//
// The contract deliberately returns (nil, nil) — not a storage
// sentinel error — when the source has never observed any
// (baseKeys × quoteKeys) combination, so this package can map
// "oracle doesn't cover the pair" to [ErrAssetUnsupported] without
// importing the storage package's ErrNotFound.
type OracleReader interface {
	// LatestOracleObservation returns the single most-recent
	// oracle_updates row for `source` whose asset matches ANY of
	// baseKeys AND whose quote matches ANY of quoteKeys. Returns
	// (nil, nil) when no row matches.
	LatestOracleObservation(ctx context.Context, source string, baseKeys, quoteKeys []string) (*canonical.OracleUpdate, error)
}

// Oracle source labels as stamped on oracle_updates rows by the
// ingest decoders (see internal/canonical/oracle.go — Source is
// part of row identity, stable across versions). Reference.Name()
// reuses the same label so the /divergences feed, Prometheus
// failure maps, and the oracle_updates table all speak one
// vocabulary per source.
const (
	OracleSourceReflectorDEX = "reflector-dex"
	OracleSourceReflectorCEX = "reflector-cex"
	OracleSourceReflectorFX  = "reflector-fx"
	OracleSourceRedstone     = "redstone"
	OracleSourceBand         = "band"
)

// Staleness ceilings per oracle family (the CS-089 discipline
// applied to served rows: a frozen feed must read as "reference
// unavailable", never as agreement/divergence).
//
//   - Reflector publishes every ~5 minutes per contract; 30m means
//     "missed five rounds + slack".
//   - Redstone batch-pushes on deviation triggers with a daily-ish
//     heartbeat floor; 26h tolerates one missed heartbeat + slack.
//   - Band relays are relayer-driven and sparse (hours apart); 26h
//     matches the Redstone rationale.
//
// Operators tune via `[divergence.<oracle>].max_age_minutes`.
const (
	DefaultOracleMaxAgeReflector = 30 * time.Minute
	DefaultOracleMaxAgeRedstone  = 26 * time.Hour
	DefaultOracleMaxAgeBand      = 26 * time.Hour
)

// OracleReference is a [Reference] backed by our OWN ingested
// on-chain oracle rows (the `oracle_updates` served tier) rather
// than an outbound HTTP call. One instance per oracle source label
// ("reflector-dex", "reflector-cex", "reflector-fx", "redstone",
// "band").
//
// Unlike CoinGecko / Chainlink this closes the loop against data we
// already captured from the chain: the comparison answers "does the
// value the on-chain consumer (e.g. Blend) sees agree with our
// VWAP?" — an independent-methodology check even though the bytes
// flowed through our indexer, because the oracle's upstream price
// discovery is not ours.
//
// Scale discipline (ADR-0003): oracle_updates stores the RAW integer
// price + a per-row decimals column (Reflector 14, Redstone 8, Band
// single-asset 9; Band's E18 pair rates are computed on-read
// upstream and never stored, so they never reach this path). The
// price is scaled via big.Rat — never int64 truncation — and only
// collapses to float64 at the [Reference] interface boundary, same
// as the Chainlink reference.
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
	default:
		return DefaultOracleMaxAgeReflector
	}
}

// Name implements [Reference].
func (r *OracleReference) Name() string { return r.source }

// LookupPrice implements [Reference].
//
// Pair mapping: both sides of the pair are expanded through
// [oracleAssetKeys] — XLM's dual identity (`native` on-chain form vs
// the abstract `crypto:XLM` ticker the CEX-class oracles publish
// under) is the one translation applied; every other asset must
// match its canonical string exactly. Which pairs each oracle
// actually covers falls out of the stored rows:
//
//   - reflector-dex   — Soroban token assets quoted in fiat:USD (the
//     DEX oracle's base is the USDC SAC, stamped as
//     fiat:USD — see reflector.quoteForVariant)
//   - reflector-cex   — crypto tickers quoted in fiat:USD
//   - reflector-fx    — fiat codes quoted in fiat:USD
//   - redstone        — per-feed base quoted in fiat:USD (EUROC→EUR)
//   - band            — crypto/fiat symbols quoted in fiat:USD
//
// No inversion or cross-quote triangulation is attempted — a pair
// the oracle doesn't publish directly returns [ErrAssetUnsupported]
// (information for the operator, not a degradation).
func (r *OracleReference) LookupPrice(ctx context.Context, pair canonical.Pair, observedAt time.Time) (float64, error) {
	u, err := r.reader.LatestOracleObservation(ctx,
		r.source, oracleAssetKeys(pair.Base), oracleAssetKeys(pair.Quote))
	if err != nil {
		return 0, fmt.Errorf("oracle %s: read latest observation for %s: %w",
			r.source, pair.String(), err)
	}
	if u == nil {
		return 0, fmt.Errorf("%w: oracle %s has no observation for %s",
			ErrAssetUnsupported, r.source, pair.String())
	}

	// Staleness gate (CS-089 analogue). observedAt is the comparison
	// timestamp Compare passes through; zero falls back to wall time
	// defensively, mirroring the Chainlink reference.
	asOf := observedAt
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	if age := asOf.Sub(u.Timestamp); age > r.maxAge {
		return 0, fmt.Errorf("%w: oracle %s observation for %s is stale (observed %s ago, max %s)",
			ErrPriceUnavailable, r.source, pair.String(), age.Truncate(time.Second), r.maxAge)
	}

	if u.Price.Sign() <= 0 {
		return 0, fmt.Errorf("oracle %s: non-positive price for %s: %s",
			r.source, pair.String(), u.Price.String())
	}
	return scaleOracleAmount(u.Price.BigInt(), int(u.Decimals))
}

// oracleAssetKeys returns the canonical asset-key strings an oracle
// row may carry for the given asset. XLM's dual identity is the one
// expansion (same translation the v1 handler applies when reading
// oracle_updates — see timescale.LatestOracleUpdatesForAssets):
// Reflector-CEX / Band publish XLM under the abstract `crypto:XLM`
// ticker while our on-chain pairs use the protocol `native` form.
func oracleAssetKeys(a canonical.Asset) []string {
	key := a.String()
	switch key {
	case "native":
		return []string{key, "crypto:XLM"}
	case "crypto:XLM":
		return []string{key, "native"}
	default:
		return []string{key}
	}
}

// scaleOracleAmount divides raw by 10^decimals via big.Rat and
// collapses to float64 only at the return boundary (ADR-0003 — the
// integer path never truncates; float64 is the [Reference] wire
// shape and the divergence threshold is percentage-based, same
// trade-off as scaleChainlinkAnswer). The 38 ceiling comfortably
// covers every stored scale (Reflector 14, Band 9, Redstone 8) with
// room for an upstream E18-class rescale.
func scaleOracleAmount(raw *big.Int, decimals int) (float64, error) {
	if raw == nil {
		return 0, errors.New("nil price")
	}
	if decimals < 0 || decimals > 38 {
		return 0, fmt.Errorf("decimals %d out of range [0, 38]", decimals)
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Rat).SetFrac(raw, div).Float64()
	return f, nil
}
