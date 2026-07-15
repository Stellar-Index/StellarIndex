package soroswap_router

import (
	"os"
	"strings"
	"testing"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Real-bytes golden tests (ROADMAP #11). Both fixtures are UNMODIFIED
// InvokeHostFunction operation bodies (base64 XDR) captured from the
// certified ClickHouse lake (stellar.operations.body_xdr) on r1,
// 2026-07-10:
//
//   - router_toplevel_op_ledger62000296.b64
//     tx 642bb2de2703c289f945b493714de0603170c3e5f48d1ef8c1aca96587fb98a6
//     op 0, ledger 62,000,296 (closed 2026-04-06 21:02:24 UTC).
//     A DIRECT router call: the op's own InvokeContract target is the
//     router. Auth tree = router root + one nested token transfer.
//
//   - router_subinvocation_op_ledger62029020.b64
//     tx da2ffe5a8651e2289408631a180cc8f6fe26247c6558bfc80dbf6d9527849cc7
//     op 0, ledger 62,029,020 (closed 2026-04-08 18:35:46 UTC).
//     The headline #11 shape: an AGGREGATOR (`exec` on CD45PQFH…JRZH)
//     wraps an adapter (`swap_exact_tokens_for_tokens` on
//     CAYP3UWL…TXTO) which wraps the ROUTER two levels deep. The
//     pre-#48 top-level-only walk never saw this call — the 8,729×
//     undercount class.
//
// The tests drive the exact production path: dispatcher call-tree
// extraction (ExtractContractCallTree — byte-identical to what
// ProcessLedger feeds ContractCallDecoders) → Decoder.Matches →
// Decoder.Decode, then assert the decoded RouterSwap field-by-field
// including the new CallPath / CallDepth / CallKind columns.

// loadRealOp reads a testdata base64 op body and returns the op.
func loadRealOp(t *testing.T, name string) sdkxdr.Operation {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var body sdkxdr.OperationBody
	if err := sdkxdr.SafeUnmarshalBase64(strings.TrimSpace(string(raw)), &body); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return sdkxdr.Operation{Body: body}
}

// decodeRouterCallsFromOp runs the production seam over one op:
// extract the auth-tree call list, route through Matches, decode each
// matching call with a ContractCallContext carrying the tree position
// (exactly as Dispatcher.ProcessLedger and chops.forEachContractCallEvent
// both do).
func decodeRouterCallsFromOp(t *testing.T, op sdkxdr.Operation, ledger uint32, txHash string) []RouterSwap {
	t.Helper()
	dec := NewDecoder(MainnetRouter)
	var out []RouterSwap
	for _, call := range dispatcher.ExtractContractCallTree(op) {
		if !dec.Matches(call.ContractID, call.FunctionName) {
			continue
		}
		evs, err := dec.Decode(dispatcher.ContractCallContext{
			Ledger:            ledger,
			ClosedAt:          time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC),
			TxHash:            txHash,
			TxSource:          "GTESTTXSOURCE",
			OpSource:          "GTESTOPSOURCE",
			OpIndex:           0,
			ContractID:        call.ContractID,
			FunctionName:      call.FunctionName,
			Args:              call.Args,
			CallPath:          call.CallPath,
			CallPathContracts: call.CallPathContracts,
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		for _, ev := range evs {
			re, ok := ev.(Event)
			if !ok {
				t.Fatalf("Decode emitted %T, want soroswap_router.Event", ev)
			}
			out = append(out, re.Swap)
		}
	}
	return out
}

// TestRealBytes_TopLevelRouterCall — direct router invocation.
func TestRealBytes_TopLevelRouterCall(t *testing.T) {
	t.Parallel()
	op := loadRealOp(t, "router_toplevel_op_ledger62000296.b64")
	swaps := decodeRouterCallsFromOp(t, op, 62_000_296,
		"642bb2de2703c289f945b493714de0603170c3e5f48d1ef8c1aca96587fb98a6")
	if len(swaps) != 1 {
		t.Fatalf("got %d router swaps, want 1", len(swaps))
	}
	s := swaps[0]
	if s.Function != FnSwapExactTokensForTokens {
		t.Errorf("Function = %q, want %q", s.Function, FnSwapExactTokensForTokens)
	}
	if s.ContractID != MainnetRouter {
		t.Errorf("ContractID = %q, want the mainnet router", s.ContractID)
	}
	if want := "GB3JCHJUP6HHZJLN5LKQDRFP2HWSLNXYE2TGWDGBTNXIW6MLVRQXNDBC"; s.Recipient != want {
		t.Errorf("Recipient = %q, want %q", s.Recipient, want)
	}
	wantPath := []string{
		"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		"CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV",
	}
	if len(s.Path) != len(wantPath) || s.Path[0] != wantPath[0] || s.Path[1] != wantPath[1] {
		t.Errorf("Path = %v, want %v", s.Path, wantPath)
	}
	if got, want := s.AmountIn.String(), "710000000"; got != want {
		t.Errorf("AmountIn = %q, want %q", got, want)
	}
	if got, want := s.AmountOut.String(), "95014502"; got != want {
		t.Errorf("AmountOut = %q, want %q (amount_out_min lower bound)", got, want)
	}
	// This real call carries a garbage-far-future deadline (year
	// 58233) — the class the sink's pgTimestamptzRepresentable guard
	// exists for. It IS representable in Go time; assert the decode
	// preserved it rather than clamping (clamping is the SINK's job,
	// NULL at insert, and only for values outside the PG domain).
	if s.DeadlineTs.Year() != 58233 {
		t.Errorf("DeadlineTs year = %d, want 58233 (garbage sentinel preserved at decode layer)", s.DeadlineTs.Year())
	}
	// #11 columns: direct call → depth 0, top_level, chain = [router].
	if s.CallDepth != 0 {
		t.Errorf("CallDepth = %d, want 0", s.CallDepth)
	}
	if s.CallKind != CallKindTopLevel {
		t.Errorf("CallKind = %q, want %q", s.CallKind, CallKindTopLevel)
	}
	if len(s.CallPath) != 1 || s.CallPath[0] != MainnetRouter {
		t.Errorf("CallPath = %v, want [%s]", s.CallPath, MainnetRouter)
	}
}

// TestRealBytes_SubInvocationRouterCall — aggregator-wrapped router
// invocation two levels deep. THE shape the pre-#48 walk missed.
func TestRealBytes_SubInvocationRouterCall(t *testing.T) {
	t.Parallel()
	op := loadRealOp(t, "router_subinvocation_op_ledger62029020.b64")
	swaps := decodeRouterCallsFromOp(t, op, 62_029_020,
		"da2ffe5a8651e2289408631a180cc8f6fe26247c6558bfc80dbf6d9527849cc7")
	if len(swaps) != 1 {
		t.Fatalf("got %d router swaps, want exactly 1 (the tree has 7 calls; only one is the router)", len(swaps))
	}
	s := swaps[0]
	if s.Function != FnSwapExactTokensForTokens {
		t.Errorf("Function = %q, want %q", s.Function, FnSwapExactTokensForTokens)
	}
	if want := "CAHA3HQFB73R7Z3D3AGOVINRKHCLPBJQWPOXM5I7LSFBF62OOSCDKV24"; s.Recipient != want {
		t.Errorf("Recipient = %q, want %q (a CONTRACT recipient — the aggregator adapter, not an end user)", s.Recipient, want)
	}
	wantPath := []string{
		"CD3X4GOWBPDU57NIPMPEMH7LFNAMBDTY5SKJCHLY7IDDWJQVUTU7CBBK",
		"CCG27OZ5AV4WUXS6XTECWAXEY5UOMEFI2CWFA3LHZGBTLYZWTJF3MJYQ",
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
	}
	if len(s.Path) != len(wantPath) {
		t.Fatalf("Path = %v, want %v (3-hop)", s.Path, wantPath)
	}
	for i := range wantPath {
		if s.Path[i] != wantPath[i] {
			t.Errorf("Path[%d] = %q, want %q", i, s.Path[i], wantPath[i])
		}
	}
	if got, want := s.AmountIn.String(), "17839213"; got != want {
		t.Errorf("AmountIn = %q, want %q", got, want)
	}
	// amount_out_min == 0: the wrapping aggregator does its own
	// slippage enforcement, so it passes 0 down to the router. Real
	// production shape — another reason router rows must never price.
	if got, want := s.AmountOut.String(), "0"; got != want {
		t.Errorf("AmountOut = %q, want %q", got, want)
	}
	if !s.DeadlineTs.Equal(time.Date(2026, 4, 8, 18, 36, 5, 0, time.UTC)) {
		t.Errorf("DeadlineTs = %v, want 2026-04-08T18:36:05Z", s.DeadlineTs)
	}
	// #11 columns: two wrapping layers → depth 2, sub_invocation,
	// chain = [aggregator, adapter, router].
	wantChain := []string{
		"CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH", // aggregator `exec`
		"CAYP3UWLJM7ZPTUKL6R6BFGTRWLZ46LRKOXTERI2K6BIJAWGYY62TXTO", // adapter `swap_exact_tokens_for_tokens`
		MainnetRouter,
	}
	if len(s.CallPath) != len(wantChain) {
		t.Fatalf("CallPath = %v, want %v", s.CallPath, wantChain)
	}
	for i := range wantChain {
		if s.CallPath[i] != wantChain[i] {
			t.Errorf("CallPath[%d] = %q, want %q", i, s.CallPath[i], wantChain[i])
		}
	}
	if s.CallDepth != 2 {
		t.Errorf("CallDepth = %d, want 2", s.CallDepth)
	}
	if s.CallKind != CallKindSubInvocation {
		t.Errorf("CallKind = %q, want %q", s.CallKind, CallKindSubInvocation)
	}
}

// TestRealBytes_CallSigIgnoresTreePosition — the same economic call
// observed at two different tree positions (a co-signed multi-entry
// tx surfaces duplicates) must produce IDENTICAL CallSigs so the
// served PK's ON CONFLICT dedups them. Guards the migration-0056
// dedup contract against a future "add CallPath to the hash" change.
func TestRealBytes_CallSigIgnoresTreePosition(t *testing.T) {
	t.Parallel()
	op := loadRealOp(t, "router_subinvocation_op_ledger62029020.b64")
	swaps := decodeRouterCallsFromOp(t, op, 62_029_020, "da2ffe5a")
	if len(swaps) != 1 {
		t.Fatalf("got %d router swaps, want 1", len(swaps))
	}
	deep := swaps[0]
	shallow := deep
	shallow.CallPath = []string{MainnetRouter}
	shallow.CallDepth = 0
	shallow.CallKind = CallKindTopLevel
	if deep.CallSig() != shallow.CallSig() {
		t.Errorf("CallSig differs across tree positions: %q vs %q — auth-tree duplicate dedup broken",
			deep.CallSig(), shallow.CallSig())
	}
}

// TestRealBytes_EventKindStable — the pipeline sink type-switches on
// the concrete Event type; EventKind is the diagnostic label. Pin it.
func TestRealBytes_EventKindStable(t *testing.T) {
	t.Parallel()
	var ev consumer.Event = Event{}
	if ev.EventKind() != "soroswap-router.swap" {
		t.Errorf("EventKind = %q, want soroswap-router.swap", ev.EventKind())
	}
}
