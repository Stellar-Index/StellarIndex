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

// classify returns the supply-event symbol for a Soroban event's
// topic[0] (one of mint / burn / clawback), or empty for any
// other topic. The pre-encoded base64 SCVal blobs in events.go
// make this a cheap string compare in the dispatch hot path.
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

// decodeAmount extracts the i128 event amount and converts it to
// *big.Int. Per ADR-0011 / ADR-0023 amounts are non-negative (the
// kind discriminates direction); the storage writer rejects negatives
// upstream, so this just guards against SDK quirks.
//
// The amount arrives in TWO on-chain SHAPES — the body (event.Value)
// is EITHER a bare i128 OR a CAP-67 map wrapping it (see amountScVal).
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

// amountScVal returns the i128-typed ScVal carrying the event amount,
// unwrapping the CAP-67 map form when the body is a map instead of a
// bare i128:
//
//	bare i128   (ScvI128)                      → the amount directly
//	CAP-67 map  { amount: i128, to_muxed_id }  → amount lives in the field
//
// A mint / transfer emits the MAP form for a muxed destination or when the
// issuer stamps a memo into `to_muxed_id`. An i128-only decode rejects those
// with ErrAmountNotI128 and drops the row, driving mint_total to zero so
// `burn_total > mint_total` trips the aggregator's dominant-burn guard. Decode
// by map-field NAME (`amount`), never by position
// (docs/architecture/ingest-pipeline.md#contract-schema-evolution).
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

// decodeCounterparty extracts the recipient (mint) or holder (burn / clawback)
// Address from the topic vector. Topic[0] is the event symbol; the counterparty
// POSITION depends on the on-chain SHAPE and topic COUNT alone does not
// disambiguate, so we branch on the TYPE of topic[2]:
//
//	mint / clawback
//	  legacy SAC     ["mint", admin(Addr), to(Addr)]            → counterparty = topic[2]
//	  CAP-67 / spec  ["mint", to(Addr), sep0011_asset(String)]  → counterparty = topic[1]
//	  bare SEP-41    ["mint", to(Addr)]                          → counterparty = topic[1]
//	burn             ["burn", from(Addr) (, sep0011_asset)]      → counterparty = topic[1] (all shapes)
//
// If topic[2] decodes as an Address it is the legacy admin-prefixed form;
// otherwise it is the sep0011_asset String (CAP-67) or absent, and the
// counterparty is topic[1]. A fixed-topic[2] decode would DROP the dominant
// CAP-67 shape (AsAddressStrkey returns ErrScValType on the String, so
// total_supply under-counts).
//
// Shorter topic vectors surface ErrShortTopic so the caller drops the row.
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
