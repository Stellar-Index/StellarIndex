package soroswap_router

import (
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// Two identical router calls authorized side by side in ONE auth entry are two
// executions (each consumes its own node), so both must survive the served
// PK's ON CONFLICT; the same call repeated in a SECOND entry is a co-signing
// duplicate and must still collapse. Both shapes are built from the real
// aggregator-wrapped fixture so the production extraction path is exercised.

// findRouterNode returns the parent of the first router node under root
// (nil when root itself is the router) and the router's index in it.
func findRouterNode(root *sdkxdr.SorobanAuthorizedInvocation) (*sdkxdr.SorobanAuthorizedInvocation, int, bool) {
	for i := range root.SubInvocations {
		child := &root.SubInvocations[i]
		if isRouterNode(child) {
			return root, i, true
		}
		if p, idx, ok := findRouterNode(child); ok {
			return p, idx, ok
		}
	}
	return nil, 0, false
}

func isRouterNode(n *sdkxdr.SorobanAuthorizedInvocation) bool {
	if n.Function.Type != sdkxdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn {
		return false
	}
	fn := n.Function.MustContractFn()
	addr, err := fn.ContractAddress.String()
	return err == nil && addr == MainnetRouter && string(fn.FunctionName) == FnSwapExactTokensForTokens
}

// routerEntry returns the index of the auth entry whose tree holds the router.
func routerEntry(t *testing.T, ihf *sdkxdr.InvokeHostFunctionOp) int {
	t.Helper()
	for j := range ihf.Auth {
		root := &ihf.Auth[j].RootInvocation
		if isRouterNode(root) {
			return j
		}
		if _, _, ok := findRouterNode(root); ok {
			return j
		}
	}
	t.Fatal("fixture has no router node in its auth tree")
	return -1
}

func cloneAuthEntry(t *testing.T, e sdkxdr.SorobanAuthorizationEntry) sdkxdr.SorobanAuthorizationEntry {
	t.Helper()
	raw, err := e.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal auth entry: %v", err)
	}
	var out sdkxdr.SorobanAuthorizationEntry
	if err := out.UnmarshalBinary(raw); err != nil {
		t.Fatalf("unmarshal auth entry: %v", err)
	}
	return out
}

func TestAuthOccurrence_SiblingIdenticalCallsStayDistinct(t *testing.T) {
	t.Parallel()
	const tx = "da2ffe5a8651e2289408631a180cc8f6fe26247c6558bfc80dbf6d9527849cc7"
	pristine := decodeRouterCallsFromOp(t, loadRealOp(t, "router_subinvocation_op_ledger62029020.b64"), 62_029_020, tx)
	if len(pristine) != 1 {
		t.Fatalf("pristine fixture decoded %d router swaps, want 1", len(pristine))
	}

	op := loadRealOp(t, "router_subinvocation_op_ledger62029020.b64")
	ihf := op.Body.MustInvokeHostFunctionOp()
	j := routerEntry(t, &ihf)
	parent, idx, ok := findRouterNode(&ihf.Auth[j].RootInvocation)
	if !ok {
		t.Fatal("router is an auth-entry root in this fixture; test needs a nested router")
	}
	// A second, byte-identical router call authorized as a sibling of the first.
	parent.SubInvocations = append(parent.SubInvocations, parent.SubInvocations[idx])
	op.Body.InvokeHostFunctionOp = &ihf

	swaps := decodeRouterCallsFromOp(t, op, 62_029_020, tx)
	if len(swaps) != 2 {
		t.Fatalf("decoded %d router swaps, want 2", len(swaps))
	}
	if swaps[0].CallSig() == swaps[1].CallSig() {
		t.Fatalf("two authorized executions in one entry share call_sig %q: the served PK keeps only one", swaps[0].CallSig())
	}
	if swaps[0].CallSig() != pristine[0].CallSig() {
		t.Errorf("first occurrence call_sig = %q, want the pre-existing %q (stored rows must keep their PK)",
			swaps[0].CallSig(), pristine[0].CallSig())
	}
}

func TestAuthOccurrence_CrossEntryDuplicateStillCollapses(t *testing.T) {
	t.Parallel()
	op := loadRealOp(t, "router_subinvocation_op_ledger62029020.b64")
	ihf := op.Body.MustInvokeHostFunctionOp()
	j := routerEntry(t, &ihf)
	// A co-signer's entry authorizing the very same call tree.
	ihf.Auth = append(ihf.Auth, cloneAuthEntry(t, ihf.Auth[j]))
	op.Body.InvokeHostFunctionOp = &ihf

	swaps := decodeRouterCallsFromOp(t, op, 62_029_020, "da2ffe5a")
	if len(swaps) != 2 {
		t.Fatalf("decoded %d router swaps, want 2 (one per entry)", len(swaps))
	}
	if swaps[0].CallSig() != swaps[1].CallSig() {
		t.Errorf("co-signing duplicate across entries got distinct call_sigs %q vs %q: it would be stored twice",
			swaps[0].CallSig(), swaps[1].CallSig())
	}
}
