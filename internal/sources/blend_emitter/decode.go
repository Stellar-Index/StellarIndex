package blend_emitter

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify reports the Emitter event kind of a single-topic Symbol event, or "" if unrecognised.
// It only byte-compares pre-encoded TopicSymbol* blobs, keeping the dispatcher hot path cheap.
func classify(e *events.Event) string {
	if len(e.Topic) < emitterTopicArity {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolDistribute:
		return EventDistribute
	case TopicSymbolDrop:
		return EventDrop
	case TopicSymbolQSwap:
		return EventQSwap
	case TopicSymbolSwap:
		return EventSwap
	}
	return ""
}

// distributeFields holds the two addressable fields from a
// `distribute` event body.
type distributeFields struct {
	BackstopID string // strkey — the backstop pool this emission went to
	Amount     canonical.Amount
}

// decodeDistribute decodes a `distribute` body (verified against a mainnet fixture): Vec[ Address backstop_id, i128 amount ], a
// fixed 2-element bare Vec, so decoded positionally.
func decodeDistribute(e *events.Event) (distributeFields, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return distributeFields{}, fmt.Errorf("%w: distribute body parse: %w", ErrMalformedPayload, err)
	}
	vec, err := scval.AsVec(body)
	if err != nil {
		return distributeFields{}, fmt.Errorf("%w: distribute body not a Vec: %w", ErrMalformedPayload, err)
	}
	if len(vec) != 2 {
		return distributeFields{}, fmt.Errorf("%w: distribute body length %d != 2", ErrMalformedPayload, len(vec))
	}

	backstopID, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return distributeFields{}, fmt.Errorf("%w: distribute backstop_id: %w", ErrMalformedPayload, err)
	}
	amount, err := scval.AsAmountFromI128(vec[1])
	if err != nil {
		return distributeFields{}, fmt.Errorf("%w: distribute amount: %w", ErrMalformedPayload, err)
	}
	if amount.Sign() <= 0 {
		return distributeFields{}, fmt.Errorf("%w: distribute amount=%s", ErrNonPositiveAmount, amount)
	}

	return distributeFields{BackstopID: backstopID, Amount: amount}, nil
}

// dropRecipient is one (recipient, amount) pair from a `drop`
// event's variable-length outer Vec.
type dropRecipient struct {
	Recipient string // strkey
	Amount    canonical.Amount
}

// decodeDrop decodes a `drop` body: Vec[ Vec[ Address recipient, i128 amount ], ... ]. The outer Vec length is NOT fixed (fixtures
// with 13 and 3 recipients); each inner element is a fixed bare Vec, decoded positionally. Storage fans the recipient slice out
// with a recipient_index, since a coarse PK would collapse a multi-row emission.
func decodeDrop(e *events.Event) ([]dropRecipient, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: drop body parse: %w", ErrMalformedPayload, err)
	}
	outer, err := scval.AsVec(body)
	if err != nil {
		return nil, fmt.Errorf("%w: drop body not a Vec: %w", ErrMalformedPayload, err)
	}
	if len(outer) == 0 {
		return nil, ErrEmptyRecipients
	}

	out := make([]dropRecipient, 0, len(outer))
	for i, item := range outer {
		pair, err := scval.AsVec(item)
		if err != nil {
			return nil, fmt.Errorf("%w: drop recipient[%d] not a Vec: %w", ErrMalformedPayload, i, err)
		}
		if len(pair) != 2 {
			return nil, fmt.Errorf("%w: drop recipient[%d] length %d != 2", ErrMalformedPayload, i, len(pair))
		}
		recipient, err := scval.AsAddressStrkey(pair[0])
		if err != nil {
			return nil, fmt.Errorf("%w: drop recipient[%d] address: %w", ErrMalformedPayload, i, err)
		}
		amount, err := scval.AsAmountFromI128(pair[1])
		if err != nil {
			return nil, fmt.Errorf("%w: drop recipient[%d] amount: %w", ErrMalformedPayload, i, err)
		}
		if amount.Sign() <= 0 {
			return nil, fmt.Errorf("%w: drop recipient[%d] amount=%s", ErrNonPositiveAmount, i, amount)
		}
		out = append(out, dropRecipient{Recipient: recipient, Amount: amount})
	}
	return out, nil
}

// swapConfigFields holds the three fields shared by `q_swap` (queue) and `swap` (execute) bodies,
// which have a byte-identical shape: real fixtures show swap executed exactly what q_swap queued.
type swapConfigFields struct {
	NewBackstop      string // strkey — the backstop the Emitter will point at / now points at
	NewBackstopToken string // strkey — that backstop's LP/BLND token
	UnlockTime       uint64 // Unix seconds; the timelock q_swap queues against
}

// decodeSwapConfig decodes a `q_swap` or `swap` event body, Map{ new_backstop, new_backstop_token: Address, unlock_time: u64 }, by
// Map field name (docs/architecture/ingest-pipeline.md#contract-schema-evolution), resilient to WASM upgrades adding fields.
func decodeSwapConfig(e *events.Event) (swapConfigFields, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: body parse: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: body not a Map: %w", ErrMalformedPayload, err)
	}

	newBackstopSv, err := scval.MustMapField(entries, "new_backstop")
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: missing new_backstop: %w", ErrMalformedPayload, err)
	}
	newBackstop, err := scval.AsAddressStrkey(newBackstopSv)
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: new_backstop: %w", ErrMalformedPayload, err)
	}

	newBackstopTokenSv, err := scval.MustMapField(entries, "new_backstop_token")
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: missing new_backstop_token: %w", ErrMalformedPayload, err)
	}
	newBackstopToken, err := scval.AsAddressStrkey(newBackstopTokenSv)
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: new_backstop_token: %w", ErrMalformedPayload, err)
	}

	unlockTimeSv, err := scval.MustMapField(entries, "unlock_time")
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: missing unlock_time: %w", ErrMalformedPayload, err)
	}
	unlockTime, err := scval.AsU64(unlockTimeSv)
	if err != nil {
		return swapConfigFields{}, fmt.Errorf("%w: unlock_time: %w", ErrMalformedPayload, err)
	}

	return swapConfigFields{
		NewBackstop:      newBackstop,
		NewBackstopToken: newBackstopToken,
		UnlockTime:       unlockTime,
	}, nil
}
