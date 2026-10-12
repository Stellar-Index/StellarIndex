package band

import (
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// UpdateEvent is the [consumer.Event] Band's Decoder emits per (symbol, rate) pair; the sink type-switches on it and calls store.InsertOracleUpdate.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

// EventKind implements [consumer.Event].
func (UpdateEvent) EventKind() string { return "band.update" }

// Source implements [consumer.Event].
func (UpdateEvent) Source() string { return SourceName }

// Compile-time check.
var _ consumer.Event = UpdateEvent{}
