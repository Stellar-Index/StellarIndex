package soroswap_router

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder implements dispatcher.ContractCallDecoder. The Soroswap router emits no events itself
// (its swaps call per-pair contracts that emit `SoroswapPair("swap")`), so it hooks the
// InvokeContract call to capture router-level intent (path + amounts + deadline) distinct from
// the per-pair legs, as Band's StandardReference decoder does.
//
// Stateless. Matching is an O(1) compare on (contract_id, function_name); SCVal args are parsed
// only for matching calls.
type Decoder struct {
	routerContract string
}

// NewDecoder constructs a router decoder bound to the given
// contract address. Mainnet callers pass [MainnetRouter];
// testnet / dev callers pass the testnet address.
func NewDecoder(routerContract string) *Decoder {
	return &Decoder{routerContract: routerContract}
}

// Name implements [dispatcher.ContractCallDecoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.ContractCallDecoder]: the contract ID is the configured router
// AND the function is one of the two swap entry points. Admin functions (set_pair_fee,
// set_protocol_fee_recipient, init, …) move no tokens and are useless for trade attribution.
func (d *Decoder) Matches(contractID, functionName string) bool {
	if contractID != d.routerContract {
		return false
	}
	return functionName == FnSwapExactTokensForTokens ||
		functionName == FnSwapTokensForExactTokens
}

// Decode implements [dispatcher.ContractCallDecoder]. Emits one
// Event per matched call. Returning an error is a "skip + count"
// signal per the dispatcher's contract — a malformed call doesn't
// abort the ledger, just gets dropped + counted.
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
