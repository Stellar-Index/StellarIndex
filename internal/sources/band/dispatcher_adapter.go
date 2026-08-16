package band

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder implements dispatcher.ContractCallDecoder. Unlike the
// event-based Decoders in sibling packages, Band plugs in here
// because its Soroban contract emits zero events — the relayer's
// `relay()` / `force_relay()` InvokeContract call carries the full
// update payload. See docs/discovery/oracles/band.md + events.go.
//
// No goroutines, no state. Matching is by (contract_id, function
// name) — O(1) string compare, no SCVal parsing on the hot path.
type Decoder struct {
	standardReferenceContract string
}

// NewDecoder constructs a Band Decoder bound to the mainnet
// StandardReference contract. Testnet callers pass the testnet
// address instead; the decoder has no knowledge of which is which.
func NewDecoder(standardReferenceContract string) *Decoder {
	return &Decoder{standardReferenceContract: standardReferenceContract}
}

// Name implements [dispatcher.ContractCallDecoder].
func (d *Decoder) Name() string { return SourceName }

// RequiresExecutionCorroboration implements
// [dispatcher.ExecutionCorroborationRequirer]: Band's relay() /
// force_relay() call args ARE the price payload, decoded verbatim, so a
// call the attacker-controlled Soroban auth tree merely DECLARED (a
// source-account auth entry naming the StandardReference contract with
// forged rates, riding along in a successful tx without ever executing)
// would otherwise be laundered into a recognised oracle price. Requiring
// corroboration makes the dispatcher drop any Band call that is not the
// op's top-level EXECUTED invocation — the shape every genuine relayer
// update takes (docs/protocols/band.md: "observes the InvokeContract op
// itself"). W8.4a.
func (d *Decoder) RequiresExecutionCorroboration() bool { return true }

// Matches implements [dispatcher.ContractCallDecoder]. Cheap
// predicate — checks contract ID and one of the two relay entry
// points. Any other function on the same contract (get_ref_data,
// add_relayers, init, …) is read-only or admin and doesn't affect
// our price view.
func (d *Decoder) Matches(contractID, functionName string) bool {
	if contractID != d.standardReferenceContract {
		return false
	}
	return functionName == FnRelay || functionName == FnForceRelay
}

// Decode implements [dispatcher.ContractCallDecoder]. Emits zero
// or more UpdateEvent wrappers — one per (symbol, rate) entry in
// symbol_rates after USD special-casing and unknown-symbol skips.
func (d *Decoder) Decode(ctx dispatcher.ContractCallContext) ([]consumer.Event, error) {
	updates, err := decodeRelayArgs(
		ctx.FunctionName, ctx.Args,
		ctx.ContractID,
		ctx.Ledger, ctx.TxHash,
		ctx.OpIndex,
		ctx.OpSource, ctx.TxSource,
		ctx.ClosedAt,
	)
	if err != nil {
		return nil, err
	}
	out := make([]consumer.Event, 0, len(updates))
	for _, u := range updates {
		out = append(out, UpdateEvent{Update: u})
	}
	return out, nil
}
