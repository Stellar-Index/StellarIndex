package spectra

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify maps topic[0] to a kind, or "" if unrecognised. Routing only;
// the contract-identity gate is [Decoder.Matches] (ADR-0035).
func classify(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolWrap:
		return EventWrap
	case TopicSymbolUnwrap:
		return EventUnwrap
	case TopicSymbolApprove:
		return EventApprove
	}
	return ""
}

func newEvent(e *events.Event, kind string) (Event, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	return Event{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		ObservedAt: closedAt,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // non-negative by Soroban spec.
		Kind:       kind,
	}, nil
}

func decodeAddrTopic(e *events.Event, i int, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[i])
	if err != nil {
		return "", fmt.Errorf("%w: parse topic[%d] (%s): %w", ErrMalformedPayload, i, field, err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return "", fmt.Errorf("%w: topic[%d] (%s): %w", ErrMalformedPayload, i, field, err)
	}
	return addr, nil
}

// bodyI128Fields reads the named i128 fields of a Map body by NAME, never
// by position.
func bodyI128Fields(e *events.Event, kind string, fields ...string) ([]canonical.Amount, error) {
	sv, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s body: %w", ErrMalformedPayload, kind, err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, fmt.Errorf("%w: %s body is not a Map: %w", ErrMalformedPayload, kind, err)
	}
	out := make([]canonical.Amount, 0, len(fields))
	for _, field := range fields {
		fv, ferr := scval.MustMapField(entries, field)
		if ferr != nil {
			return nil, fmt.Errorf("%w: %s.%s: %w", ErrMalformedPayload, kind, field, ferr)
		}
		amt, aerr := scval.AsAmountFromI128(fv)
		if aerr != nil {
			return nil, fmt.Errorf("%w: %s.%s: %w", ErrMalformedPayload, kind, field, aerr)
		}
		out = append(out, amt)
	}
	return out, nil
}

// decodeWrapper decodes wrap (3 topics) and unwrap (4 topics: adds owner).
func decodeWrapper(e *events.Event, kind string) (Event, error) {
	want := 3
	if kind == EventUnwrap {
		want = 4
	}
	if len(e.Topic) < want {
		return Event{}, fmt.Errorf("%w: %s expects %d topics, got %d", ErrShortTopic, kind, want, len(e.Topic))
	}
	out, err := newEvent(e, kind)
	if err != nil {
		return Event{}, err
	}
	if out.Caller, err = decodeAddrTopic(e, 1, "caller"); err != nil {
		return Event{}, err
	}
	if out.Receiver, err = decodeAddrTopic(e, 2, "receiver"); err != nil {
		return Event{}, err
	}
	if kind == EventUnwrap {
		if out.Owner, err = decodeAddrTopic(e, 3, "owner"); err != nil {
			return Event{}, err
		}
	}
	amounts, err := bodyI128Fields(e, kind, "shares", "vault_shares")
	if err != nil {
		return Event{}, err
	}
	out.Shares, out.VaultShares = amounts[0], amounts[1]
	return out, nil
}
