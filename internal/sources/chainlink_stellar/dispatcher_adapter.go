package chainlink_stellar

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder decodes FeedUpdated events from one DataFeedsCache contract (ADR-0035: gated on
// contract identity, since any contract can emit a "FeedUpdated" topic).
type Decoder struct {
	contractID string
}

func NewDecoder(cacheContractID string) *Decoder {
	return &Decoder{contractID: cacheContractID}
}

func (d *Decoder) Name() string { return SourceName }

func (d *Decoder) Matches(ev events.Event) bool {
	return ev.ContractID == d.contractID && classify(&ev)
}

func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	u, err := decodeFeedUpdated(&ev, closedAt)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{UpdateEvent{Update: u}}, nil
}
