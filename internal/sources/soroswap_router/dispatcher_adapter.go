package soroswap_router

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder implements dispatcher.ContractCallDecoder: the router emits no events (its swaps call per-pair contracts), so it hooks the InvokeContract call to capture router-level intent (path, amounts, deadline) apart from the per-pair legs. Stateless; matching is an O(1) (contract_id, function_name) compare and SCVal args are parsed only for matching calls.
type Decoder struct {
	routerContract string
}

// NewDecoder constructs a router decoder bound to routerContract ([MainnetRouter] on mainnet, the testnet address elsewhere).
func NewDecoder(routerContract string) *Decoder {
	return &Decoder{routerContract: routerContract}
}

// Name implements [dispatcher.ContractCallDecoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.ContractCallDecoder]: the contract ID is the configured router AND the function is one of the two swap entry points; admin functions move no tokens and are useless for trade attribution.
func (d *Decoder) Matches(contractID, functionName string) bool {
	if contractID != d.routerContract {
		return false
	}
	return functionName == FnSwapExactTokensForTokens ||
		functionName == FnSwapTokensForExactTokens
}

// Decode implements [dispatcher.ContractCallDecoder], emitting one Event per matched call; an error is the dispatcher's "skip + count" signal, so a malformed call is dropped and counted without aborting the ledger.
func (d *Decoder) Decode(ctx dispatcher.ContractCallContext) ([]consumer.Event, error) {
	swap, err := decodeRouterArgs(
		ctx.FunctionName, ctx.Args,
		ctx.ContractID,
		ctx.Ledger, ctx.TxHash,
		ctx.OpIndex,
		ctx.OpSource, ctx.TxSource,
		ctx.ClosedAt,
		ctx.CallPathContracts,
	)
	if err != nil {
		return nil, err
	}
	swap.AuthOccurrence = ctx.AuthOccurrence
	return []consumer.Event{Event{Swap: *swap}}, nil
}
