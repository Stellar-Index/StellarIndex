// Package band decodes on-chain price updates from Band Protocol's
// Soroban StandardReference contract.
//
// Architectural note: Band's Stellar contract **emits zero events**
// (verified 2026-04-22 via grep across
// bandprotocol/band-std-reference-contracts-soroban; confirmed
// 2026-04-24 against the pinned source). A conventional
// dispatcher.Decoder running on emitted events would never fire. So
// this package plugs into dispatcher.ContractCallDecoder instead —
// it observes the InvokeContract op itself, decoding the relayer's
// call args as the authoritative payload.
//
// Wire shape (verified
// .discovery-repos/band-soroban/src/contract.rs:23-35):
//
//	relay(from: Address, symbol_rates: Vec<(Symbol, u64)>,
//	      resolve_time: u64, request_id: u64)
//	force_relay(symbol_rates: Vec<(Symbol, u64)>,
//	            resolve_time: u64, request_id: u64)
//
// `force_relay` drops the `from` arg — admin-only path, not gated
// by the relayer check. Both produce the same logical output: one
// (Symbol, rate) pair per entry written to Band's ref_data storage.
//
// Rates: u64 at E9 scale (adapter/config.rs). Single-symbol rates
// are USD-denominated per the Band convention — `get_ref_data(XYZ)`
// returns XYZ priced in USD. Pair rates (`get_reference_data`) are
// computed on-read at E18; we don't emit those from relay calls
// because they're a function of storage state, not the wire input.
//
// Timestamps: resolve_time is UNIX seconds (
// band-soroban/src/storage/ref_data.rs:56 compares against
// `env.ledger().timestamp()` which is seconds).
//
// See docs/discovery/oracles/band.md for the full analysis.
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

// DefaultResolutionSeconds is Band's MEASURED relay cadence on
// mainnet: one hour.
//
// Emitted as the `stellarindex_oracle_resolution_seconds` gauge by
// [pipeline.BuildDispatcher] at registration time. It is not
// documentation — `stellarindex_oracle_stale` alerts at 10× this
// value, so the constant IS the alert threshold for this source.
//
// It was 60 until 2026-09-01, taken from "the poll-cadence
// recommendation in the discovery doc" — how often a CONSUMER might
// poll, not how often the relayer publishes. That made the threshold
// 10 minutes against an oracle that updates hourly, and
// `stellarindex_oracle_stale{source="band"}` fired for 100% of
// samples over the trailing 7 days, for both crypto:USDC and
// crypto:XLM. An alert that is always firing carries no information
// and desensitises the one signal that would show a real oracle
// outage.
//
// Measured on r1, 2026-09-01, over 24h:
//
//	changes(stellarindex_oracle_last_update_unix{source="band"}[24h])
//	  crypto:USDC = 24    crypto:XLM = 24     → every 3600s
//
// For contrast, the same query put reflector-dex at 301s against a
// declared 300 and reflector-fx at exactly 300 — band was the only
// source whose declared resolution disagreed with reality, and it
// disagreed by 60×.
//
// If Band's relayer cadence changes, RE-MEASURE with the query above
// rather than reasoning from its docs: this constant is wrong exactly
// when it is derived from intent instead of observation.
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

	// ErrEmptyRates — the symbol_rates vector was empty (or every
	// slot was USD / rate 0). Band relayers don't normally submit
	// empty batches; surface loudly. Since the oracle
	// capture-totality change (PR-2) an unmapped symbol is NOT a
	// reason: it is recorded verbatim as a `raw:<symbol>` row
	// (canonical.AssetOracleRaw). The former ErrUnknownSymbol
	// per-entry skip sentinel was retired with that change.
	ErrEmptyRates = errors.New("band: empty symbol_rates vector")
)
