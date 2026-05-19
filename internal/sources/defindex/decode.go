package defindex

import (
	"fmt"

	"github.com/RatesEngine/rates-engine/internal/events"
	"github.com/RatesEngine/rates-engine/internal/scval"
)

// classify decides whether this is a Blend strategy flow event we
// decode. Topics are 2-tuples:
//
//	topic[0] = String("BlendStrategy")     — pre-encoded, byte-equal
//	topic[1] = Symbol("deposit"|"withdraw")
//
// Both positions are compared as byte-equal base64 against the
// constants computed at package init — no SCVal parsing on the
// reject path. Returns "" for anything else (the dispatcher's
// drop-counter handles "" cases: harvest / keeper admin / …).
func classify(e *events.Event) string {
	if len(e.Topic) < 2 {
		return ""
	}
	if e.Topic[0] != TopicPrefixStrategy {
		return ""
	}
	switch e.Topic[1] {
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolWithdraw:
		return EventWithdraw
	}
	return ""
}

// decodeFlow converts one classified strategy event into a
// StrategyFlow.
//
// Body shape (verified on-chain via scan-soroban-events — identical
// for both deposit and withdraw):
//
//	{ from: Address, amount: i128 }
//
// Fields are pulled by name from the top-level Map per
// docs/architecture/contract-schema-evolution.md's decode-by-name
// rule — positional decoding would silently break across upgrades.
func decodeFlow(e *events.Event, kind string) (StrategyFlow, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}

	flow := StrategyFlow{
		Source:     SourceName,
		Ledger:     e.Ledger,
		ClosedAt:   closedAt,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ContractID: e.ContractID,
	}

	switch kind {
	case EventDeposit:
		flow.Direction = DirectionDeposit
	case EventWithdraw:
		flow.Direction = DirectionWithdraw
	default:
		// Defensive — classify() should have filtered.
		return StrategyFlow{}, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: body not a Map: %w", ErrMalformedPayload, err)
	}

	fromSv, err := scval.MustMapField(entries, "from")
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.from: %w", ErrMalformedPayload, kind, err)
	}
	flow.From, err = scval.AsAddressStrkey(fromSv)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.from: %w", ErrMalformedPayload, kind, err)
	}

	amountSv, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.amount: %w", ErrMalformedPayload, kind, err)
	}
	flow.Amount, err = scval.AsAmountFromI128(amountSv)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.amount: %w", ErrMalformedPayload, kind, err)
	}

	return flow, nil
}
