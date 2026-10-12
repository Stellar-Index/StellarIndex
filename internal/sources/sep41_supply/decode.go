package sep41_supply

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

var (
	ErrUnknownSEP41Symbol = errors.New("sep41_supply: topic[0] is not a supply-affecting SEP-41 symbol")
	ErrShortTopic         = errors.New("sep41_supply: topic too short for event variant")
	ErrAmountNotI128      = errors.New("sep41_supply: event Value is not I128")
)

// classify returns the supply-event symbol for a Soroban event's topic[0] (mint / burn / clawback), or empty. Pre-encoded base64
// SCVal blobs in events.go make this a cheap string compare in the dispatch hot path.
func classify(ev *events.Event) string {
	if len(ev.Topic) == 0 {
		return ""
	}
	switch ev.Topic[0] {
	case TopicSymbolMint:
		return SymbolMint
	case TopicSymbolBurn:
		return SymbolBurn
	case TopicSymbolClawback:
		return SymbolClawback
	default:
		return ""
	}
}

// decodeAmount extracts the i128 event amount as a *big.Int. Amounts are non-negative (the kind
// gives direction) and the storage writer rejects negatives, so this only guards SDK quirks.
// The body is EITHER a bare i128 OR a CAP-67 map wrapping it (see amountScVal).
func decodeAmount(ev *events.Event) (*big.Int, error) {
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("sep41_supply: parse Value: %w", err)
	}
	amountSV, err := amountScVal(sv)
	if err != nil {
		return nil, err
	}
	amt, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return nil, fmt.Errorf("sep41_supply: i128 → amount: %w", err)
	}
	out := amt.BigInt()
	if out.Sign() < 0 {
		return nil, fmt.Errorf("sep41_supply: negative amount %s (kind discriminates direction)", out)
	}
	return out, nil
}

// amountScVal returns the i128 ScVal carrying the event amount, unwrapping the CAP-67 map form { amount: i128, to_muxed_id } when
// the body is a map instead of a bare i128. A mint / transfer emits the MAP form for a muxed destination or issuer memo; an
// i128-only decode would drop those rows with ErrAmountNotI128, driving mint_total to zero so the dominant-burn guard trips.
func amountScVal(sv xdr.ScVal) (xdr.ScVal, error) {
	switch sv.Type {
	case xdr.ScValTypeScvI128:
		return sv, nil
	case xdr.ScValTypeScvMap:
		entries, err := scval.AsMap(sv)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("sep41_supply: amount map: %w", err)
		}
		field, ok := scval.MapField(entries, "amount")
		if !ok {
			return xdr.ScVal{}, fmt.Errorf("%w: map body has no \"amount\" field", ErrAmountNotI128)
		}
		if field.Type != xdr.ScValTypeScvI128 {
			return xdr.ScVal{}, fmt.Errorf("%w: map \"amount\" is %s", ErrAmountNotI128, field.Type)
		}
		return field, nil
	default:
		return xdr.ScVal{}, fmt.Errorf("%w: got %s", ErrAmountNotI128, sv.Type)
	}
}

// decodeCounterparty extracts the recipient (mint) or holder (burn / clawback) Address. Topic COUNT does not disambiguate, so it
// branches on the TYPE of topic[2]: an Address is the legacy admin-prefixed ["mint", admin, to] (counterparty = topic[2]); otherwise
// it is the CAP-67 sep0011_asset String or absent and the counterparty is topic[1]. A fixed-topic[2] decode would drop the dominant
// CAP-67 shape (ErrScValType) and under-count total_supply. Shorter topic vectors surface ErrShortTopic.
func decodeCounterparty(ev *events.Event, kind string) (string, error) {
	switch kind {
	case SymbolBurn:
		return addressAtTopic(ev, 1)
	case SymbolMint, SymbolClawback:
		// Legacy admin-prefixed form iff topic[2] is itself an Address;
		// CAP-67 puts the sep0011_asset String there instead.
		if len(ev.Topic) >= 3 {
			if sv, err := scval.Parse(ev.Topic[2]); err == nil && sv.Type == xdr.ScValTypeScvAddress {
				return scval.AsAddressStrkey(sv)
			}
		}
		// CAP-67 (["mint", to, sep0011_asset]) or bare spec (["mint", to]).
		return addressAtTopic(ev, 1)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownSEP41Symbol, kind)
	}
}

// addressAtTopic parses topic[idx] as an Address strkey, surfacing
// ErrShortTopic when the vector is too short.
func addressAtTopic(ev *events.Event, idx int) (string, error) {
	if len(ev.Topic) <= idx {
		return "", fmt.Errorf("%w: expects topic[%d], got len=%d", ErrShortTopic, idx, len(ev.Topic))
	}
	sv, err := scval.Parse(ev.Topic[idx])
	if err != nil {
		return "", fmt.Errorf("sep41_supply: parse topic[%d]: %w", idx, err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return "", fmt.Errorf("sep41_supply: counterparty address at topic[%d]: %w", idx, err)
	}
	return addr, nil
}
