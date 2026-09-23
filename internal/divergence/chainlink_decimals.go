package divergence

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// On-chain decimals() verification for the Chainlink reference.
//
// The feed's `decimals` used to be STATIC config (defaulting to 8) that
// the code never checked against the AggregatorV3 proxy's `decimals()`
// view — so a wrong or drifted configured value scaled every reading
// by 10^(configured-actual) silently, producing a permanent false
// divergence (or, worse, masking a real one). The reference now reads
// `decimals()` from each feed over the same JSON-RPC path it uses for
// `latestRoundData()`, on first use and again every
// chainlinkDecimalsRefreshInterval, and:
//
//   - configured value ABSENT (0) → adopts the on-chain value;
//   - both present and EQUAL → readings flow;
//   - both present and DIFFERENT → logs at ERROR with both values,
//     counts obs.ChainlinkFeedDecimalsMismatchTotal on every refused
//     reading, and REFUSES the feed (ErrPriceUnavailable) until they
//     agree — a divergence check that scales wrongly is worse than none;
//   - decimals() RPC failure → keeps the last known value (configured,
//     or the previously verified on-chain value) with a WARN and
//     retries after chainlinkDecimalsRetryInterval; a feed with no
//     configured value and no successful read yet is refused, never
//     served at a guessed scale.
const (
	// chainlinkDecimalsRefreshInterval is how long a verified
	// decimals() read stays trusted before it is re-read. A proxy's
	// decimals is effectively immutable, so daily is plenty; the
	// refresh exists so a proxy re-pointed at a differently-scaled
	// aggregator cannot mis-scale for the lifetime of the process.
	chainlinkDecimalsRefreshInterval = 24 * time.Hour

	// chainlinkDecimalsRetryInterval bounds how often a feed whose
	// decimals() call failed, or whose value disagrees with config, is
	// re-read — one WARN/ERROR per feed per interval rather than one per
	// LookupPrice.
	chainlinkDecimalsRetryInterval = 5 * time.Minute

	// chainlinkDecimalsSelector = keccak256("decimals()")[:4].
	// AggregatorV3Interface.decimals() returns uint8, ABI-padded to
	// one 32-byte word.
	chainlinkDecimalsSelector = "0x313ce567"
)

// ErrChainlinkDecimalsMismatch — the feed's configured decimals
// disagrees with the aggregator's on-chain decimals(). Always wrapped
// together with [ErrPriceUnavailable] so [Compare] classifies the
// refusal as price_unavailable ("reference unavailable this run"), and
// exposed on its own so operators and tests can errors.Is the cause.
var ErrChainlinkDecimalsMismatch = errors.New("divergence: chainlink configured decimals disagree with on-chain decimals()")

// chainlinkDecimalsState is the per-feed verification memory, keyed by
// lowercase feed address in ChainlinkReference.decimals. onChain is a
// property of the ADDRESS, so it is safe to share across every
// canonical pair mapped to that address. Whether it agrees with a
// caller's configured decimals is NOT a property of the address — two
// pairs can point at the same feed with different (or absent)
// configured values — so that verdict is never cached here; it is
// recomputed per call in decimalsVerdict against the caller's own
// spec.Decimals.
type chainlinkDecimalsState struct {
	onChain   int       // last successfully read decimals(); valid when hasChain
	hasChain  bool      // at least one decimals() read has succeeded
	nextCheck time.Time // do not re-read decimals() before this instant
	lastErr   error     // most recent decimals() failure, for the refusal message
}

// resolveDecimals returns the decimals to scale `pair`'s feed by, or
// an ErrPriceUnavailable-wrapped error when the feed must be refused.
// Safe for concurrent callers (Reference contract); the RPC read runs
// outside the lock so a slow endpoint does not block sibling feeds.
func (r *ChainlinkReference) resolveDecimals(ctx context.Context, pair canonical.Pair, spec chainlinkFeedSpec) (int, error) {
	now := r.now()
	key := strings.ToLower(spec.Address)

	r.decMu.Lock()
	st, ok := r.decimals[key]
	if ok && now.Before(st.nextCheck) {
		defer r.decMu.Unlock()
		return r.decimalsVerdict(pair, spec, st)
	}
	r.decMu.Unlock()

	onChain, err := r.fetchDecimals(ctx, spec.Address)

	r.decMu.Lock()
	defer r.decMu.Unlock()
	st, ok = r.decimals[key]
	if !ok {
		st = &chainlinkDecimalsState{}
		r.decimals[key] = st
	}
	if err != nil {
		// Keep whatever we knew (configured, or a prior on-chain read);
		// never guess a scale for a feed we know nothing about. Counted
		// separately from the mismatch counter: this is the fail-OPEN
		// path (readings keep flowing at the last known scale), so a
		// WARN log alone gives an operator nothing to alert on.
		st.lastErr = err
		st.nextCheck = now.Add(chainlinkDecimalsRetryInterval)
		obs.ChainlinkFeedDecimalsVerifyFailedTotal.WithLabelValues("divergence", pair.String()).Inc()
		r.logger.Warn("chainlink decimals() read failed — keeping last known value, will retry",
			"source", ChainlinkSourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"configured_decimals", spec.Decimals,
			"known_onchain", st.hasChain,
			"retry_in", chainlinkDecimalsRetryInterval,
			"err", err)
		return r.decimalsVerdict(pair, spec, st)
	}

	st.onChain = onChain
	st.hasChain = true
	st.lastErr = nil
	if spec.Decimals != 0 && spec.Decimals != onChain {
		st.nextCheck = now.Add(chainlinkDecimalsRetryInterval)
		r.logger.Error("chainlink feed decimals mismatch — refusing readings until config and chain agree",
			"source", ChainlinkSourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"configured_decimals", spec.Decimals,
			"onchain_decimals", onChain)
		return r.decimalsVerdict(pair, spec, st)
	}
	st.nextCheck = now.Add(chainlinkDecimalsRefreshInterval)
	if spec.Decimals == 0 {
		r.logger.Info("chainlink feed decimals adopted from chain (none configured)",
			"source", ChainlinkSourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"onchain_decimals", onChain)
	}
	return r.decimalsVerdict(pair, spec, st)
}

// decimalsVerdict turns the per-feed state into a scale or a refusal.
// Caller holds r.decMu. The mismatch check is against THIS call's own
// spec.Decimals, never a value cached on st: st is shared by every
// canonical pair mapped to the same feed address, so a mismatch found
// for one pair's configured decimals must never be served — from the
// resolveDecimals fast-path cache hit — to a different pair whose
// configured value agrees (or asserts none).
func (r *ChainlinkReference) decimalsVerdict(pair canonical.Pair, spec chainlinkFeedSpec, st *chainlinkDecimalsState) (int, error) {
	switch {
	case st.hasChain && spec.Decimals != 0 && spec.Decimals != st.onChain:
		obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String()).Inc()
		return 0, fmt.Errorf("%w: %w: %s configured decimals=%d, on-chain decimals()=%d",
			ErrPriceUnavailable, ErrChainlinkDecimalsMismatch, pair.String(), spec.Decimals, st.onChain)
	case st.hasChain:
		// Verified equal, or adopted because nothing was configured.
		return st.onChain, nil
	case spec.Decimals != 0:
		// decimals() has never succeeded; the configured value is the
		// best we have and is what the operator asserted.
		return spec.Decimals, nil
	default:
		return 0, fmt.Errorf("%w: chainlink: %s decimals unresolved (none configured and decimals() unavailable): %w",
			ErrPriceUnavailable, pair.String(), st.lastErr)
	}
}

// fetchDecimals performs eth_call decimals() against the feed and
// decodes the uint8 result.
func (r *ChainlinkReference) fetchDecimals(ctx context.Context, address string) (int, error) {
	result, err := r.ethCall(ctx, address, chainlinkDecimalsSelector)
	if err != nil {
		return 0, err
	}
	dec, err := decodeChainlinkDecimals(result)
	if err != nil {
		return 0, fmt.Errorf("chainlink: decode decimals() for %s: %w", address, err)
	}
	return dec, nil
}

// decodeChainlinkDecimals parses the 32-byte `decimals()` eth_call
// result (uint8, left-padded to one word). Rejects anything that is
// not exactly one word — a proxy answering a different shape must fail
// loudly — and a value outside 1..255: uint8 cannot exceed 255, and a
// 0-decimal price feed does not exist on Chainlink, so 0 is treated as
// a broken contract rather than a scale.
func decodeChainlinkDecimals(hexStr string) (int, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(hexStr, "0x"))
	if err != nil {
		return 0, fmt.Errorf("decimals hex: %w", err)
	}
	if len(raw) != 32 {
		return 0, fmt.Errorf("decimals result: want 32 bytes, got %d", len(raw))
	}
	v := new(big.Int).SetBytes(raw)
	if !v.IsInt64() || v.Int64() < 1 || v.Int64() > 255 { // i128:ok IsInt64 range-checked first; decimals() is 1..255
		return 0, fmt.Errorf("decimals result %s outside uint8 range 1..255", v.String())
	}
	return int(v.Int64()), nil // i128:ok range-checked 1..255 above
}
