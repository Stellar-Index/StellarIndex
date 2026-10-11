package dispatcher

import "github.com/stellar/go-stellar-sdk/xdr"

// ContractCall is the public projection of one extracted InvokeContract call: the invoked contract
// (C-strkey), function symbol, base64 SCVal args, and the auth-tree CallPath ([] for the top-level
// call).
//
// Off-line tooling (stellarindex-ops compute-completeness) uses it to replay what the live
// dispatcher routes to ContractCallDecoders over the ClickHouse lake. Event-less sources (band,
// soroswap-router) have no soroban_events landing zone, so their census is re-derived by streaming
// InvokeContract ops and running this extraction plus the source's decoder over each.
type ContractCall struct {
	ContractID   string
	FunctionName string
	Args         []string
	CallPath     []int
	// CallPathContracts is the ordered ancestor+self contract chain —
	// see ContractCallContext.CallPathContracts. Always ends in
	// ContractID.
	CallPathContracts []string
	// AuthOccurrence — see ContractCallContext.AuthOccurrence.
	AuthOccurrence int
	// ExecutionCorroborated — see ContractCallContext.ExecutionCorroborated.
	// Callers must gate on it via [RefusesUncorroborated].
	ExecutionCorroborated bool
}

// ExtractContractCallTree returns every InvokeContract call reachable from one operation: the
// top-level call (always, at CallPath []) plus every call in the Soroban auth tree (ADR-0052).
// It is byte-identical to what the live dispatcher feeds its ContractCallDecoders, including the
// ExecutionCorroborated flag, so a re-derived census reconciles by the same routing logic.
// Returns nil for non-InvokeContract ops.
//
// The top-level call is emitted even when the op carries an auth array: an auth entry's
// RootInvocation is the root of the subtree needing authorization, not necessarily the op's entry
// point, so a no-auth-fallback-only emit would export the nested auth root at CallPath [] and never
// the real root. See extractInvokeContractCallTrees for the re-rooting rule and its dedup.
func ExtractContractCallTree(op xdr.Operation) []ContractCall {
	trees := extractInvokeContractCallTrees([]xdr.Operation{op})
	if len(trees) == 0 || trees[0] == nil {
		return nil
	}
	top := extractInvokeContractCalls([]xdr.Operation{op})[0]
	out := make([]ContractCall, 0, len(trees[0]))
	for _, c := range trees[0] {
		out = append(out, ContractCall{
			ContractID:            c.ContractID,
			FunctionName:          c.FunctionName,
			Args:                  c.Args,
			CallPath:              c.CallPath,
			CallPathContracts:     c.CallPathContracts,
			AuthOccurrence:        c.AuthOccurrence,
			ExecutionCorroborated: executionCorroborated(top, c),
		})
	}
	return out
}
