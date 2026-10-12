package soroswap_router

import (
	"encoding/base64"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestDecodeRouterArgs_swapExactTokensForTokens covers the common entry point: argument positions, i128 amount
// preservation without truncation (ADR-0003), and the address-strkey round-trip.
func TestDecodeRouterArgs_swapExactTokensForTokens(t *testing.T) {
	t.Parallel()
	// One USDC → XLM → BTC three-hop swap. Path is 3 contract
	// addresses (USDC, XLM-native-wrapper, BTC).
	usdc := makeContractAddress(t, byte(0x00))
	xlm := makeContractAddress(t, byte(0x01))
	btc := makeContractAddress(t, byte(0x02))
	to := makeAccountAddress(t, byte(0x03))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(100_000_000_000))), // amount_in: 100 USDC at e8
		mustB64(t, i128SCVal(big.NewInt(150_000_000))),     // amount_out_min: 0.0015 BTC at e8
		mustB64(t, vecSCVal(addrSCVal(usdc), addrSCVal(xlm), addrSCVal(btc))),
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(1735689600)), // 2025-01-01 deadline
	}

	swap, err := decodeRouterArgs(
		FnSwapExactTokensForTokens, args,
		MainnetRouter,
		uint32(56_000_000),
		"abc123",
		7, // op_index
		"GAOPSOURCE...", "GATXSOURCE...",
		time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		[]string{MainnetRouter}, // top-level: chain is just the router itself
	)
	if err != nil {
		t.Fatalf("decodeRouterArgs: %v", err)
	}
	if got, want := swap.Source, SourceName; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := swap.Function, FnSwapExactTokensForTokens; got != want {
		t.Errorf("Function = %q, want %q", got, want)
	}
	if got, want := swap.AmountIn.String(), "100000000000"; got != want {
		t.Errorf("AmountIn = %q, want %q", got, want)
	}
	if got, want := swap.AmountOut.String(), "150000000"; got != want {
		t.Errorf("AmountOut = %q, want %q", got, want)
	}
	if got, want := len(swap.Path), 3; got != want {
		t.Fatalf("len(Path) = %d, want %d", got, want)
	}
	if !swap.DeadlineTs.Equal(time.Unix(1735689600, 0).UTC()) {
		t.Errorf("DeadlineTs = %v, want 2025-01-01 UTC", swap.DeadlineTs)
	}
	if swap.Recipient == "" {
		t.Errorf("Recipient empty")
	}
	if got, want := swap.CallDepth, 0; got != want {
		t.Errorf("CallDepth = %d, want %d (top-level)", got, want)
	}
	if got, want := swap.CallKind, CallKindTopLevel; got != want {
		t.Errorf("CallKind = %q, want %q", got, want)
	}
	if got, want := swap.CallPath, []string{MainnetRouter}; len(got) != len(want) || got[0] != want[0] {
		t.Errorf("CallPath = %v, want %v", got, want)
	}
}

// TestDecodeRouterArgs_swapTokensForExactTokens covers the inverse: args[0] is exact-out, args[1] is in-max,
// so the per-function branch inverts (a0, a1) to (AmountIn, AmountOut).
func TestDecodeRouterArgs_swapTokensForExactTokens(t *testing.T) {
	t.Parallel()
	a := makeContractAddress(t, byte(0x10))
	b := makeContractAddress(t, byte(0x11))
	to := makeAccountAddress(t, byte(0x12))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(50_000_000))), // amount_out (exact)
		mustB64(t, i128SCVal(big.NewInt(75_000_000))), // amount_in_max
		mustB64(t, vecSCVal(addrSCVal(a), addrSCVal(b))),
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(1735689600)),
	}

	// Sub-invocation: an aggregator wrapping the router one level deep covers the CallDepth/CallKind derivation for non-top-level
	// calls. decodeRouterArgs only uses CallPath's length, so a synthetic placeholder strkey is fine.
	const aggregatorStrkey = "CAGGREGATORPLACEHOLDER0000000000000000000000000000000000000"
	swap, err := decodeRouterArgs(
		FnSwapTokensForExactTokens, args,
		MainnetRouter,
		uint32(56_000_001),
		"def456",
		3,
		"GAOPSOURCE...", "GATXSOURCE...",
		time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		[]string{aggregatorStrkey, MainnetRouter},
	)
	if err != nil {
		t.Fatalf("decodeRouterArgs: %v", err)
	}
	// Inversion: AmountIn = a1 (in-max), AmountOut = a0 (exact-out).
	if got, want := swap.AmountIn.String(), "75000000"; got != want {
		t.Errorf("AmountIn = %q, want %q (a1 = in-max)", got, want)
	}
	if got, want := swap.AmountOut.String(), "50000000"; got != want {
		t.Errorf("AmountOut = %q, want %q (a0 = exact-out)", got, want)
	}
	if got, want := swap.CallDepth, 1; got != want {
		t.Errorf("CallDepth = %d, want %d (one aggregator layer)", got, want)
	}
	if got, want := swap.CallKind, CallKindSubInvocation; got != want {
		t.Errorf("CallKind = %q, want %q", got, want)
	}
	if len(swap.CallPath) != 2 || swap.CallPath[1] != MainnetRouter {
		t.Errorf("CallPath = %v, want [<aggregator>, %s]", swap.CallPath, MainnetRouter)
	}
}

// TestDecodeRouterArgs_unknownFunction defends the dispatcher's
// pre-filter — if some other function name reaches us we return
// ErrUnknownFunction (not a panic, not silent acceptance).
func TestDecodeRouterArgs_unknownFunction(t *testing.T) {
	t.Parallel()
	_, err := decodeRouterArgs("init", nil, MainnetRouter, 0, "", 0, "", "", time.Time{}, nil)
	if !errors.Is(err, ErrUnknownFunction) {
		t.Errorf("err = %v, want ErrUnknownFunction", err)
	}
}

// TestDecodeRouterArgs_shortArgs covers the malformed-input path.
// Arity mismatch is the most common malformation (someone calls
// with a partial arg slice during contract migration).
func TestDecodeRouterArgs_shortArgs(t *testing.T) {
	t.Parallel()
	_, err := decodeRouterArgs(
		FnSwapExactTokensForTokens,
		[]string{"only", "two", "args"}, // need 5
		MainnetRouter, 0, "", 0, "", "", time.Time{}, nil,
	)
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("err = %v, want ErrMalformedArgs", err)
	}
}

// TestDecodeRouterArgs_extraArgs pins exact arity: a well-formed swap
// with a trailing sixth arg is an unaudited shape and must be refused.
func TestDecodeRouterArgs_extraArgs(t *testing.T) {
	t.Parallel()
	a := makeContractAddress(t, byte(0x50))
	b := makeContractAddress(t, byte(0x51))
	to := makeAccountAddress(t, byte(0x52))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(10))),
		mustB64(t, i128SCVal(big.NewInt(20))),
		mustB64(t, vecSCVal(addrSCVal(a), addrSCVal(b))),
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(0)),
		mustB64(t, u64SCVal(7)),
	}
	_, err := decodeRouterArgs(
		FnSwapExactTokensForTokens, args,
		MainnetRouter, 0, "", 0, "", "", time.Time{}, nil,
	)
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("err = %v, want ErrMalformedArgs (6 args)", err)
	}
}

// TestDecodeRouterArgs_pathTooShort covers the path-length invariant. The router rejects len < 2
// on-chain; the decoder mirrors that so a malformed call is dropped via the dispatcher's
// drop-counter rather than panicking on a nil-element vec access downstream.
func TestDecodeRouterArgs_pathTooShort(t *testing.T) {
	t.Parallel()
	a := makeContractAddress(t, byte(0x20))
	to := makeAccountAddress(t, byte(0x21))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(1))),
		mustB64(t, i128SCVal(big.NewInt(1))),
		mustB64(t, vecSCVal(addrSCVal(a))), // length 1 — too short
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(0)),
	}
	_, err := decodeRouterArgs(
		FnSwapExactTokensForTokens, args,
		MainnetRouter, 0, "", 0, "", "", time.Time{}, nil,
	)
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("err = %v, want ErrMalformedArgs (path too short)", err)
	}
}

// TestDecodeRouterArgs_zeroDeadlineYieldsZeroTime: a deadline of 0 ("no deadline" sentinel) leaves DeadlineTs zero,
// so the sink's IsZero() guard NULLs the column instead of stamping the 1970 epoch.
func TestDecodeRouterArgs_zeroDeadlineYieldsZeroTime(t *testing.T) {
	t.Parallel()
	a := makeContractAddress(t, byte(0x30))
	b := makeContractAddress(t, byte(0x31))
	to := makeAccountAddress(t, byte(0x32))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(10))),
		mustB64(t, i128SCVal(big.NewInt(20))),
		mustB64(t, vecSCVal(addrSCVal(a), addrSCVal(b))),
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(0)), // deadline == 0 sentinel
	}
	swap, err := decodeRouterArgs(
		FnSwapExactTokensForTokens, args,
		MainnetRouter, 0, "tx", 0, "", "",
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		[]string{MainnetRouter},
	)
	if err != nil {
		t.Fatalf("decodeRouterArgs: %v", err)
	}
	if !swap.DeadlineTs.IsZero() {
		t.Errorf("DeadlineTs = %v, want zero time (so the sink NULLs the column)", swap.DeadlineTs)
	}
}

// TestDecodeRouterArgs_overflowDeadlineYieldsZeroTime: a deadline near math.MaxUint64 ("unlimited" sentinel) wraps negative under
// a bare int64 cast and would store a plausible 1970 time, so it must be treated as absent.
func TestDecodeRouterArgs_overflowDeadlineYieldsZeroTime(t *testing.T) {
	t.Parallel()
	a := makeContractAddress(t, byte(0x40))
	b := makeContractAddress(t, byte(0x41))
	to := makeAccountAddress(t, byte(0x42))

	args := []string{
		mustB64(t, i128SCVal(big.NewInt(10))),
		mustB64(t, i128SCVal(big.NewInt(20))),
		mustB64(t, vecSCVal(addrSCVal(a), addrSCVal(b))),
		mustB64(t, addrSCVal(to)),
		mustB64(t, u64SCVal(math.MaxUint64)), // overflow-wrap sentinel
	}
	swap, err := decodeRouterArgs(
		FnSwapExactTokensForTokens, args,
		MainnetRouter, 0, "tx", 0, "", "",
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		[]string{MainnetRouter},
	)
	if err != nil {
		t.Fatalf("decodeRouterArgs: %v", err)
	}
	if !swap.DeadlineTs.IsZero() {
		t.Errorf("DeadlineTs = %v, want zero time — got a wrapped near-epoch time instead of the overflow being rejected", swap.DeadlineTs)
	}
}

// ─── SCVal builders for tests ─────────────────────────────────
// Keep simple and self-contained — duplicates similar helpers in
// other source packages but the test-time graph is small enough
// that DRYing across packages would obscure rather than help.

func i128SCVal(n *big.Int) sdkxdr.ScVal {
	abs := new(big.Int).Set(n)
	if abs.Sign() < 0 {
		abs.Neg(abs)
	}
	bytes := abs.Bytes()
	for len(bytes) < 16 {
		bytes = append([]byte{0}, bytes...)
	}
	hi := int64(0)
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | int64(bytes[i])
	}
	lo := uint64(0)
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(bytes[i])
	}
	if n.Sign() < 0 {
		// two's complement
		hi = ^hi
		lo = ^lo + 1
		if lo == 0 {
			hi++
		}
	}
	return sdkxdr.ScVal{
		Type: sdkxdr.ScValTypeScvI128,
		I128: &sdkxdr.Int128Parts{
			Hi: sdkxdr.Int64(hi),
			Lo: sdkxdr.Uint64(lo),
		},
	}
}

func u64SCVal(n uint64) sdkxdr.ScVal {
	v := sdkxdr.Uint64(n)
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvU64, U64: &v}
}

func vecSCVal(elems ...sdkxdr.ScVal) sdkxdr.ScVal {
	v := sdkxdr.ScVec(elems)
	pv := &v
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvVec, Vec: &pv}
}

func addrSCVal(addr sdkxdr.ScAddress) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
}

func makeContractAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var hash sdkxdr.Hash
	for i := range hash {
		hash[i] = fillByte
	}
	cid := sdkxdr.ContractId(hash)
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

func makeAccountAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var ed25519 sdkxdr.Uint256
	for i := range ed25519 {
		ed25519[i] = fillByte
	}
	acct := sdkxdr.AccountId{
		Type:    sdkxdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &ed25519,
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
}

func mustB64(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	bs, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bs)
}

// Two identical router calls authorized side by side in ONE auth entry are two executions and both must survive the served PK;
// the same call repeated in a SECOND entry is a co-signing duplicate and must still collapse.

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

// TestCallSig guards the PK discriminator: CallSig is deterministic, ignores deadline (auth-tree dups dedup),
// and differs for any economically distinct swap (recipient / path / amount), else sub-op swaps are lost or dups double-counted.
func TestCallSig(t *testing.T) {
	t.Parallel()
	base := RouterSwap{
		Function:   FnSwapExactTokensForTokens,
		Recipient:  "GRECIPIENT",
		Path:       []string{"CTOKENA", "CTOKENB"},
		AmountIn:   canonical.NewAmount(big.NewInt(1_000_000)),
		AmountOut:  canonical.NewAmount(big.NewInt(2_000_000)),
		DeadlineTs: time.Unix(1_700_000_000, 0).UTC(),
	}

	// Deterministic.
	if base.CallSig() != base.CallSig() {
		t.Fatal("CallSig is not deterministic")
	}

	// Deadline must NOT affect the sig (it's an excluded user sentinel) — and
	// fields that don't enter the identity (ledger/tx/op/sources) must not either.
	dupDiffDeadline := base
	dupDiffDeadline.DeadlineTs = time.Unix(3_100_000_000_000_000_000, 0).UTC()
	dupDiffDeadline.Ledger = 999
	dupDiffDeadline.TxHash = "differenttx"
	dupDiffDeadline.OpIndex = 7
	if dupDiffDeadline.CallSig() != base.CallSig() {
		t.Error("CallSig must be invariant to deadline / non-identity fields (would break auth-tree dedup)")
	}

	// Every economic dimension must change the sig.
	diffRecipient := base
	diffRecipient.Recipient = "GOTHER"
	diffPath := base
	diffPath.Path = []string{"CTOKENA", "CTOKENC"}
	diffAmountIn := base
	diffAmountIn.AmountIn = canonical.NewAmount(big.NewInt(1_000_001))
	diffAmountOut := base
	diffAmountOut.AmountOut = canonical.NewAmount(big.NewInt(2_000_001))
	diffFn := base
	diffFn.Function = FnSwapTokensForExactTokens

	for name, v := range map[string]RouterSwap{
		"recipient": diffRecipient,
		"path":      diffPath,
		"amountIn":  diffAmountIn,
		"amountOut": diffAmountOut,
		"function":  diffFn,
	} {
		if v.CallSig() == base.CallSig() {
			t.Errorf("CallSig collision: distinct %s produced the same sig as base (sub-op swap would be lost)", name)
		}
	}

	// Sanity: 16-byte hash → 32 hex chars.
	if len(base.CallSig()) != 32 {
		t.Errorf("CallSig len = %d, want 32 hex chars", len(base.CallSig()))
	}
}

// TestCallSig_AuthOccurrence pins that occurrence 0 hashes exactly as stored
// rows were keyed (a changed sig would re-insert every row beside its old PK)
// and that each later occurrence gets its own sig.
func TestCallSig_AuthOccurrence(t *testing.T) {
	t.Parallel()
	base := RouterSwap{
		Function:  FnSwapExactTokensForTokens,
		Recipient: "GRECIPIENT",
		Path:      []string{"CTOKENA", "CTOKENB"},
		AmountIn:  canonical.NewAmount(big.NewInt(1_000_000)),
		AmountOut: canonical.NewAmount(big.NewInt(2_000_000)),
	}
	if got, want := base.CallSig(), "1e3e6d4d561957e6db1756237ec46e8e"; got != want {
		t.Fatalf("occurrence-0 CallSig = %q, want the stored %q", got, want)
	}
	seen := map[string]int{base.CallSig(): 0}
	for n := 1; n <= 3; n++ {
		v := base
		v.AuthOccurrence = n
		if prev, dup := seen[v.CallSig()]; dup {
			t.Errorf("occurrence %d shares CallSig with occurrence %d", n, prev)
		}
		seen[v.CallSig()] = n
	}
}
