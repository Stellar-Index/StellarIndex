package divergence

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	externalchainlink "github.com/Stellar-Index/StellarIndex/internal/sources/external/chainlink"
)

// ChainlinkReference is a [Reference] reading Chainlink AggregatorV3 `latestRoundData()` on Ethereum via eth_call, for the divergence cross-check ONLY (no VWAP/TWAP input).
type ChainlinkReference struct {
	httpClient *http.Client
	rpcURL     string
	logger     *slog.Logger
	now        func() time.Time // injectable clock for the decimals() cadence tests

	// FeedMap routes canonical pair string ("native/fiat:USD",
	// "fiat:EUR/fiat:USD", etc.) to a Chainlink AggregatorV3
	// contract address (0x-prefixed hex). Operator-curated.
	feedMap map[string]chainlinkFeedSpec

	// decimals is the per-feed on-chain decimals() verification state
	// (see chainlink_decimals.go), keyed by lowercase feed address.
	decMu    sync.Mutex
	decimals map[string]*chainlinkDecimalsState
}

// chainlinkFeedSpec holds one AggregatorV3 feed's address, asserted decimals (0 = none) and an optional inversion flag.
// Decimals is VERIFIED against the contract's on-chain `decimals()` (chainlink_decimals.go): absent adopts it, a disagreeing value refuses the feed; the resolver's scale is what applies.
type chainlinkFeedSpec struct {
	Address  string // 0x-prefixed
	Decimals int    // operator-asserted power-of-10; 0 = adopt on-chain decimals()
	Invert   bool
	// MaxAge is the staleness ceiling: an older round is rejected as ErrPriceUnavailable, since a frozen feed must read "unavailable", not agreement/divergence.
	// Per feed class: crypto/USD heartbeats at <=1h; FX heartbeats at 24h and pauses over market closes (~72h by Sunday), hence the two defaults in defaultChainlinkFeedMap.
	MaxAge time.Duration
}

// ChainlinkOptions configures [NewChainlinkReference].
type ChainlinkOptions struct {
	// HTTPClient — nil falls back to a 10s-timeout client.
	HTTPClient *http.Client

	// RPCURL is the Ethereum JSON-RPC endpoint for eth_call; empty defaults to Cloudflare's public endpoint (tests pass httptest URLs).
	// The public free tier is intentional: this is a cross-check, and per-tick failures just mean "reference unavailable this run".
	RPCURL string

	// Logger receives the decimals() verification lines (ERROR on a
	// config/chain mismatch, WARN on a failed read, INFO when an
	// unconfigured feed adopts the on-chain value). nil → slog.Default.
	Logger *slog.Logger

	// FeedMap maps canonical pair string ("<base>/<quote>", e.g. "fiat:EUR/fiat:USD") to feed metadata; empty seeds defaultChainlinkFeedMap, and operator entries merge OVER the defaults.
	FeedMap map[string]ChainlinkFeed
}

// ChainlinkFeed is the operator-facing feed-config shape, with no internal type leakage.
type ChainlinkFeed struct {
	// Address is the 0x-prefixed Ethereum contract address of the
	// Chainlink AggregatorV3 feed.
	Address string

	// Decimals is the power-of-10 divisor the operator asserts for the raw answer (8 for crypto/USD and most FX, 18 for ETH-denominated). 0 adopts the on-chain `decimals()`;
	// a disagreeing value REFUSES the feed (ErrPriceUnavailable + ErrChainlinkDecimalsMismatch), since a 10^(d-8) error would be a permanent false divergence.
	Decimals int

	// Invert is true when the canonical pair is the reciprocal of
	// the feed's natural quote (e.g. operator wants USD/EUR but
	// the feed publishes EUR/USD). When set, LookupQuote returns
	// 1 / raw_price instead of raw_price.
	Invert bool

	// MaxAge is the staleness ceiling for the feed's latestRoundData
	// updatedAt. Zero = the built-in feed's budget for a
	// built-in key, 76h for any other fiat/fiat key, else 3h.
	MaxAge time.Duration
}

// defaultChainlinkMaxAgeFX is the Chainlink FX staleness default,
// owned by the ingest source so both readers agree.
const defaultChainlinkMaxAgeFX = externalchainlink.DefaultMaxAgeFX

// NewChainlinkReference constructs a Chainlink-backed reference; an empty opts.FeedMap falls back to built-in Ethereum mainnet proxies, and operator entries merge OVER the defaults (override or add, never removed by omission).
func NewChainlinkReference(opts ChainlinkOptions) *ChainlinkReference {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	rpcURL := opts.RPCURL
	if rpcURL == "" {
		rpcURL = "https://cloudflare-eth.com"
	}
	rpcURL = strings.TrimRight(rpcURL, "/")
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	feedMap := defaultChainlinkFeedMap()
	for k, v := range opts.FeedMap {
		spec := chainlinkFeedSpec(v)
		// Decimals == 0 is left as-is: it means "adopt the on-chain
		// decimals()" (chainlink_decimals.go), not "divide by 10^0".
		if spec.MaxAge == 0 {
			spec.MaxAge = externalchainlink.DefaultMaxAge(k)
		}
		feedMap[k] = spec
	}
	// Pre-register zero-valued mismatch/verify-failed series per configured feed (bounded: FeedMap is fixed at construction), so "zero forever" is distinguishable from "never wired".
	for k := range feedMap {
		obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", k)
		obs.ChainlinkFeedDecimalsVerifyFailedTotal.WithLabelValues("divergence", k)
	}
	return &ChainlinkReference{
		httpClient: httpClient,
		rpcURL:     rpcURL,
		logger:     logger,
		now:        time.Now,
		feedMap:    feedMap,
		decimals:   make(map[string]*chainlinkDecimalsState),
	}
}

// defaultChainlinkFeedMap returns the built-in pair to AggregatorV3 contract seed, derived from [externalchainlink.DefaultFeedMap] so the
// two can't drift onto different proxies or staleness budgets for the same pair.
func defaultChainlinkFeedMap() map[string]chainlinkFeedSpec {
	src := externalchainlink.DefaultFeedMap()
	out := make(map[string]chainlinkFeedSpec, len(src))
	for k, v := range src {
		out[k] = chainlinkFeedSpec{
			Address:  v.Address,
			Decimals: int(v.Decimals),
			Invert:   v.Invert,
			MaxAge:   v.MaxAge,
		}
	}
	return out
}

// ChainlinkSourceName is the stable source label for the Chainlink reference (see SyntheticCrossName for the naming contract).
const ChainlinkSourceName = "chainlink"

func (*ChainlinkReference) Name() string { return ChainlinkSourceName }

// LookupQuote implements Reference with AsOf the round's updatedAt, via `latestRoundData()` (not `latestAnswer()`, which has no timestamp); a decimals() disagreement refuses the feed and a round older than MaxAge is rejected.
func (r *ChainlinkReference) LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error) {
	spec, ok := r.feedMap[pair.String()]
	if !ok {
		return Quote{}, fmt.Errorf("%w: chainlink: no feed configured for %s", ErrAssetUnsupported, pair.String())
	}

	decimals, err := r.resolveDecimals(ctx, pair, spec)
	if err != nil {
		return Quote{}, err
	}

	// `latestRoundData()` selector. AggregatorV3Interface.
	// keccak256("latestRoundData()")[:4] = feaf968c. Returns
	// (roundId uint80, answer int256, startedAt uint256,
	// updatedAt uint256, answeredInRound uint80) — 160 bytes.
	const latestRoundDataSelector = "0xfeaf968c"

	result, err := r.ethCall(ctx, spec.Address, latestRoundDataSelector)
	if err != nil {
		if errors.Is(err, errChainlinkEmptyResult) {
			return Quote{}, fmt.Errorf("chainlink: empty rpc result for %s", pair.String())
		}
		return Quote{}, err
	}

	answer, updatedAt, err := decodeChainlinkRoundData(result)
	if err != nil {
		return Quote{}, fmt.Errorf("chainlink: decode round data for %s: %w", pair.String(), err)
	}
	if answer.Sign() <= 0 {
		return Quote{}, fmt.Errorf("chainlink: non-positive answer for %s: %s", pair.String(), answer.String())
	}

	// Staleness gate. observedAt is the comparison timestamp
	// Compare passes through (also our injected clock for tests);
	// zero falls back to wall time defensively.
	asOf := observedAt
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	if age := asOf.Sub(updatedAt); age > spec.MaxAge {
		return Quote{}, fmt.Errorf("%w: chainlink: %s round is stale (updated %s ago, max %s)",
			ErrPriceUnavailable, pair.String(), age.Truncate(time.Second), spec.MaxAge)
	}

	priceFloat, err := scaleChainlinkAnswer(answer, decimals, spec.Invert)
	if err != nil {
		return Quote{}, fmt.Errorf("chainlink: scale answer for %s: %w", pair.String(), err)
	}
	return Quote{Price: priceFloat, AsOf: updatedAt}, nil
}

// errChainlinkEmptyResult — the RPC answered `0x` / empty. Wrong
// contract address or a reverted view; callers add the pair context.
var errChainlinkEmptyResult = errors.New("chainlink: empty rpc result")

// ethCall performs one eth_call round-trip at block "latest" and returns the raw 0x result; shared by the price read and the decimals() verification so both use the same endpoint, transport and body cap.
func (r *ChainlinkReference) ethCall(ctx context.Context, to, data string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": to, "data": data},
			"latest",
		},
	})
	if err != nil {
		return "", fmt.Errorf("chainlink: marshal request: %w", err)
	}

	// rpcURL is a SECRET: keyed providers (Alchemy, Infura, QuickNode) carry the API key in the path, and a *url.Error quotes the URL verbatim.
	// Errors land in Result.Failures, which the worker caches in a no-AUTH Redis, so route both error paths through externalchainlink's RedactedTransportError (secret-free text, still unwraps for errors.Is/As).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.rpcURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("chainlink: new request: %w", externalchainlink.NewRedactedTransportError(err, r.rpcURL))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := externalchainlink.DoWithoutRedirects(r.httpClient, req)
	if err != nil {
		return "", fmt.Errorf("chainlink: rpc transport: %w", externalchainlink.NewRedactedTransportError(err, r.rpcURL))
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the body at 64 KiB (as the Dashboard fetcher does): rpc_url may be a third-party RPC and this runs in internet-facing processes, so an unbounded io.ReadAll could OOM them.
	const maxBody = 64 << 10
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("chainlink: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chainlink: rpc status %d: %s", resp.StatusCode, string(respBody))
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return "", fmt.Errorf("chainlink: decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("chainlink: rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == "" || rpcResp.Result == "0x" {
		return "", fmt.Errorf("%w: to=%s data=%s", errChainlinkEmptyResult, to, data)
	}
	return rpcResp.Result, nil
}

// decodeChainlinkRoundData parses the 160-byte `latestRoundData()` result (five 32-byte words), returning the two's-complement int256 answer and updatedAt as UTC.
// It rejects fewer than 5 words so a one-word latestAnswer-shaped reply fails loudly instead of decoding garbage.
func decodeChainlinkRoundData(hexStr string) (*big.Int, time.Time, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(hexStr, "0x"))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("round data hex: %w", err)
	}
	if len(raw) < 160 {
		return nil, time.Time{}, fmt.Errorf("round data too short: %d bytes, want 160", len(raw))
	}
	answer := new(big.Int).SetBytes(raw[32:64])
	// Two's complement for negative int256 (sign bit set).
	if raw[32]&0x80 != 0 {
		answer.Sub(answer, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	updatedRaw := new(big.Int).SetBytes(raw[96:128])
	if !updatedRaw.IsInt64() {
		return nil, time.Time{}, fmt.Errorf("updatedAt overflows int64: %s", updatedRaw.String())
	}
	return answer, time.Unix(updatedRaw.Int64(), 0).UTC(), nil // i128:ok updatedAt unix seconds, IsInt64 range-checked above
}

// scaleChainlinkAnswer returns answer/10^decimals (or its reciprocal
// when invert) as a float64 for the percentage cross-check. The
// inversion happens in big.Rat so the float carries one rounding, not two.
func scaleChainlinkAnswer(answer *big.Int, decimals int, invert bool) (float64, error) {
	if decimals < 0 || decimals > 38 {
		return 0, fmt.Errorf("decimals %d out of range [0, 38]", decimals)
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	q := new(big.Rat).SetFrac(answer, div)
	if invert {
		if q.Sign() == 0 {
			return 0, fmt.Errorf("cannot invert zero answer")
		}
		q.Inv(q)
	}
	f, _ := q.Float64() // i128:ok reference price for a percentage cross-check; one correctly-rounded conversion of the exact ratio
	return f, nil
}
