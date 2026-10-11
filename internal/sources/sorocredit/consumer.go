package sorocredit

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// decodeOne classifies and decodes one event into the canonical [Event] row, joining decode.go's
// per-kind helpers with the identity fields on events.Event. An untracked topic returns
// ErrNotSoroCreditEvent so the dispatcher skips cheaply; a malformed tracked event returns an
// ErrMalformedPayload the dispatcher counts and skips.
func decodeOne(ev *events.Event) (Event, error) {
	kind := classify(ev)
	if kind == "" {
		return Event{}, fmt.Errorf("%w: topic0=%q", ErrNotSoroCreditEvent, topic0(ev))
	}

	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return Event{}, fmt.Errorf("sorocredit: %s: %w", kind, err)
	}

	var (
		d    decoded
		derr error
	)
	switch kind {
	case TypeNewCollateralContract:
		d, derr = decodeNewCollateralContract(ev)
	case TypeStatement:
		d, derr = decodeStatement(ev, observedAt)
	case TypeSettlement:
		d, derr = decodeSettlement(ev)
	case TypeWithdrawal:
		d, derr = decodeWithdrawal(ev)
	case TypeSupportedAssetAdded:
		d, derr = decodeSupportedAssetAdded(ev)
	case TypeBeaconUpdated, TypeCollateralHashUpdated, TypeTreasuryUpdated:
		d, derr = decodeConfigBody(ev)
	default:
		// Unreachable while classify and this switch stay in lockstep.
		return Event{}, fmt.Errorf("%w: %s", ErrNotSoroCreditEvent, kind)
	}
	if derr != nil {
		return Event{}, derr
	}

	attrs := d.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	return Event{
		ContractID:         ev.ContractID,
		Ledger:             ev.Ledger,
		TxHash:             ev.TxHash,
		OpIndex:            ev.OperationIndex,
		EventIndex:         ev.EventIndex,
		ObservedAt:         observedAt,
		EventType:          kind,
		CollateralContract: d.CollateralContract,
		PositionUUID:       d.PositionUUID,
		StatementUUID:      d.StatementUUID,
		PositionName:       d.PositionName,
		Owner:              d.Owner,
		Account:            d.Account,
		Asset:              d.Asset,
		Amount:             d.Amount,
		StatementTime:      d.StatementTime,
		Attributes:         attrs,
	}, nil
}

// topic0 returns the base64 topic[0] for error context, or "" when the
// event has no topics.
func topic0(ev *events.Event) string {
	if len(ev.Topic) == 0 {
		return ""
	}
	return ev.Topic[0]
}

// project wraps decodeOne and adapts to the dispatcher's
// []consumer.Event return shape — exactly one Event per recognised
// contract event.
func project(ev *events.Event) ([]consumer.Event, error) {
	out, err := decodeOne(ev)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{out}, nil
}
