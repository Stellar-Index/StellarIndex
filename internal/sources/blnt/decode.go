package blnt

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Tier names exactly as the contract emits them; stored verbatim.
var tiers = map[string]bool{"FirstLoss": true, "SecondLoss": true, "ThirdLoss": true}

// classify resolves topic[0] only within the ABI selected by contract role.
// Contract identity is established by Decoder.roleOf before this is called.
func classify(role Role, ev *events.Event) string {
	if len(ev.Topic) == 0 {
		return ""
	}
	return encodedRoleEvents[role][ev.Topic[0]]
}

// decodeEvent decodes one event strictly against its spec: a topic count, body arity or field type
// the ABI does not declare is an error, never a partial row.
func decodeEvent(role Role, kind string, ev *events.Event) (Event, error) {
	sp, ok := abi[role][kind]
	if !ok {
		return Event{}, ErrNotBLNTEvent
	}
	fail := func(what string, err error) (Event, error) {
		return Event{}, fmt.Errorf("%w: %s/%s %s: %w", ErrMalformedEvent, role, kind, what, err)
	}
	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return fail("close time", err)
	}
	out := Event{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex), //nolint:gosec // non-negative by Soroban event contract.
		EventIndex: uint32(ev.EventIndex),     //nolint:gosec // non-negative by Soroban event contract.
		ObservedAt: observedAt,
		Role:       role,
		EventType:  kind,
		Attributes: map[string]any{},
	}
	if role == RolePool {
		out.Pool = ev.ContractID
	}

	if err := out.setTopics(sp.topics, ev.Topic[1:]); err != nil {
		return fail("topics", err)
	}
	if err := out.setBody(sp, ev.Value); err != nil {
		return fail("body", err)
	}
	return out, nil
}

func (e *Event) setTopics(declared []field, got []string) error {
	if len(got) > len(declared) || (len(got) < len(declared) && !declared[len(got)].optional) {
		return fmt.Errorf("got %d after the symbol, ABI declares %d", len(got), len(declared))
	}
	for i, b64 := range got {
		sv, err := scval.Parse(b64)
		if err == nil {
			err = e.set(declared[i], sv)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", declared[i].name, err)
		}
	}
	return nil
}

func (e *Event) setBody(sp spec, b64 string) error {
	body, err := scval.Parse(b64)
	if err != nil {
		return err
	}
	if sp.body == bodyUnit {
		if !scval.IsVoid(body) {
			return errors.New("ABI declares no payload, got one")
		}
		return nil
	}
	values := []scval.ScVal{body}
	if sp.body == bodyVec {
		if values, err = scval.AsTupleN(body, len(sp.data)); err != nil {
			return err
		}
	}
	for i, f := range sp.data {
		if err := e.set(f, values[i]); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// set decodes sv as f's declared type into f's column.
func (e *Event) set(f field, sv scval.ScVal) error {
	var v any
	var err error
	switch f.typ {
	case tAddr:
		v, err = scval.AsAddressStrkey(sv)
	case tOptAddr:
		var s string
		if s, err = scval.AsAddressOrVoid(sv); err == nil && s != "" {
			v = s
		}
	case tI128:
		a, aerr := scval.AsAmountFromI128(sv)
		v, err = a.String(), aerr
	case tU32:
		v, err = scval.AsU32(sv)
	case tU64:
		n, nerr := scval.AsU64(sv)
		v, err = fmt.Sprintf("%d", n), nerr
	case tTier:
		v, err = decodeTier(sv)
	case tAny:
		v, err = scval.ToJSON(sv)
	}
	if err != nil {
		return err
	}
	if f.col == cAttr {
		e.Attributes[f.name] = v
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("promoted field decoded to %T, want string", v)
	}
	switch f.col {
	case cPool:
		e.Pool = s
	case cUser:
		e.UserAddress = s
	case cAsset:
		e.Asset = s
	case cTier:
		e.Tier = s
	case cAmount:
		e.Amount = s
	case cAmount2:
		e.Amount2 = s
	case cAttr:
	}
	return nil
}

func decodeTier(sv scval.ScVal) (string, error) {
	parts, err := scval.AsTupleN(sv, 1)
	if err != nil {
		return "", err
	}
	name, err := scval.AsSymbol(parts[0])
	if err != nil {
		return "", err
	}
	if !tiers[name] {
		return "", fmt.Errorf("unknown backstop tier %q", name)
	}
	return name, nil
}
