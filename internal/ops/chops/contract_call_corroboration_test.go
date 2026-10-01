package chops

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// TestDecodeContractCallTree_RefusesAuthOnlyOracleCall: the census and
// ch-rebuild must apply the live dispatcher's execution-corroboration gate, or
// they expect and write oracle rows from an auth entry that never executed.
func TestDecodeContractCallTree_RefusesAuthOnlyOracleCall(t *testing.T) {
	cases := []struct {
		name string
		op   xdr.Operation
		want int
	}{
		{"relay declared only in the auth tree of a no-op call", invokeOp(invokeArgs(0x5A, "noop"), invokeArgs(0x11, "relay")), 0},
		{"top-level executed relay", invokeOp(invokeArgs(0x11, "relay")), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blind := completeness.NewBlindTracker()
			var emitted int
			err := decodeContractCallTree(clickhouse.ContractCallOp{Ledger: 7}, dispatcher.ExtractContractCallTree(tc.op),
				corroboratingRelayDecoder{}, blind, func(uint32, consumer.Event) error { emitted++; return nil })
			if err != nil {
				t.Fatalf("decodeContractCallTree: %v", err)
			}
			if emitted != tc.want {
				t.Errorf("emitted = %d, want %d", emitted, tc.want)
			}
			if blind.Result().Any() {
				t.Errorf("a refused uncorroborated call is not a blind spot: %+v", blind.Result())
			}
		})
	}
}
