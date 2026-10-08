package discovery

import (
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Kind names the sniffer that produced a [Hit]. Its values are stored in
// discovered_assets.discovery_kind, so renaming one is a wire break.
type Kind string

const (
	// KindSEP41 marks a [Hit] produced by [Sniff].
	KindSEP41 Kind = "sep41"
	// KindOracleEvent marks a [Hit] produced by [SniffOracleEvent].
	KindOracleEvent Kind = "oracle_event"
	// KindOracleCall marks a [Hit] produced by [SniffOracleCall].
	KindOracleCall Kind = "oracle_call"
)

// SEP41EventType identifies which of the four SEP-41 event topics
// fired. Stable string values appear in the discovered_assets table
// and metric labels — renaming is a wire break.
type SEP41EventType string

const (
	// EventTransfer fires on `transfer` events. Topic shape:
	// ("transfer", from, to, sep0011_asset?). The asset topic
	// position-3 was added post-P23 (CAP-67) for unified events.
	EventTransfer SEP41EventType = "transfer"

	// EventMint fires on `mint`. The topic shape varies (legacy SAC carries an
	// admin), which only topic[0] readers can ignore.
	EventMint SEP41EventType = "mint"

	// EventBurn fires on `burn` events. Topic shape:
	// ("burn", from, sep0011_asset?). Distinct from EventClawback
	// — burn is voluntary, clawback is admin-driven.
	EventBurn SEP41EventType = "burn"

	// EventClawback fires on `clawback`; frequent clawbacks are a compliance
	// signal distinct from voluntary burns.
	EventClawback SEP41EventType = "clawback"
)

// Hit is one sighting from a sniffer: the C-strkey of the contract, which
// sniffer, and when.
type Hit struct {
	ContractID string
	// Kind is always set by a sniffer; Recorders treat an empty Kind as
	// KindSEP41.
	Kind Kind
	// EventType is set only for KindSEP41, the field existing consumers
	// (first_seen_event, metric labels) read.
	EventType SEP41EventType
	// Symbol is the matched topic[0] symbol or function name, set for
	// every Kind.
	Symbol string
	Ledger uint32
	// ObservedAtRFC3339 is the ledger close time, kept as a string to stay
	// allocation-light on the hot path.
	ObservedAtRFC3339 string
	// Count is the observations this Record represents; zero means 1.
	// [AsyncSink] sets it when flushing a dedup delta.
	Count int64
}

// Sniff reports whether a contract event's topic[0] is one of the four SEP-41
// symbols. It is pure and does not check arity beyond topic[0]: older SEP-41
// contracts emit three-topic transfers, and decoders reject bad bodies.
func Sniff(ev events.Event) (Hit, bool) {
	sym, ok := parseTopic0Symbol(ev)
	if !ok {
		return Hit{}, false
	}

	eventType, ok := classifySymbol(sym)
	if !ok {
		return Hit{}, false
	}

	return Hit{
		ContractID:        ev.ContractID,
		Kind:              KindSEP41,
		EventType:         eventType,
		Symbol:            sym,
		Ledger:            ev.Ledger,
		ObservedAtRFC3339: ev.LedgerClosedAt,
	}, true
}

// parseTopic0Symbol decodes a contract event's topic[0] as a Symbol, with the
// guards [Sniff] and [SniffOracleEvent] share.
func parseTopic0Symbol(ev events.Event) (string, bool) {
	if ev.Type != "contract" {
		return "", false
	}
	if ev.ContractID == "" {
		return "", false
	}
	if len(ev.Topic) == 0 {
		return "", false
	}

	sv, err := scval.Parse(ev.Topic[0])
	if err != nil {
		return "", false
	}
	sym, err := scval.AsSymbol(sv)
	if err != nil {
		return "", false
	}
	return sym, true
}

// classifySymbol maps an SCVal symbol string to its [SEP41EventType].
// Returns ok=false for symbols that aren't SEP-41 events (e.g. the
// many DEX-specific symbols this repo also routes — `swap`, `sync`,
// `deposit`, `withdraw`, etc.).
func classifySymbol(sym string) (SEP41EventType, bool) {
	switch sym {
	case "transfer":
		return EventTransfer, true
	case "mint":
		return EventMint, true
	case "burn":
		return EventBurn, true
	case "clawback":
		return EventClawback, true
	default:
		return "", false
	}
}

// oracleEventSymbols is the topic[0] list from the lake census in
// docs/architecture/oracle-manipulation-defense.md. The census found false
// positives on it, which is why a match is a sighting, never an attribution.
var oracleEventSymbols = map[string]struct{}{
	"price":             {},
	"prices":            {},
	"lastprice":         {},
	"last_price":        {},
	"x_last_price":      {},
	"set_price":         {},
	"update_price":      {},
	"price_update":      {},
	"new_price":         {},
	"oracle":            {},
	"Oracle":            {},
	"ORACLE":            {},
	"feed":              {},
	"PriceData":         {},
	"resolution":        {},
	"write_prices":      {},
	"relay":             {},
	"force_relay":       {},
	"REFLECTOR":         {},
	"REDSTONE":          {},
	"rate":              {},
	"rates":             {},
	"set_rate":          {},
	"symbol_rates":      {},
	"StandardReference": {},
	"update":            {},
	"base":              {},
	"decimals":          {},
	"assets":            {},
}

// SniffOracleEvent reports whether an event's topic[0] is in
// [oracleEventSymbols], so a new oracle is sighted before any decoder knows it.
// The set is disjoint from [Sniff]'s.
func SniffOracleEvent(ev events.Event) (Hit, bool) {
	sym, ok := parseTopic0Symbol(ev)
	if !ok {
		return Hit{}, false
	}
	if _, ok := oracleEventSymbols[sym]; !ok {
		return Hit{}, false
	}
	return Hit{
		ContractID:        ev.ContractID,
		Kind:              KindOracleEvent,
		Symbol:            sym,
		Ledger:            ev.Ledger,
		ObservedAtRFC3339: ev.LedgerClosedAt,
	}, true
}

// oracleCallFunctions is the InvokeContract allow-list from the same note's
// "Event-less discovery"; every entry is also in [oracleEventSymbols].
var oracleCallFunctions = map[string]struct{}{
	"lastprice":    {},
	"price":        {},
	"prices":       {},
	"relay":        {},
	"force_relay":  {},
	"write_prices": {},
	"x_last_price": {},
}

// OracleCallInput is the call shape [SniffOracleCall] needs, declared here
// because internal/dispatcher imports this package.
type OracleCallInput struct {
	ContractID        string
	FunctionName      string
	Ledger            uint32
	ObservedAtRFC3339 string
}

// SniffOracleCall reports whether an InvokeContract function name is in
// [oracleCallFunctions], catching oracles that update storage without an event
// (Band's relay). One map lookup; args are not inspected.
func SniffOracleCall(in OracleCallInput) (Hit, bool) {
	if in.ContractID == "" || in.FunctionName == "" {
		return Hit{}, false
	}
	if _, ok := oracleCallFunctions[in.FunctionName]; !ok {
		return Hit{}, false
	}
	return Hit{
		ContractID:        in.ContractID,
		Kind:              KindOracleCall,
		Symbol:            in.FunctionName,
		Ledger:            in.Ledger,
		ObservedAtRFC3339: in.ObservedAtRFC3339,
	}, true
}
