// Package band decodes on-chain price updates from Band Protocol's Soroban
// StandardReference contract.
//
// Band's Stellar contract **emits zero events**, so this package plugs into
// dispatcher.ContractCallDecoder and decodes the relayer's call args (relay / force_relay:
// symbol_rates Vec<(Symbol, u64)>, resolve_time, request_id) as the payload.
//
// Rates are u64 at E9 scale, USD-denominated per symbol. Pair rates are computed on read at
// E18, so relay calls do not carry them. resolve_time is UNIX seconds.
//
// See docs/protocols/band.md for the full analysis.
package band

import "errors"

// SourceName is stamped on every OracleUpdate this package emits.
// Single source — Band has one StandardReference contract per
// network.
const SourceName = "band"

// DefaultDecimals is the Band single-symbol rate scale —
// `E9 = 10^9` per band-soroban/src/constant.rs. Every relayed rate
// is u64 at this scale.
const DefaultDecimals uint8 = 9

// DefaultResolutionSeconds is Band's MEASURED mainnet relay cadence: one hour.
//
// [pipeline.BuildDispatcher] emits it as the `stellarindex_oracle_resolution_seconds` gauge
// and `stellarindex_oracle_stale` alerts at 10x this value, so the constant IS the alert
// threshold; a too-small value fires on every sample and hides a real outage. If the cadence
// changes, re-measure with changes(stellarindex_oracle_last_update_unix{source="band"}[24h])
// rather than reasoning from docs.
const DefaultResolutionSeconds = 3600

// Relay function names on the StandardReference contract. Both
// produce symbol_rates updates; the decoder matches either.
const (
	FnRelay      = "relay"
	FnForceRelay = "force_relay"
)

// Errors returned by the decode path.
var (
	// ErrNotBandCall — the ContractCallContext's contract+function
	// pair doesn't identify a Band relay/force_relay call. Skip;
	// the decoder only owns these two functions.
	ErrNotBandCall = errors.New("band: not a StandardReference relay/force_relay call")

	// ErrMalformedArgs — the op args don't decode to the expected shape: a contract upgrade
	// shifted the signature, or the envelope is broken.
	ErrMalformedArgs = errors.New("band: malformed InvokeContract args")

	// ErrEmptyRates — every slot of a non-empty symbol_rates vector was USD / rate 0, or
	// relay()'s resolve_time is outside the contract's window (an empty vector is a no-op, not
	// this error). An unmapped symbol is NOT a reason: it is recorded as `raw:<symbol>`.
	ErrEmptyRates = errors.New("band: empty symbol_rates vector")
)
