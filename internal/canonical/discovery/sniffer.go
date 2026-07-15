package discovery

import (
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Kind classifies which sniffer produced a [Hit]:
//
//   - [KindSEP41] — the original topic[0] sniffer, one of the four
//     SEP-41 event symbols (transfer/mint/burn/clawback). Populates
//     Hit.EventType (legacy field, unchanged) as well as Hit.Symbol.
//   - [KindOracleEvent] — the broader oracle-suggestive topic[0]
//     sniffer added per docs/architecture/generic-oracle-sep-onboarding.md
//     §3(b)(1). Only Hit.Symbol is populated.
//   - [KindOracleCall] — the ContractCallContext-path sniffer for
//     event-less oracles (the Band pattern), added per the same
//     note's §3(b)(2). Only Hit.Symbol is populated.
//
// Stable string values appear in discovered_assets.discovery_kind —
// renaming a value is a wire break.
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

	// EventMint fires on `mint` events. Topic shape is shape-dependent:
	// legacy SAC ("mint", admin, to); CAP-67/spec ("mint", to,
	// sep0011_asset?) — the dominant mainnet form (the admin was dropped
	// post-P23). The sniffer only reads topic[0], so the position shift is
	// immaterial here; the supply observer's [sep41_supply.decodeCounterparty]
	// handles it.
	EventMint SEP41EventType = "mint"

	// EventBurn fires on `burn` events. Topic shape:
	// ("burn", from, sep0011_asset?). Distinct from EventClawback
	// — burn is voluntary, clawback is admin-driven.
	EventBurn SEP41EventType = "burn"

	// EventClawback fires on `clawback` events. Topic shape is
	// shape-dependent: legacy SAC ("clawback", admin, from); CAP-67/spec
	// ("clawback", from, sep0011_asset?) — the dominant mainnet form.
	// Compliance-significant: a token with frequent clawbacks reads
	// differently from one with frequent voluntary burns.
	EventClawback SEP41EventType = "clawback"
)

// Hit is the structured result of a successful [Sniff],
// [SniffOracleEvent], or [SniffOracleCall]. ContractID is the
// C-strkey of the emitting/invoked contract; Kind identifies which
// sniffer produced the hit; Ledger + ObservedAt locate the
// observation in time.
//
// The struct is small and copyable; the [Recorder] is responsible
// for de-duplicating on ContractID before writing.
type Hit struct {
	ContractID string
	// Kind identifies which sniffer produced this Hit. Always set
	// (Sniff sets [KindSEP41] explicitly) — never left as the zero
	// value by a sniffer function; hand-built Hits in tests/legacy
	// callers that leave it empty are treated as KindSEP41 by
	// [Recorder] implementations for backward compatibility.
	Kind Kind
	// EventType identifies which SEP-41 topic fired. Populated ONLY
	// for Kind == KindSEP41 (or the legacy empty-Kind case) — kept
	// as its own typed field, unchanged, so existing SEP-41
	// consumers (discovered_assets.first_seen_event, metric labels)
	// are untouched by the broader discovery this file adds.
	EventType SEP41EventType
	// Symbol is the raw matched topic[0] symbol (KindOracleEvent) or
	// InvokeContract function name (KindOracleCall) that tripped the
	// sniffer. Also populated for KindSEP41 hits (mirroring
	// string(EventType)) so a single field lets callers read "what
	// got sighted" without branching on Kind.
	Symbol string
	Ledger uint32
	// ObservedAtRFC3339 is event.LedgerClosedAt (or the equivalent
	// ContractCallContext.ClosedAt, formatted) verbatim — the caller
	// parses to time.Time when needed (typically at recorder
	// boundary). Kept as string so the sniffer is allocation-light
	// in the hot dispatch path.
	ObservedAtRFC3339 string
}

// Sniff inspects an event and reports whether it matches a SEP-41
// topic shape. Returns (hit, true) when the event's topic[0] decodes
// to one of the four SEP-41 event symbols; (zero, false) otherwise.
//
// Sniff is pure: no I/O, no allocations beyond the SCVal parse.
// Designed to run in the dispatcher's hot path on every contract
// event without measurable overhead.
//
// Returns (zero, false) when:
//   - The event is not a contract event (Type != "contract").
//   - Topic is empty or topic[0] doesn't decode to a Symbol.
//   - The symbol value isn't one of the four SEP-41 events.
//   - ContractID is empty (defensive — should never happen for
//     contract events but the guard prevents writing junk to the
//     Recorder).
//
// Specifically does NOT validate topic arity beyond topic[0] —
// SEP-41 went through several revisions across Soroban genesis, and
// older contracts may emit transfer events with three topics rather
// than four. Discovery records the contract-id sighting; downstream
// decoders reject malformed bodies on their own schedule.
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

// parseTopic0Symbol extracts and decodes topic[0] as an SCVal Symbol,
// applying the same precondition checks [Sniff] and [SniffOracleEvent]
// both need: contract event, non-empty ContractID, non-empty Topic,
// topic[0] decodes to a Symbol. Shared so the two event-path sniffers
// don't duplicate (and can't drift on) the defensive-guard list.
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

// oracleEventSymbols is the oracle-suggestive topic[0] symbol set
// from the 2026-07-10 investigation's ClickHouse lake census
// (docs/architecture/generic-oracle-sep-onboarding.md §2 — the exact
// `WHERE topic_0_sym IN (...)` list the census ran against r1's
// `stellar.contract_events` table). Sighting one of these on a
// contract we don't already track flags it for operator review; it
// is NOT an attribution signal — the census itself found several
// false positives against this exact list (a beef-traceability
// anchor on `update`, dead RedStone test deployments on `REDSTONE`,
// tutorial contracts on `price_update`), which is precisely why
// discovery only records a sighting and never decodes/attributes.
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

// SniffOracleEvent inspects an event and reports whether its topic[0]
// matches the oracle-suggestive symbol set in [oracleEventSymbols].
// This is the event-path half of the discovery broadening described
// in docs/architecture/generic-oracle-sep-onboarding.md §3(b)(1): a
// NEW oracle deploying tomorrow with an event shape resembling
// SEP-40/RedStone/Band gets sighted here even though its contract id
// is unknown to every real decoder.
//
// Pure, allocation-light, and disjoint from [Sniff]'s four SEP-41
// symbols (no overlap between the two sets) — an event can trip at
// most one of the two event-path sniffers.
//
// Returns (zero, false) under the same preconditions as [Sniff]
// (non-contract event, empty ContractID/Topic, unparseable topic[0])
// PLUS when the symbol isn't in the oracle-suggestive set.
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

// oracleCallFunctions is the oracle-suggestive InvokeContract
// function-name allow-list from
// docs/architecture/generic-oracle-sep-onboarding.md §3(b)(2) — the
// curated candidate list the investigation named for the
// ContractCallContext path: `lastprice`, `price`, `prices`, `relay`,
// `force_relay`, `write_prices`, `x_last_price`. Every entry here is
// already a member of [oracleEventSymbols] (SEP-40 read methods and
// Band/RedStone write functions double as plausible event topics),
// so the two watch-lists never diverge on meaning, only on which
// dispatcher seam they're checked against.
var oracleCallFunctions = map[string]struct{}{
	"lastprice":    {},
	"price":        {},
	"prices":       {},
	"relay":        {},
	"force_relay":  {},
	"write_prices": {},
	"x_last_price": {},
}

// OracleCallInput is the minimal call-shape [SniffOracleCall] needs.
// Defined locally (rather than accepting a dispatcher.ContractCallContext
// value) to avoid an import cycle — internal/dispatcher already
// imports this package for the event-path hook.
type OracleCallInput struct {
	ContractID        string
	FunctionName      string
	Ledger            uint32
	ObservedAtRFC3339 string
}

// SniffOracleCall inspects one Soroban InvokeContract call (function
// name only — args are NOT inspected) and reports whether the
// invoked function name matches [oracleCallFunctions]. This is the
// event-less-oracle half of the discovery broadening
// (docs/architecture/generic-oracle-sep-onboarding.md §3(b)(2)): the
// seam Band uses (relay/force_relay update storage without
// publishing an event), generalized so a FUTURE event-less oracle
// under a different function name still gets sighted instead of
// being structurally invisible the way Band originally was.
//
// Cheap by construction: a single map lookup on functionName, no
// SCVal parsing, no argument decoding — safe to call on every
// InvokeContract call the dispatcher observes without measurable
// per-op overhead when nothing matches.
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
