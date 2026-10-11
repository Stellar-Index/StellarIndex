// Package chainlink ingests price observations from Chainlink Data Feeds on Ethereum mainnet via
// JSON-RPC reads against the AggregatorV3 contract interface.
//
// Unlike internal/divergence/chainlink.go (a synchronous cross-check helper), this is a real ingest
// source: its poller dedupes by roundId and writes canonical.OracleUpdate rows to oracle_updates.
// See README.md for design rationale.
package chainlink

import (
	"encoding/hex"
	"errors"
	"math/big"
	"sync"
	"time"

	"golang.org/x/crypto/sha3"
)

// init computes [AnswerUpdatedTopic0] once, so a typo in AnswerUpdatedSignature is caught at boot
// instead of silently mis-filtering every log.
func init() {
	h := sha3.NewLegacyKeccak256()
	_, _ = h.Write([]byte(AnswerUpdatedSignature))
	AnswerUpdatedTopic0 = "0x" + hex.EncodeToString(h.Sum(nil))
}

// SourceName is the canonical source identifier stamped on every emitted
// canonical.OracleUpdate.Source. Stable: it appears in metrics labels, /v1/sources and
// oracle_updates rows. A future Soroban Chainlink source gets its own name.
const SourceName = "chainlink"

// DefaultPollInterval is the freshness ceiling for current-price endpoints. Most feeds update on a
// heartbeat of minutes or a 0.5% deviation, so polling faster wastes RPC budget.
const DefaultPollInterval = 30 * time.Second

// DefaultDecimals is the standard for crypto/USD and fiat/USD feeds and the value every built-in
// DefaultFeedMap entry asserts. It is NOT substituted for an omitted value: FeedSpec.Decimals == 0
// means "adopt the feed's on-chain decimals()", and an asserted value is verified against the chain
// before any row is projected.
const DefaultDecimals uint8 = 8

// DefaultEndpoint is the Cloudflare public Ethereum JSON-RPC endpoint (free, per-IP rate-limited).
// Production ingest of hundreds of feeds will be throttled; operators should override it.
const DefaultEndpoint = "https://cloudflare-eth.com"

// AggregatorV3Interface function selectors, keccak256(signature)[:4], precomputed.
const (
	// SelLatestRoundData = keccak256("latestRoundData()")[:4]
	SelLatestRoundData = "0xfeaf968c"

	// SelLatestAnswer = keccak256("latestAnswer()")[:4]; the older AggregatorV2 selector.
	SelLatestAnswer = "0x50d25bcd"

	// SelDecimals = keccak256("decimals()")[:4]
	SelDecimals = "0x313ce567"
)

// AnswerUpdatedSignature is the Solidity event signature hashed into topic[0] at init, not
// hardcoded as hex, so a typo cannot silently miss every log.
//
//	event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt)
//
// `current` and `roundId` are topics[1] and topics[2]; `updatedAt` is in the data field.
const AnswerUpdatedSignature = "AnswerUpdated(int256,uint256,uint256)"

// AnswerUpdatedTopic0 is the keccak256 hash of [AnswerUpdatedSignature],
// 0x-prefixed. Computed at package init from the signature constant.
var AnswerUpdatedTopic0 string

// FeedSpec describes one Chainlink AggregatorV3 contract and how to scale its raw int256. The pair
// is implicit in the FeedMap key; FeedSpec only carries the per-address fields.
type FeedSpec struct {
	// Address is the 0x-prefixed AggregatorV3 proxy address; it is stable across the underlying
	// aggregator's upgrades.
	Address string

	// Decimals is the power-of-10 divisor applied to the raw int256 answer. Zero adopts the
	// feed's on-chain decimals() (see [DefaultDecimals]).
	Decimals uint8

	// Invert flips price to 1/price after scaling, for a canonical pair that is the reciprocal of
	// the feed's quote (USD/EUR from an EUR/USD feed). Same semantic as divergence/chainlink.go.
	Invert bool

	// MaxAge is the live-poll staleness budget: a round whose updatedAt is older is refused, never
	// emitted as a new observation. Calibrated to the feed heartbeat (see [DefaultFeedMap]); zero
	// resolves via [DefaultMaxAge]. Historical AnswerUpdated backfill is exempt.
	MaxAge time.Duration
}

// Round is one decoded latestRoundData() / getRoundData() tuple: the dedup key (RoundID) and the
// source of the OracleUpdate's timestamp and price.
type Round struct {
	// FeedAddress is the 0x-prefixed contract address this round came from.
	FeedAddress string

	// RoundID is the FULL uint80 proxy round id, (phaseId<<64)|aggregatorRoundId, held as a
	// *big.Int so it never truncates. Dedup is by (FeedAddress, RoundID) so repeated polls of an
	// unchanged feed emit no duplicate rows. Never nil after decode; treat nil as zero.
	RoundID *big.Int

	// AnsweredInRound is the round that computed Answer, in RoundID's phase-encoded uint80 space;
	// less than RoundID means the answer was carried forward. Nil for AnswerUpdated logs, which
	// only fire on a fresh answer.
	AnsweredInRound *big.Int

	// Answer is the raw int256 price at the feed's native decimals, unscaled: OracleUpdate.Price
	// stores raw integers (ADR-0003).
	Answer string // decimal-string form for cross-package portability

	// UpdatedAt is Chainlink's `updatedAt`, the authoritative publication time. Always UTC.
	UpdatedAt time.Time
}

// Errors used by the package. Callers classify via errors.Is.
var (
	// ErrEmptyResult — the RPC returned `0x` or empty: a wrong address or a server-side revert.
	// The poller logs, skips and retries next tick.
	ErrEmptyResult = errors.New("chainlink: empty rpc result")

	// ErrMalformedResult — the RPC data does not decode as the expected ABI shape.
	ErrMalformedResult = errors.New("chainlink: malformed rpc result")

	// ErrUnknownFeed — a polled pair is not in the FeedMap.
	ErrUnknownFeed = errors.New("chainlink: pair not in configured FeedMap")

	// ErrNonPositivePrice — some legacy feeds returned negative answers under stress; dropped
	// rather than written, since oracle_updates has CHECK (price > 0).
	ErrNonPositivePrice = errors.New("chainlink: non-positive answer")

	// ErrFutureUpdatedAt — updatedAt is ahead of the poller's clock beyond a small skew allowance.
	// Refused so a future-dated row can never pin itself as the pair's latest.
	ErrFutureUpdatedAt = errors.New("chainlink: updatedAt ahead of poller clock")

	// ErrStaleRound — the latest round is older than the feed's MaxAge. Refused so a frozen or
	// retired feed is never re-emitted as a current observation.
	ErrStaleRound = errors.New("chainlink: round older than the feed's max age")

	// ErrCarriedForwardRound — answeredInRound < roundId: the answer was computed earlier, so this
	// is not a new publication.
	ErrCarriedForwardRound = errors.New("chainlink: answer carried forward from an earlier round")
)

// roundCache is the per-feed dedup memory: last roundId emitted per lowercased feed address.
// Correctness across restarts rests on the oracle_updates PK: a re-emitted round hits ON CONFLICT,
// so the cache only saves the round-trip.
type roundCache struct {
	mu   sync.Mutex
	last map[string]*big.Int // feed address (lowercase) → last roundId emitted
}

func newRoundCache() *roundCache {
	return &roundCache{last: make(map[string]*big.Int)}
}

// shouldEmit reports whether this (feed, roundId) is strictly greater than the last seen, and if
// so records it. The production poll path splits wouldEmit and commitEmit so a failure between
// check and round build cannot strand the round.
//
// roundID is the FULL uint80 proxy round id. Comparing the wide value keeps emission alive across a
// proxy phase upgrade (aggregatorRoundId resets to ~1 but phaseId increments); a low-64-bit key
// would wedge the feed until restart. A nil roundID is zero.
func (c *roundCache) shouldEmit(feedAddr string, roundID *big.Int) bool {
	if !c.wouldEmit(feedAddr, roundID) {
		return false
	}
	c.commitEmit(feedAddr, roundID)
	return true
}
