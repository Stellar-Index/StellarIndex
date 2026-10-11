// Package band decodes on-chain price updates from Band Protocol's Soroban
// StandardReference contract.
//
// Band's Stellar contract **emits zero events**, so a conventional
// dispatcher.Decoder would never fire. This package plugs into
// dispatcher.ContractCallDecoder instead: it observes the InvokeContract op
// and decodes the relayer's call args (relay / force_relay, carrying
// symbol_rates: Vec<(Symbol, u64)>, resolve_time, request_id) as the payload.
//
// Rates are u64 at E9 scale, USD-denominated per symbol (`get_ref_data(XYZ)`
// prices XYZ in USD). Pair rates (`get_reference_data`) are computed on read
// at E18 from storage state, so relay calls do not emit them. resolve_time is
// UNIX seconds.
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

// DefaultResolutionSeconds is Band's MEASURED relay cadence on mainnet: one
// hour.
//
// [pipeline.BuildDispatcher] emits it as the
// `stellarindex_oracle_resolution_seconds` gauge, and
// `stellarindex_oracle_stale` alerts at 10× this value, so the constant IS the
// alert threshold. A 60 taken from the discovery doc's consumer poll
// recommendation (not the relayer's publish cadence) made that alert fire for
// 100% of samples; an always-firing alert hides a real outage.
//
// Measured on r1 over 24h: changes(stellarindex_oracle_last_update_unix
// {source="band"}[24h]) = 24 for both crypto:USDC and crypto:XLM, i.e. every
// 3600s. If Band's cadence changes, RE-MEASURE with that query rather than
// reasoning from docs.
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

	// ErrMalformedArgs — the op args don't decode to the expected
	// shape for the claimed function. Either a contract upgrade
	// shifted the signature, or the envelope is broken.
	ErrMalformedArgs = errors.New("band: malformed InvokeContract args")

	// ErrEmptyRates — every slot of a non-empty symbol_rates vector
	// was USD / rate 0, or relay()'s resolve_time falls outside the
	// contract's acceptance window (an empty vector is a no-op, not
	// this error). An unmapped symbol is NOT a reason: it is recorded
	// verbatim as a `raw:<symbol>` row (canonical.AssetOracleRaw).
	ErrEmptyRates = errors.New("band: empty symbol_rates vector")
)
