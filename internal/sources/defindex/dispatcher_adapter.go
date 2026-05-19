package defindex

import (
	"github.com/RatesEngine/rates-engine/internal/consumer"
	"github.com/RatesEngine/rates-engine/internal/events"
)

// Decoder implements dispatcher.Decoder (the event-based variant —
// not ContractCallDecoder). Blend strategy contracts publish
// Soroban contract events on every capital flow
// (`("BlendStrategy","deposit"|"withdraw"|…)`), so the standard
// event path is the right hook.
//
// Dispatch is by TOPIC, not a hand-curated contract set: any
// contract emitting the BlendStrategy deposit/withdraw topic is
// matched. This mirrors the comet / aquarius shared-emitter
// topology and is what the granular-coverage mission wants — every
// Blend autocompound strategy instance, not just paltalabs' three.
// (The previous revision filtered on a 3-contract "vault" set that
// was mislabeled tag-1.0.0 fiction; see defindex.md.)
//
// Stateless. Matching is O(1) — two byte-equal topic compares
// before any SCVal parsing.
type Decoder struct{}

// NewDecoder constructs a topic-matched Blend strategy event
// decoder. No arguments — matching is purely on the
// ("BlendStrategy", deposit|withdraw) topic shape.
func NewDecoder() *Decoder { return &Decoder{} }

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Cheap predicate: the
// topic shape is a BlendStrategy deposit/withdraw. The dispatcher
// only calls Decode() when this returns true.
func (d *Decoder) Matches(ev events.Event) bool {
	return classify(&ev) != ""
}

// Decode implements [dispatcher.Decoder]. Emits one Event per
// matched strategy flow. Returning an error is a "skip + count"
// signal per the dispatcher's contract — a malformed event doesn't
// abort the ledger, just gets dropped + counted.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		// Defensive — Matches should have filtered.
		return nil, ErrUnknownEvent
	}
	flow, err := decodeFlow(&ev, kind)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{Event{Flow: flow}}, nil
}
