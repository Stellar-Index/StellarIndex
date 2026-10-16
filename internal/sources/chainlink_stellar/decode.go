package chainlink_stellar

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// eventFanoutStride spaces one operation's events in the synthetic OpIndex: one on_report
// emits a FeedUpdated per feed, so OperationIndex alone would collide them.
const eventFanoutStride = 1024

// opIndexFanoutMax keeps OperationIndex*eventFanoutStride+EventIndex within uint32.
const opIndexFanoutMax = (1 << 32) / eventFanoutStride

const dataIDLen = 32

func classify(e *events.Event) bool {
	return len(e.Topic) >= 1 && e.Topic[0] == TopicSymbolFeedUpdated
}

func isAdminTopic(e *events.Event) bool {
	return len(e.Topic) >= 1 && adminTopics[e.Topic[0]]
}

// decodeFeedUpdated turns one FeedUpdated{data_id; round_id, timestamp, answer: I256,
// ledger_seq, primary} into one OracleUpdate. The cache emits it only for a permitted,
// non-stale report (data-feeds-cache contract.rs on_report).
func decodeFeedUpdated(e *events.Event, closedAt time.Time) (canonical.OracleUpdate, error) {
	if !classify(e) {
		return canonical.OracleUpdate{}, ErrNotFeedUpdated
	}
	if e.EventIndex < 0 || e.EventIndex >= eventFanoutStride {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: got %d", ErrEventIndexOverflow, e.EventIndex)
	}
	if e.OperationIndex < 0 || e.OperationIndex >= opIndexFanoutMax {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: got %d", ErrOperationIndexOverflow, e.OperationIndex)
	}
	if len(e.Topic) != 2 {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: want 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	dataID, err := decodeDataID(e.Topic[1])
	if err != nil {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: data_id: %w", ErrMalformedPayload, err)
	}
	answer, tsSec, err := decodeBody(e.Value)
	if err != nil {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	if answer.Sign() <= 0 {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: %s", ErrNonPositiveAnswer, answer)
	}
	feed, err := resolveFeed(dataID)
	if err != nil {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: data_id: %w", ErrMalformedPayload, err)
	}
	return canonical.OracleUpdate{
		Source:     SourceName,
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex)*eventFanoutStride + uint32(e.EventIndex),
		Timestamp:  canonical.SafeUnixSeconds(tsSec, closedAt),
		Asset:      feed.Base,
		Quote:      feed.Quote,
		Price:      answer,
		Decimals:   DefaultDecimals,
	}, nil
}

func decodeDataID(topic string) ([]byte, error) {
	sv, err := scval.Parse(topic)
	if err != nil {
		return nil, err
	}
	b, err := scval.AsBytes(sv)
	if err != nil {
		return nil, err
	}
	if len(b) != dataIDLen {
		return nil, fmt.Errorf("want %d bytes, got %d", dataIDLen, len(b))
	}
	return b, nil
}

// decodeBody reads the fields by name, so a later contract version that adds fields still decodes.
func decodeBody(value string) (canonical.Amount, uint64, error) {
	sv, err := scval.Parse(value)
	if err != nil {
		return canonical.Amount{}, 0, err
	}
	m, err := scval.AsMap(sv)
	if err != nil {
		return canonical.Amount{}, 0, err
	}
	av, err := scval.MustMapField(m, "answer")
	if err != nil {
		return canonical.Amount{}, 0, err
	}
	answer, err := scval.AsAmountFromI256(av)
	if err != nil {
		return canonical.Amount{}, 0, fmt.Errorf("answer: %w", err)
	}
	tv, err := scval.MustMapField(m, "timestamp")
	if err != nil {
		return canonical.Amount{}, 0, err
	}
	ts, err := scval.AsU64(tv)
	if err != nil {
		return canonical.Amount{}, 0, fmt.Errorf("timestamp: %w", err)
	}
	return answer, ts, nil
}
