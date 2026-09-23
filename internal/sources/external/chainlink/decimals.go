package chainlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// On-chain decimals() verification for the ingest poller.
//
// FeedSpec.Decimals used to be taken from config (or the built-in 8)
// and stamped onto every oracle_updates row without ever asking the
// AggregatorV3 proxy what scale it actually publishes at — a wrong or
// drifted value would have stored every reading 10^(configured-actual)
// off, silently. The poller now reads `decimals()` (SelDecimals) over
// the same Client it uses for latestRoundData(), on the first poll of
// each feed and again every decimalsRefreshInterval, and:
//
//   - configured value ABSENT (0) → adopts the on-chain value;
//   - both present and EQUAL → readings flow;
//   - both present and DIFFERENT → logs at ERROR with both values,
//     counts obs.ChainlinkFeedDecimalsMismatchTotal on every refused
//     reading, and REFUSES the feed (ErrDecimalsMismatch) until they
//     agree — fail closed, a mis-scaled oracle row is worse than none;
//   - decimals() RPC failure → keeps the last known value (configured,
//     or the previously verified on-chain value) with a WARN and
//     retries after decimalsRetryInterval; a feed with no configured
//     value and no successful read yet is refused (ErrDecimalsUnresolved),
//     never projected at a guessed scale.
//
// Backfill goes through the same resolver, so a historical walk of a
// mis-scaled feed is refused too.
const (
	// decimalsRefreshInterval is how long a verified decimals() read
	// stays trusted before it is re-read. A proxy's decimals is
	// effectively immutable, so daily is plenty; the refresh exists so
	// a proxy re-pointed at a differently-scaled aggregator cannot
	// mis-scale for the lifetime of the process.
	decimalsRefreshInterval = 24 * time.Hour

	// decimalsRetryInterval bounds how often a feed whose decimals()
	// call failed, or whose value disagrees with config, is re-read —
	// one WARN/ERROR per feed per interval rather than one per tick.
	decimalsRetryInterval = 5 * time.Minute
)

// Errors returned by the decimals resolver. Callers classify via
// errors.Is; PollOnce surfaces them through the per-feed warn path.
var (
	// ErrDecimalsMismatch — the feed's configured decimals disagrees
	// with the aggregator's on-chain decimals(). The feed is refused
	// until they agree.
	ErrDecimalsMismatch = errors.New("chainlink: configured decimals disagree with on-chain decimals()")

	// ErrDecimalsUnresolved — no decimals is configured for the feed
	// and decimals() has not been read successfully yet, so no scale
	// is known. Also returned by project for a literal 0.
	ErrDecimalsUnresolved = errors.New("chainlink: feed decimals unresolved")
)

// feedDecimalsState is the per-feed verification memory.
type feedDecimalsState struct {
	onChain   uint8     // last successfully read decimals(); valid when hasChain
	hasChain  bool      // at least one decimals() read has succeeded
	mismatch  bool      // last successful read disagreed with the configured value
	nextCheck time.Time // do not re-read decimals() before this instant
	lastErr   error     // most recent decimals() failure, for the refusal message
}

// decimalsCache holds feedDecimalsState per feed address (lowercase),
// mirroring roundCache's keying. Goroutine-safe: PollOnce resolves
// feeds concurrently.
type decimalsCache struct {
	mu    sync.Mutex
	feeds map[string]*feedDecimalsState
}

func newDecimalsCache() *decimalsCache {
	return &decimalsCache{feeds: make(map[string]*feedDecimalsState)}
}

// resolveDecimals returns the decimals to stamp on `pair`'s rows, or
// an error when the feed must be refused this tick. The RPC read runs
// outside the cache lock so a slow endpoint does not block siblings.
func (p *Poller) resolveDecimals(ctx context.Context, pair canonical.Pair, spec FeedSpec) (uint8, error) {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := p.clock()
	key := strings.ToLower(spec.Address)
	cache := p.decimals

	cache.mu.Lock()
	st, ok := cache.feeds[key]
	if ok && now.Before(st.nextCheck) {
		defer cache.mu.Unlock()
		return decimalsVerdict(pair, spec, st)
	}
	cache.mu.Unlock()

	onChain, err := p.fetchDecimals(ctx, spec)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	st, ok = cache.feeds[key]
	if !ok {
		st = &feedDecimalsState{}
		cache.feeds[key] = st
	}
	if err != nil {
		// Keep whatever we knew (configured, or a prior on-chain read);
		// never guess a scale for a feed we know nothing about.
		st.lastErr = err
		st.nextCheck = now.Add(decimalsRetryInterval)
		logger.Warn("chainlink decimals() read failed — keeping last known value, will retry",
			"source", SourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"configured_decimals", spec.Decimals,
			"known_onchain", st.hasChain,
			"retry_in", decimalsRetryInterval,
			"err", err)
		return decimalsVerdict(pair, spec, st)
	}

	st.onChain = onChain
	st.hasChain = true
	st.lastErr = nil
	st.mismatch = spec.Decimals != 0 && spec.Decimals != onChain
	if st.mismatch {
		st.nextCheck = now.Add(decimalsRetryInterval)
		logger.Error("chainlink feed decimals mismatch — refusing readings until config and chain agree",
			"source", SourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"configured_decimals", spec.Decimals,
			"onchain_decimals", onChain)
		return decimalsVerdict(pair, spec, st)
	}
	st.nextCheck = now.Add(decimalsRefreshInterval)
	if spec.Decimals == 0 {
		logger.Info("chainlink feed decimals adopted from chain (none configured)",
			"source", SourceName,
			"pair", pair.String(),
			"address", spec.Address,
			"onchain_decimals", onChain)
	}
	return decimalsVerdict(pair, spec, st)
}

// decimalsVerdict turns the per-feed state into a scale or a refusal.
// Caller holds the cache lock.
func decimalsVerdict(pair canonical.Pair, spec FeedSpec, st *feedDecimalsState) (uint8, error) {
	switch {
	case st.mismatch:
		obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("ingest", pair.String()).Inc()
		return 0, fmt.Errorf("%w: %s configured decimals=%d, on-chain decimals()=%d",
			ErrDecimalsMismatch, pair.String(), spec.Decimals, st.onChain)
	case st.hasChain:
		// Verified equal, or adopted because nothing was configured.
		return st.onChain, nil
	case spec.Decimals != 0:
		// decimals() has never succeeded; the configured value is the
		// best we have and is what the operator asserted.
		return spec.Decimals, nil
	default:
		return 0, fmt.Errorf("%w: %s none configured and decimals() unavailable: %w",
			ErrDecimalsUnresolved, pair.String(), st.lastErr)
	}
}

// fetchDecimals performs eth_call decimals() against the feed and
// decodes the uint8 result.
func (p *Poller) fetchDecimals(ctx context.Context, spec FeedSpec) (uint8, error) {
	rawHex, err := p.Client.EthCall(ctx, spec.Address, SelDecimals, "latest")
	if err != nil {
		return 0, fmt.Errorf("eth_call decimals(): %w", err)
	}
	return decodeDecimals(rawHex)
}

// decodeDecimals parses the 32-byte `decimals()` eth_call result
// (uint8, left-padded to one word). Rejects anything that is not
// exactly one word — a proxy answering a different shape must fail
// loudly — and a value outside 1..255: uint8 cannot exceed 255, and a
// 0-decimal price feed does not exist on Chainlink, so 0 is treated as
// a broken contract rather than a scale.
func decodeDecimals(rawHex string) (uint8, error) {
	b, err := hexBytes(rawHex)
	if err != nil {
		return 0, fmt.Errorf("%w: decimals() hex: %w", ErrMalformedResult, err)
	}
	if len(b) != 32 {
		return 0, fmt.Errorf("%w: decimals() expected 32 bytes, got %d", ErrMalformedResult, len(b))
	}
	v := new(big.Int).SetBytes(b)
	if !v.IsInt64() || v.Int64() < 1 || v.Int64() > 255 {
		return 0, fmt.Errorf("%w: decimals() %s outside uint8 range 1..255", ErrMalformedResult, v.String())
	}
	return uint8(v.Int64()), nil
}
