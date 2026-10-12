package dispatcher

import "github.com/stellar/go-stellar-sdk/xdr"

// ContractCall is the public projection of one extracted InvokeContract call: contract (C-strkey), function symbol, base64 SCVal args and auth-tree CallPath ([] for the top-level call).
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

// ExtractContractCallTree returns every InvokeContract call in one operation (top-level at CallPath [], plus the auth tree), byte-identical to what the live dispatcher routes, including ExecutionCorroborated; nil for non-InvokeContract ops.
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
