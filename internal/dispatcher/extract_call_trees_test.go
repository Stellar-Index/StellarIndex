package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// extractInvokeContractCallTrees is the #48 fix — walks the full
// Soroban auth tree per op, capturing every (contract_id,
// function_name, args) tuple reachable from each op's HostFunction.
// Pre-fix, the dispatcher only saw the top-level call; ~99.99% of
// soroswap-router invocations come through an aggregator and were
// invisible.

// helper — build a no-auth, top-level-only InvokeContract op.
func opInvokeNoAuth(t *testing.T, contractByte byte, fnName string) xdr.Operation {
	t.Helper()
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = contractByte
	}
	addr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &cid,
	}
	ic := xdr.InvokeContractArgs{
		ContractAddress: addr,
		FunctionName:    xdr.ScSymbol(fnName),
	}
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type:           xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &ic,
				},
				Auth: nil,
			},
		},
	}
}

// helper — build an InvokeContract args node for an auth tree.
func authNodeContract(t *testing.T, contractByte byte, fnName string, subs ...xdr.SorobanAuthorizedInvocation) xdr.SorobanAuthorizedInvocation {
	t.Helper()
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = contractByte
	}
	addr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &cid,
	}
	ic := xdr.InvokeContractArgs{
		ContractAddress: addr,
		FunctionName:    xdr.ScSymbol(fnName),
	}
	return xdr.SorobanAuthorizedInvocation{
		Function: xdr.SorobanAuthorizedFunction{
			Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
			ContractFn: &ic,
		},
		SubInvocations: subs,
	}
}

// TestExtractCallTrees_topLevelOnlyNoAuth — falls back to top-level
// invocation when the op has no Auth array. Pre-#48 behaviour
// preserved for this case.
func TestExtractCallTrees_topLevelOnlyNoAuth(t *testing.T) {
	op := opInvokeNoAuth(t, 0xAA, "do_thing")
	got := extractInvokeContractCallTrees([]xdr.Operation{op})
	if len(got) != 1 {
		t.Fatalf("got %d slots, want 1", len(got))
	}
	calls := got[0]
	if len(calls) != 1 {
		t.Fatalf("got %d calls in slot[0], want 1 (top-level only)", len(calls))
	}
	if calls[0].FunctionName != "do_thing" {
		t.Errorf("FunctionName = %q, want do_thing", calls[0].FunctionName)
	}
	if len(calls[0].CallPath) != 0 {
		t.Errorf("CallPath = %v, want empty (top-level)", calls[0].CallPath)
	}
}

// TestExtractCallTrees_authRootOnly — auth tree present with just a
// root (no sub-invocations). Returns exactly the root call. This is
// the equivalent of the pre-#48 baseline for txs that DO have auth
// entries; we expect no behavioural change for direct-router calls.
func TestExtractCallTrees_authRootOnly(t *testing.T) {
	root := authNodeContract(t, 0xBB, "swap_exact_tokens_for_tokens")
	op := opInvokeNoAuth(t, 0xBB, "swap_exact_tokens_for_tokens")
	op.Body.InvokeHostFunctionOp.Auth = []xdr.SorobanAuthorizationEntry{
		{RootInvocation: root},
	}
	got := extractInvokeContractCallTrees([]xdr.Operation{op})
	if len(got) != 1 {
		t.Fatalf("got %d slots, want 1", len(got))
	}
	if len(got[0]) != 1 {
		t.Fatalf("got %d calls, want 1 (root only)", len(got[0]))
	}
	if got[0][0].FunctionName != "swap_exact_tokens_for_tokens" {
		t.Errorf("FunctionName = %q, want swap_exact_tokens_for_tokens", got[0][0].FunctionName)
	}
	if len(got[0][0].CallPath) != 0 {
		t.Errorf("root CallPath = %v, want empty", got[0][0].CallPath)
	}
}

// TestExtractCallTrees_aggregatorWrappingRouter — the headline bug
// scenario. Top-level op calls aggregator.yeet, auth tree shows
// aggregator → router.swap → pair.swap. Pre-#48 the dispatcher saw
// only aggregator.yeet (no decoder matched). Post-fix it sees all
// three calls; the router decoder's Matches() picks the router node.
func TestExtractCallTrees_aggregatorWrappingRouter(t *testing.T) {
	// Build: aggregator(0xCC).yeet → router(0xDD).swap_exact_tokens_for_tokens → pair(0xEE).swap
	pair := authNodeContract(t, 0xEE, "swap")
	router := authNodeContract(t, 0xDD, "swap_exact_tokens_for_tokens", pair)
	root := authNodeContract(t, 0xCC, "yeet", router)
	op := opInvokeNoAuth(t, 0xCC, "yeet")
	op.Body.InvokeHostFunctionOp.Auth = []xdr.SorobanAuthorizationEntry{
		{RootInvocation: root},
	}

	got := extractInvokeContractCallTrees([]xdr.Operation{op})
	if len(got) != 1 {
		t.Fatalf("got %d slots, want 1", len(got))
	}
	calls := got[0]
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3 (aggregator + router + pair)", len(calls))
	}

	// Pre-order DFS: root, first child, child's child.
	want := []struct {
		fn       string
		pathLen  int
		pathLast int
	}{
		{"yeet", 0, -1},
		{"swap_exact_tokens_for_tokens", 1, 0},
		{"swap", 2, 0},
	}
	for i, w := range want {
		if calls[i].FunctionName != w.fn {
			t.Errorf("call[%d].FunctionName = %q, want %q", i, calls[i].FunctionName, w.fn)
		}
		if len(calls[i].CallPath) != w.pathLen {
			t.Errorf("call[%d].CallPath len = %d, want %d (path=%v)", i, len(calls[i].CallPath), w.pathLen, calls[i].CallPath)
			continue
		}
		if w.pathLen > 0 && calls[i].CallPath[len(calls[i].CallPath)-1] != w.pathLast {
			t.Errorf("call[%d].CallPath last = %d, want %d (path=%v)", i, calls[i].CallPath[len(calls[i].CallPath)-1], w.pathLast, calls[i].CallPath)
		}
	}
}

// TestExtractCallTrees_multipleAuthEntries — multi-user co-signed tx
// has multiple auth entries. Each entry's tree is walked
// independently; duplicates accepted at this layer (consumer dedup
// via CallPath if needed).
func TestExtractCallTrees_multipleAuthEntries(t *testing.T) {
	rootA := authNodeContract(t, 0xAA, "a_fn")
	rootB := authNodeContract(t, 0xBB, "b_fn")
	op := opInvokeNoAuth(t, 0xAA, "a_fn")
	op.Body.InvokeHostFunctionOp.Auth = []xdr.SorobanAuthorizationEntry{
		{RootInvocation: rootA},
		{RootInvocation: rootB},
	}
	got := extractInvokeContractCallTrees([]xdr.Operation{op})
	if len(got[0]) != 2 {
		t.Fatalf("got %d calls, want 2 (two distinct auth entries)", len(got[0]))
	}
	if got[0][0].FunctionName != "a_fn" || got[0][1].FunctionName != "b_fn" {
		t.Errorf("function names = [%q, %q], want [a_fn, b_fn]", got[0][0].FunctionName, got[0][1].FunctionName)
	}
}

// TestExtractCallTrees_nonInvokeContractOpNilSlot — non-InvokeContract
// ops (classic, wasm upload, create contract) yield nil slots. Parallel
// indexing with ops[] preserved.
func TestExtractCallTrees_nonInvokeContractOpNilSlot(t *testing.T) {
	classicOp := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypePayment},
	}
	invokeOp := opInvokeNoAuth(t, 0xAA, "fn")
	got := extractInvokeContractCallTrees([]xdr.Operation{classicOp, invokeOp})
	if len(got) != 2 {
		t.Fatalf("got %d slots, want 2", len(got))
	}
	if got[0] != nil {
		t.Errorf("classic op should yield nil slot, got %v", got[0])
	}
	if len(got[1]) != 1 {
		t.Errorf("invoke op slot len = %d, want 1", len(got[1]))
	}
}
