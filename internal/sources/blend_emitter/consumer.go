package blend_emitter

import (
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/consumer"
)

// Recipient is one (address, amount) pair from a DropEvent's
// variable-length recipient list.
type Recipient struct {
	Address string
	Amount  canonical.Amount
}

// DistributeEvent is the [consumer.Event] emitted on a successful
// `distribute` decode — one BLND emission distributed to a backstop.
// The indexer's event sink type-switches on this and writes to
// `blend_emitter_events` (event_kind='distribute').
type DistributeEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time

	BackstopID string
	Amount     canonical.Amount
}

// EventKind implements [consumer.Event].
func (DistributeEvent) EventKind() string { return "blend_emitter.distribute" }

// Source implements [consumer.Event].
func (DistributeEvent) Source() string { return SourceName }

// DropEvent is the [consumer.Event] emitted on a successful `drop`
// decode — a one-shot BLND airdrop to a variable-length recipient
// list. Recipients carries every (address, amount) pair from the
// ONE underlying Soroban event; the storage writer fans this out one
// row per recipient (a `recipient_index` discriminator distinguishes
// them in the PK), same shape as
// [internal/sources/aquarius.ReservesEvent]'s per-token fan-out.
type DropEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time

	Recipients []Recipient
}

// EventKind implements [consumer.Event].
func (DropEvent) EventKind() string { return "blend_emitter.drop" }

// Source implements [consumer.Event].
func (DropEvent) Source() string { return SourceName }

// SwapConfigKind discriminates the two backstop-swap lifecycle
// events. Both share an identical body shape (new_backstop /
// new_backstop_token / unlock_time); only the topic (and therefore
// the lifecycle stage) differs.
type SwapConfigKind string

const (
	// SwapConfigQueued — a `q_swap` event: a backstop swap was
	// QUEUED, subject to the timelock in UnlockTime.
	SwapConfigQueued SwapConfigKind = "q_swap"
	// SwapConfigExecuted — a `swap` event: a previously queued
	// backstop swap was EXECUTED once its timelock elapsed.
	SwapConfigExecuted SwapConfigKind = "swap"
)

// IsValid reports whether k is one of the two known kinds.
func (k SwapConfigKind) IsValid() bool {
	switch k {
	case SwapConfigQueued, SwapConfigExecuted:
		return true
	}
	return false
}

// SwapConfigEvent is the [consumer.Event] emitted on a successful
// `q_swap` / `swap` decode — the Emitter queuing or executing a
// change of which backstop (and backstop token) it targets.
type SwapConfigEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time

	Kind             SwapConfigKind
	NewBackstop      string
	NewBackstopToken string
	UnlockTime       uint64 // Unix seconds
}

// EventKind implements [consumer.Event].
func (SwapConfigEvent) EventKind() string { return "blend_emitter.swap_config" }

// Source implements [consumer.Event].
func (SwapConfigEvent) Source() string { return SourceName }

// Compile-time checks.
var (
	_ consumer.Event = DistributeEvent{}
	_ consumer.Event = DropEvent{}
	_ consumer.Event = SwapConfigEvent{}
)
