package sushiswap_v3

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// MainnetPositionManager is the SushiSwap V3 position manager on pubnet: it holds every position it opens on a pool, so
// pool-side rows name it as `owner`, and its own events carry the token id that links a position to its end-user owner.
const MainnetPositionManager = "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4"

// PositionManagerGenesisLedger is the manager's first event (a `mint` + `increase_liquidity`).
const PositionManagerGenesisLedger uint32 = 61_487_383

// MainnetPositionManagers is the live manager-role set; empty until the manager's WASM hashes are audited.
var MainnetPositionManagers []string

// Position-manager topic names beyond the pool set it shares (`mint`, `burn`, `collect`, `upgraded`, `migrated`).
const (
	EventIncreaseLiquidity = "increase_liquidity"
	EventDecreaseLiquidity = "decrease_liquidity"
)

var (
	TopicSymbolIncreaseLiquidity = scval.MustEncodeSymbol(EventIncreaseLiquidity)
	TopicSymbolDecreaseLiquidity = scval.MustEncodeSymbol(EventDecreaseLiquidity)
)

// managerShape is one verified position-manager event: its topic count and the exact body field set (a Map, by name).
type managerShape struct {
	topics int
	fields []string
}

// managerShapes is the post-upgrade wire surface verified against the lake. A body with any other field set, field type or
// topic count is refused: the pre-upgrade shape is unverified, so it must fail closed rather than be guessed.
var managerShapes = map[string]managerShape{
	EventMint:              {topics: 2, fields: []string{"token_id"}},
	EventBurn:              {topics: 2, fields: []string{"token_id"}},
	EventIncreaseLiquidity: {topics: 1, fields: []string{"amount0", "amount1", "liquidity", "token_id"}},
	EventDecreaseLiquidity: {topics: 1, fields: []string{"amount0", "amount1", "liquidity", "token_id"}},
	EventCollect:           {topics: 1, fields: []string{"amount0", "amount1", "recipient", "token_id"}},
}

// classifyManager resolves topic[0] within the manager's ABI; lifecycle events are recognised for the zero-row path.
func classifyManager(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	var kind string
	switch e.Topic[0] {
	case TopicSymbolMint:
		kind = EventMint
	case TopicSymbolBurn:
		kind = EventBurn
	case TopicSymbolIncreaseLiquidity:
		kind = EventIncreaseLiquidity
	case TopicSymbolDecreaseLiquidity:
		kind = EventDecreaseLiquidity
	case TopicSymbolCollect:
		kind = EventCollect
	case TopicSymbolUpgraded, TopicSymbolMigrated:
		if len(e.Topic) != 1 {
			return ""
		}
		return classify(e)
	default:
		return ""
	}
	if len(e.Topic) != managerShapes[kind].topics {
		return ""
	}
	return kind
}

// ManagerFields is one decoded position-manager event. Owner is topic[1] of a mint / burn (the NFT holder); Recipient is
// a collect's payee; Liquidity (HasLiquidity) is set for increase / decrease, Amount0 / Amount1 (HasAmounts) for increase /
// decrease / collect. The Has flags say the field was on the wire: an absent field is NULL, never a zero.
type ManagerFields struct {
	TokenID      uint32
	Owner        string
	Recipient    string
	Liquidity    canonical.Amount
	HasLiquidity bool
	Amount0      canonical.Amount
	Amount1      canonical.Amount
	HasAmounts   bool
}

// decodeManagerFields decodes a manager event of a known kind, refusing anything off the verified shape.
func decodeManagerFields(kind string, topics []string, valueB64 string) (ManagerFields, error) {
	shape := managerShapes[kind]
	fail := func(what string, err error) (ManagerFields, error) {
		return ManagerFields{}, fmt.Errorf("%w: manager %s.%s: %w", ErrMalformedPayload, kind, what, err)
	}
	body, err := scval.Parse(valueB64)
	if err != nil {
		return fail("body", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return fail("body", err)
	}
	if len(entries) != len(shape.fields) {
		return fail("body", fmt.Errorf("%d fields, want exactly %v", len(entries), shape.fields))
	}
	var out ManagerFields
	for _, name := range shape.fields {
		if err := out.set(name, entries); err != nil {
			return fail(name, err)
		}
	}
	if shape.topics == 2 {
		sv, err := scval.Parse(topics[1])
		if err != nil {
			return fail("topic[1]", err)
		}
		if out.Owner, err = scval.AsAddressStrkey(sv); err != nil {
			return fail("topic[1]", err)
		}
	}
	return out, nil
}

// set decodes one named field of the body Map.
func (f *ManagerFields) set(name string, entries []scval.ScMapEntry) error {
	sv, err := scval.MustMapField(entries, name)
	if err != nil {
		return err
	}
	switch name {
	case "token_id":
		f.TokenID, err = scval.AsU32(sv)
	case "amount0":
		f.Amount0, err = scval.AsAmountFromU128(sv)
		f.HasAmounts = true
	case "amount1":
		f.Amount1, err = scval.AsAmountFromU128(sv)
	case "liquidity":
		f.Liquidity, err = scval.AsAmountFromU128(sv)
		f.HasLiquidity = true
	case "recipient":
		f.Recipient, err = scval.AsAddressStrkey(sv)
	default:
		err = fmt.Errorf("no decoder for field %q", name)
	}
	return err
}

// ManagerEvent is the [consumer.Event] for one position-manager mint / burn / increase_liquidity / decrease_liquidity /
// collect; Kind discriminates and the sink writes one sushiswap_v3_manager_events row.
type ManagerEvent struct {
	ContractID string // position manager C-strkey (emitter)
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       string
	ManagerFields
}

// EventKind implements [consumer.Event].
func (ManagerEvent) EventKind() string { return "sushiswap_v3.manager" }

// Source implements [consumer.Event] — matches [SourceName].
func (ManagerEvent) Source() string { return SourceName }

var _ consumer.Event = ManagerEvent{}

// emitManager decodes one manager event; lifecycle events project zero rows like a pool's.
func emitManager(ev events.Event, kind string) ([]consumer.Event, error) {
	if kind == EventUpgraded || kind == EventMigrated {
		return nil, nil
	}
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	fields, err := decodeManagerFields(kind, ev.Topic, ev.Value)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{ManagerEvent{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex),
		//nolint:gosec // EventIndex is non-negative by Soroban spec.
		EventIndex:    uint32(ev.EventIndex),
		ObservedAt:    closedAt,
		Kind:          kind,
		ManagerFields: fields,
	}}, nil
}
