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

// On-chain decimals() verification for the Chainlink reference: a configured `decimals` never checked against the proxy would silently mis-scale every reading by 10^(configured-actual).
// Read on first use and every chainlinkDecimalsRefreshInterval: absent adopts the chain; equal flows; DIFFERENT logs ERROR, counts obs.ChainlinkFeedDecimalsMismatchTotal and REFUSES the feed (ErrPriceUnavailable);
// an RPC failure keeps the last known value with a WARN and retries after chainlinkDecimalsRetryInterval, and a feed with no value and no read is refused, never served at a guessed scale.
const (
	// chainlinkDecimalsRefreshInterval is how long a verified decimals() read is trusted; daily suffices, and re-reading catches a proxy re-pointed at a differently-scaled aggregator.
	chainlinkDecimalsRefreshInterval = 24 * time.Hour

	// chainlinkDecimalsRetryInterval bounds how often a feed whose
	// decimals() call failed, or whose value disagrees with config, is
	// re-read — one WARN/ERROR per feed per interval rather than one per
	// LookupQuote.
	chainlinkDecimalsRetryInterval = 5 * time.Minute

	// chainlinkDecimalsSelector = keccak256("decimals()")[:4].
	// AggregatorV3Interface.decimals() returns uint8, ABI-padded to
	// one 32-byte word.
	chainlinkDecimalsSelector = "0x313ce567"
)

// ErrChainlinkDecimalsMismatch: configured decimals disagree with on-chain decimals(). Always wrapped with [ErrPriceUnavailable] so [Compare] classifies it as price_unavailable; exposed for errors.Is.
var ErrChainlinkDecimalsMismatch = errors.New("divergence: chainlink configured decimals disagree with on-chain decimals()")

// chainlinkDecimalsState is the per-feed memory keyed by lowercase address; onChain is a property of the ADDRESS and shared across pairs, but agreement with a caller's configured decimals is not,
// so that verdict is never cached here and is recomputed per call in decimalsVerdict.
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
		// Keep whatever we knew; never guess a scale for an unknown feed. Counted apart from the mismatch counter because this fail-OPEN path keeps readings flowing, so a WARN alone gives nothing to alert on.
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

// decimalsVerdict turns the per-feed state into a scale or a refusal; caller holds r.decMu. The mismatch check uses THIS call's spec.Decimals, never a value cached on st
// (shared by every pair on the address), so one pair's mismatch is never served to another pair that agrees.
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

// decodeChainlinkDecimals parses the 32-byte `decimals()` result (uint8, one word). It rejects any other shape and values outside 1..255 (a 0-decimal Chainlink feed does not exist, so 0 means a broken contract).
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
