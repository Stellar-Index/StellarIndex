package blnt

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify resolves topic[0] only within the ABI selected by contract role.
// Contract identity is established by Decoder.roleOf before this is called.
func classify(role Role, ev *events.Event) string {
	if len(ev.Topic) == 0 {
		return ""
	}
	return encodedRoleEvents[role][ev.Topic[0]]
}

func decodeEvent(role Role, kind string, ev *events.Event) (Event, error) {
	if role == "" || kind == "" {
		return Event{}, ErrNotBLNTEvent
	}
	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return Event{}, fmt.Errorf("%w: %s/%s close time: %w", ErrMalformedEvent, role, kind, err)
	}
	// Parse every topic and the body once. The projection intentionally stores
	// their exact base64 wire form, but corrupt XDR must fail loud rather than
	// become an apparently valid semantic event row.
	for i, topic := range ev.Topic {
		if _, err := scval.Parse(topic); err != nil {
			return Event{}, fmt.Errorf("%w: %s/%s topic[%d]: %w", ErrMalformedEvent, role, kind, i, err)
		}
	}
	if ev.Value == "" {
		return Event{}, fmt.Errorf("%w: %s/%s has empty value", ErrMalformedEvent, role, kind)
	}
	if _, err := scval.Parse(ev.Value); err != nil {
		return Event{}, fmt.Errorf("%w: %s/%s body: %w", ErrMalformedEvent, role, kind, err)
	}

	return Event{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex), //nolint:gosec // non-negative by Soroban event contract.
		EventIndex: uint32(ev.EventIndex),     //nolint:gosec // non-negative by Soroban event contract.
		ObservedAt: observedAt,
		Role:       role,
		EventType:  kind,
		TopicsXDR:  append([]string(nil), ev.Topic...),
		ValueXDR:   ev.Value,
	}, nil
}

func decodeDeployTarget(ev *events.Event) (string, error) {
	if len(ev.Topic) != 1 {
		return "", fmt.Errorf("%w: factory/deploy expected 1 topic, got %d", ErrMalformedEvent, len(ev.Topic))
	}
	body, err := scval.Parse(ev.Value)
	if err != nil {
		return "", fmt.Errorf("%w: factory/deploy body: %w", ErrMalformedEvent, err)
	}
	target, err := scval.AsAddressStrkey(body)
	if err != nil {
		return "", fmt.Errorf("%w: factory/deploy target: %w", ErrMalformedEvent, err)
	}
	return target, nil
}
