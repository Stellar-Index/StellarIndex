package reflector

import (
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// UpdateEvent is the [consumer.Event] Reflector's Decoder emits per oracle update; the sink type-switches on it and calls store.InsertOracleUpdate.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

// EventKind implements [consumer.Event].
func (UpdateEvent) EventKind() string { return "reflector.update" }

// Source implements [consumer.Event], returning the contained update's source name so the sink can attribute metrics per variant (reflector-dex / -cex / -fx).
func (u UpdateEvent) Source() string { return u.Update.Source }

// Compile-time check that UpdateEvent satisfies consumer.Event.
var _ consumer.Event = UpdateEvent{}
