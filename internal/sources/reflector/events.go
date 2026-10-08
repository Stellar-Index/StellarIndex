// Package reflector ingests oracle updates from the three Reflector contracts (DEX / CEX / FX),
// a SEP-40 oracle network native to Stellar / Soroban.
//
// Read internal/sources/reflector/README.md's Q1–Q5 quirks (and docs/protocols/reflector.md)
// before changing the decoder.
package reflector

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Source names, one per variant; used as metric labels and canonical.OracleUpdate.Source.
const (
	SourceDEX = "reflector-dex"
	SourceCEX = "reflector-cex"
	SourceFX  = "reflector-fx"
)

// Variant identifies which Reflector contract a Source targets, and so the SourceName it stamps.
type Variant uint8

const (
	VariantDEX Variant = iota + 1
	VariantCEX
	VariantFX
)

func (v Variant) SourceName() string {
	switch v {
	case VariantDEX:
		return SourceDEX
	case VariantCEX:
		return SourceCEX
	case VariantFX:
		return SourceFX
	default:
		return "reflector-unknown"
	}
}

// DefaultDecimals is the Reflector price scale unless [WithDecoderDecimals] overrides it. 14 is
// confirmed on-chain for DEX only and ASSUMED for CEX/FX: the pure decoder makes no startup RPC call,
// so a re-pointed `[oracle.reflector]` contract at another scale mis-scales prices silently; confirm its
// decimals() and set `dex_decimals` / `cex_decimals` / `fx_decimals`.
const DefaultDecimals uint8 = 14

// DefaultResolutionSeconds is every Reflector contract's 5-min cadence (Q3), exported as the
// `stellarindex_oracle_resolution_seconds` gauge so the oracle-stale alert has a per-source threshold.
const DefaultResolutionSeconds = 300

// Event topics, from reflector-contract/oracle/src/events.rs:4-10 (soroban-sdk 25.3.0):
//
//	topic[0] = Symbol("REFLECTOR")
//	topic[1] = Symbol("update")
//	topic[2] = U64(timestamp)
//	body     = Map { "update_data": Vec<(ScVal, I128)> }
const (
	EventTopic0 = "REFLECTOR"
	EventTopic1 = "update"
)

// Base64 SCVal::Symbol topic blobs for byte-equality matching, derived from [EventTopic0] /
// [EventTopic1]; TestGolden_symbolBytes in internal/scval pins the encoding across SDK upgrades.
var (
	TopicSymbolReflector = scval.MustEncodeSymbol(EventTopic0) // topic[0]
	TopicSymbolUpdate    = scval.MustEncodeSymbol(EventTopic1) // topic[1]
)

// Errors returned by the decode path.
var (
	// ErrNotReflectorEvent — topic[0..1] is not REFLECTOR + update; skip.
	ErrNotReflectorEvent = errors.New("reflector: not a REFLECTOR.update event")

	// ErrMalformedPayload — the body is not Map{"update_data": Vec<(Val, i128)>}.
	ErrMalformedPayload = errors.New("reflector: malformed event payload")

	// ErrEmptyPrices — every slot of a non-empty vector was non-positive (the contract filters
	// zeros, so defensive). An empty vector is a no-op and unmapped symbols are raw rows, not this error.
	ErrEmptyPrices = errors.New("reflector: empty prices vector")

	// ErrPriceVectorOverflow — the vector exceeds opIndexFanoutStride, so OpIndex would collide with
	// the next block on the oracle_updates primary key. Refused loudly; observed max is ~50 assets.
	ErrPriceVectorOverflow = errors.New("reflector: price vector exceeds OpIndex fanout stride")

	// ErrEventIndexOverflow — e.EventIndex >= eventFanoutStride would spill into the next operation's
	// OpIndex range: a decoder bug or a contract emitting far more events per op than observed.
	ErrEventIndexOverflow = errors.New("reflector: EventIndex exceeds OpIndex fanout stride")

	// ErrOperationIndexOverflow — e.OperationIndex is negative or >= opIndexFanoutMax, so OpIndex
	// would wrap uint32. Unreachable on-chain; a hit means a producer bug.
	ErrOperationIndexOverflow = errors.New("reflector: OperationIndex exceeds OpIndex fanout bound")
)
