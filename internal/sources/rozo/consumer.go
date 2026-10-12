package rozo

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is the [consumer.Event] the Rozo Decoder emits, one per decoded v1 contract event in the rozo_events row shape; per-type fields (From / Memo for payment, Token for flush) are *string so nil writes SQL NULL.
type Event struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    int
	// EventIndex is the event's position within its operation's contract-event list, the
	// rozo_events PK discriminator that keeps two same-type events from ONE operation from
	// collapsing to a single row.
	EventIndex  uint32
	ObservedAt  time.Time
	EventType   string // one of the Event* constants
	Amount      string // decimal i128
	Destination string
	From        *string // payment-only
	// Memo is payment-only ('' is a valid tag), already in lossless text-column-safe
	// scval.ToText form, so a sink binds it as-is and must not encode it again.
	Memo  *string
	Token *string // flush-only
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return "rozo.event" }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}

// eventFromPayment projects a decoded Payment into the canonical row
// Event. From + Memo are payment-only; Token stays nil.
func eventFromPayment(p Payment, observedAt time.Time) Event {
	from := p.From
	memo := p.Memo
	return Event{
		ContractID:  p.ContractID,
		Ledger:      p.Ledger,
		TxHash:      p.TxHash,
		OpIndex:     p.OpIndex,
		ObservedAt:  observedAt,
		EventType:   EventPayment,
		Amount:      p.Amount,
		Destination: p.Destination,
		From:        &from,
		Memo:        &memo,
	}
}

// eventFromFlush projects a decoded Flush. Token is flush-only;
// From + Memo stay nil.
func eventFromFlush(f Flush, observedAt time.Time) Event {
	token := f.Token
	return Event{
		ContractID:  f.ContractID,
		Ledger:      f.Ledger,
		TxHash:      f.TxHash,
		OpIndex:     f.OpIndex,
		ObservedAt:  observedAt,
		EventType:   EventFlush,
		Amount:      f.Amount,
		Destination: f.Destination,
		Token:       &token,
	}
}
