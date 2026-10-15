package chainlink_stellar

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the oracle_updates source label. Kept apart from "chainlink", the EVM poller's
// historical rows, so the two feeds' histories never merge.
const SourceName = "chainlink-stellar"

// DefaultDecimals is the answer scale every mainnet feed declares in its FeedConfigSet.
const DefaultDecimals uint8 = 18

// DefaultResolutionSeconds is the slowest heartbeat among the mainnet feeds (24h).
const DefaultResolutionSeconds = 24 * 60 * 60

// MainnetCacheContract is the DataFeedsCache every mainnet feed writes through; the proxy
// (CDQBHEUG…) only reads it and emits nothing.
const MainnetCacheContract = "CAAFKBE6AMTERARISMMT4TCC4UGNO35XCKQNC5XVLL5WY5GRWXH6AMIO"

// GenesisLedger is the cache's first event on mainnet (FeedConfigSet); the first FeedUpdated is 64545407.
const GenesisLedger uint32 = 64_442_968

// EventTopic0 is the FeedUpdated contractevent's topic: ["FeedUpdated", data_id: BytesN<32>].
const EventTopic0 = "FeedUpdated"

// TopicSymbolFeedUpdated is the base64 ScVal of [EventTopic0].
var TopicSymbolFeedUpdated = scval.MustEncodeSymbol(EventTopic0)

var (
	ErrNotFeedUpdated = errors.New("chainlink_stellar: not a FeedUpdated event")

	ErrMalformedPayload = errors.New("chainlink_stellar: malformed FeedUpdated payload")

	ErrNonPositiveAnswer = errors.New("chainlink_stellar: non-positive answer")

	ErrEventIndexOverflow = errors.New("chainlink_stellar: EventIndex exceeds OpIndex fanout stride")

	ErrOperationIndexOverflow = errors.New("chainlink_stellar: OperationIndex exceeds OpIndex fanout bound")
)
