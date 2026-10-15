package chainlink_stellar

import (
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// UpdateEvent is one decoded FeedUpdated: one feed's new answer.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

func (UpdateEvent) EventKind() string { return "chainlink_stellar.update" }

func (UpdateEvent) Source() string { return SourceName }

var _ consumer.Event = UpdateEvent{}
