package band

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder implements dispatcher.ContractCallDecoder: Band's contract emits zero events, so the relayer's relay()/force_relay() InvokeContract call carries the payload (docs/protocols/band.md). Stateless; matching is an O(1) string compare.
type Decoder struct {
	standardReferenceContract string
}

// NewDecoder constructs a Decoder bound to the given StandardReference contract (mainnet or testnet; the decoder doesn't know which).
func NewDecoder(standardReferenceContract string) *Decoder {
	return &Decoder{standardReferenceContract: standardReferenceContract}
}

// Name implements [dispatcher.ContractCallDecoder].
func (d *Decoder) Name() string { return SourceName }

// RequiresExecutionCorroboration implements [dispatcher.ExecutionCorroborationRequirer]. The call args ARE the price payload, so a call the attacker-controlled auth tree merely DECLARED (forged rates naming the contract, never executed) would be laundered into a recognised price; corroboration drops any call that isn't the op's top-level EXECUTED invocation.
func (d *Decoder) RequiresExecutionCorroboration() bool { return true }

// Matches implements [dispatcher.ContractCallDecoder]: the contract ID plus one of the two relay entry points; every other function is read-only or admin.
func (d *Decoder) Matches(contractID, functionName string) bool {
	if contractID != d.standardReferenceContract {
		return false
	}
	return functionName == FnRelay || functionName == FnForceRelay
}

// Decode implements [dispatcher.ContractCallDecoder], emitting one UpdateEvent per (symbol, rate) entry in symbol_rates after USD special-casing and unknown-symbol skips.
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
