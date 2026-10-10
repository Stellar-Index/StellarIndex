package chops

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// corroboratingRelayDecoder claims every relay() call and requires execution
// corroboration, as the band adapter does.
type corroboratingRelayDecoder struct{}

func (corroboratingRelayDecoder) Name() string                         { return "stub-oracle" }
func (corroboratingRelayDecoder) Matches(_, fn string) bool            { return fn == "relay" }
func (corroboratingRelayDecoder) RequiresExecutionCorroboration() bool { return true }
func (corroboratingRelayDecoder) Decode(dispatcher.ContractCallContext) ([]consumer.Event, error) {
	return []consumer.Event{stubCallEvent{}}, nil
}

func invokeArgs(fill byte, fn string) *xdr.InvokeContractArgs {
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = fill
	}
	return &xdr.InvokeContractArgs{
		ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
		FunctionName:    xdr.ScSymbol(fn),
	}
}

func invokeOp(top *xdr.InvokeContractArgs, auth ...*xdr.InvokeContractArgs) xdr.Operation {
	entries := make([]xdr.SorobanAuthorizationEntry, 0, len(auth))
	for _, a := range auth {
		entries = append(entries, xdr.SorobanAuthorizationEntry{
			Credentials: xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount},
			RootInvocation: xdr.SorobanAuthorizedInvocation{
				Function: xdr.SorobanAuthorizedFunction{
					Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
					ContractFn: a,
				},
			},
		})
	}
	return xdr.Operation{Body: xdr.OperationBody{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
			HostFunction: xdr.HostFunction{Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract, InvokeContract: top},
			Auth:         entries,
		},
	}}
}
