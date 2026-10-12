package rozo

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the stateless dispatcher-facing Rozo v1 Payment view, decoding each payment / flush independently into one rozo_events row; it matches by topic[0] symbol AND contract id (ADR-0035) because `symbol_short!` has no protocol namespace.
type Decoder struct{}

// NewDecoder constructs a Rozo Decoder; it is stateless and safe to share.
func NewDecoder() *Decoder { return &Decoder{} }

// withIndex stamps the intra-op event index onto the projected Event. EventIndex is the rozo_events PK discriminator that keeps two
// events of the SAME event_type from ONE operation from collapsing to a row; it comes from the dispatcher live and the lake on re-derive.
func withIndex(idx int, e Event) []consumer.Event {
	e.EventIndex = uint32(idx) //nolint:gosec // EventIndex is non-negative by Soroban spec.
	return []consumer.Event{e}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// rozoContracts is the set of v1 Payment contract C-strkeys this decoder claims; all share one
// WASM hash and thus one event schema (see [MainnetPaymentContracts]).
var rozoContracts = func() map[string]struct{} {
	m := make(map[string]struct{}, len(MainnetPaymentContracts))
	for _, c := range MainnetPaymentContracts {
		m[c] = struct{}{}
	}
	return m
}()

// IsRozoContract reports whether id is one of the known Rozo v1 Payment contracts on Stellar mainnet.
func IsRozoContract(id string) bool {
	_, ok := rozoContracts[id]
	return ok
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder], claiming an event only when topic[0] is a Rozo v1 symbol AND a known Rozo contract emitted it.
func (*Decoder) Matches(ev events.Event) bool {
	return IsRozoContract(ev.ContractID) && Classify(&ev) != ""
}

// Decode implements [dispatcher.Decoder], emitting exactly one [Event] per recognised Rozo event; a decode error is non-fatal per the dispatcher contract (counted and skipped).
func (*Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := Classify(&ev)
	if kind == "" || !IsRozoContract(ev.ContractID) {
		return nil, nil
	}

	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, fmt.Errorf("rozo: %s: %w", kind, err)
	}

	switch kind {
	case EventPayment:
		p, err := DecodePayment(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromPayment(p, observedAt)), nil
	case EventFlush:
		f, err := DecodeFlush(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromFlush(f, observedAt)), nil
	}
	// Unreachable while Classify and this switch stay in lockstep. Returning the sentinel makes a
	// future Classify case without a switch arm a counted decode error, not a silent drop.
	return nil, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
}
