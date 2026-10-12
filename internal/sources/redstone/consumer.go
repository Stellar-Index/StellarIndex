package redstone

import (
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// UpdateEvent is the [consumer.Event] Redstone's Decoder emits per feed update; the sink type-switches on it and calls store.InsertOracleUpdate, same shape as reflector.UpdateEvent.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

// EventKind implements [consumer.Event].
func (UpdateEvent) EventKind() string { return "redstone.update" }

// Source implements [consumer.Event].
func (UpdateEvent) Source() string { return SourceName }

// Compile-time check that UpdateEvent satisfies consumer.Event.
var _ consumer.Event = UpdateEvent{}
