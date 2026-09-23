package soroswap_router

import (
	"encoding/base64"
	"errors"
	"math"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// refI128 is the i128 reference value hi·2^64 + lo computed in big.Int,
// independent of canonical.FromInt128Parts (ADR-0003).
func refI128(hi int64, lo uint64) *big.Int {
	r := big.NewInt(hi)
	r.Lsh(r, 64)
	return r.Add(r, new(big.Int).SetUint64(lo))
}

func encodeB64(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	bs, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bs)
}

func rawI128SCVal(hi int64, lo uint64) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvI128, I128: &sdkxdr.Int128Parts{Hi: sdkxdr.Int64(hi), Lo: sdkxdr.Uint64(lo)}}
}

// realRouterArgs returns the five router-call args of every swap call in
// the captured mainnet ops under testdata/.
func realRouterArgs(f *testing.F) [][]string {
	f.Helper()
	var out [][]string
	for _, name := range []string{"router_toplevel_op_ledger62000296.b64", "router_subinvocation_op_ledger62029020.b64"} {
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		var body sdkxdr.OperationBody
		if err := sdkxdr.SafeUnmarshalBase64(strings.TrimSpace(string(raw)), &body); err != nil {
			f.Fatal(err)
		}
		for _, call := range dispatcher.ExtractContractCallTree(sdkxdr.Operation{Body: body}) {
			if NewDecoder(MainnetRouter).Matches(call.ContractID, call.FunctionName) && len(call.Args) == 5 {
				out = append(out, append([]string{call.FunctionName}, call.Args...))
			}
		}
	}
	if len(out) == 0 {
		f.Fatal("no router swap calls in testdata")
	}
	return out
}

// FuzzDecodeRouterArgs builds well-formed router calls from fuzzed parts,
// decodes them through Decoder.Decode (the dispatcher's entry point) and
// checks every emitted field against the inputs: amounts exact to
// 128 bits and routed by function (exact-in: args[0] is in; exact-out:
// args[0] is out), path and recipient strkeys verbatim, the deadline only
// where it fits int64 seconds, and call depth from the call path.
func FuzzDecodeRouterArgs(f *testing.F) {
	for _, a := range realRouterArgs(f) {
		var s0, s1 sdkxdr.ScVal
		if err := sdkxdr.SafeUnmarshalBase64(a[1], &s0); err != nil || s0.I128 == nil {
			f.Fatalf("seed args[0]: %v", err)
		}
		if err := sdkxdr.SafeUnmarshalBase64(a[2], &s1); err != nil || s1.I128 == nil {
			f.Fatalf("seed args[1]: %v", err)
		}
		fn := uint8(0)
		if a[0] == FnSwapTokensForExactTokens {
			fn = 1
		}
		f.Add(fn, int64(s0.I128.Hi), uint64(s0.I128.Lo), int64(s1.I128.Hi), uint64(s1.I128.Lo), uint8(3), uint8(7), uint64(1735689600), uint8(1))
	}
	f.Add(uint8(1), int64(-1), uint64(1), int64(1<<62), ^uint64(0), uint8(2), uint8(0), uint64(math.MaxInt64)+1, uint8(3))
	f.Add(uint8(0), int64(0), uint64(0), int64(0), uint64(0), uint8(1), uint8(9), uint64(0), uint8(0))
	f.Fuzz(func(t *testing.T, fnSel uint8, h0 int64, l0 uint64, h1 int64, l1 uint64, pathLen, seed uint8, deadline uint64, callPathLen uint8) {
		fn := []string{FnSwapExactTokensForTokens, FnSwapTokensForExactTokens}[fnSel&1]
		n := int(pathLen % 6)
		pathSv := make([]sdkxdr.ScVal, n)
		wantPath := make([]string, n)
		for i := range pathSv {
			cid := sdkxdr.ContractId{}
			for j := range cid {
				cid[j] = seed + byte(i*31+j)
			}
			a := sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
			pathSv[i] = addrSCVal(a)
			s, err := strkey.Encode(strkey.VersionByteContract, cid[:])
			if err != nil {
				t.Fatal(err)
			}
			wantPath[i] = s
		}
		to := makeAccountAddress(t, seed^0x5a)
		wantTo, err := strkey.Encode(strkey.VersionByteAccountID, to.AccountId.Ed25519[:])
		if err != nil {
			t.Fatal(err)
		}
		args := []string{
			encodeB64(t, rawI128SCVal(h0, l0)),
			encodeB64(t, rawI128SCVal(h1, l1)),
			encodeB64(t, vecSCVal(pathSv...)),
			encodeB64(t, addrSCVal(to)),
			encodeB64(t, u64SCVal(deadline)),
		}
		callPath := make([]string, int(callPathLen%5))
		for i := range callPath {
			callPath[i] = "C" + string(rune('A'+i))
		}
		closed := time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC)

		opIndex := int(seed) + 1
		evs, err := NewDecoder(MainnetRouter).Decode(dispatcher.ContractCallContext{
			Ledger: 62000296, ClosedAt: closed, TxHash: "tx", TxSource: "GTX", OpSource: "GOP", OpIndex: opIndex,
			ContractID: MainnetRouter, FunctionName: fn, Args: args, CallPathContracts: callPath,
		})
		if n < 2 {
			if !errors.Is(err, ErrMalformedArgs) {
				t.Fatalf("path len %d: err = %v, want ErrMalformedArgs", n, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("Decode emitted %d events, want 1", len(evs))
		}
		ev, ok := evs[0].(Event)
		if !ok {
			t.Fatalf("Decode emitted %T", evs[0])
		}
		swap := ev.Swap
		in, out := refI128(h0, l0), refI128(h1, l1)
		if fn == FnSwapTokensForExactTokens {
			in, out = out, in
		}
		if swap.AmountIn.BigInt().Cmp(in) != 0 || swap.AmountOut.BigInt().Cmp(out) != 0 {
			t.Fatalf("%s: in/out = %s/%s, want %s/%s", fn, swap.AmountIn, swap.AmountOut, in, out)
		}
		if strings.Join(swap.Path, ",") != strings.Join(wantPath, ",") || swap.Recipient != wantTo {
			t.Fatalf("path/recipient = %v/%s, want %v/%s", swap.Path, swap.Recipient, wantPath, wantTo)
		}
		switch {
		case deadline == 0 || deadline > math.MaxInt64:
			if !swap.DeadlineTs.IsZero() {
				t.Fatalf("deadline %d: DeadlineTs = %v, want zero", deadline, swap.DeadlineTs)
			}
		case swap.DeadlineTs.Unix() != int64(deadline) || swap.DeadlineTs.Location() != time.UTC:
			t.Fatalf("deadline %d: DeadlineTs = %v", deadline, swap.DeadlineTs)
		}
		wantDepth, wantKind := 0, CallKindTopLevel
		if len(callPath) > 1 {
			wantDepth, wantKind = len(callPath)-1, CallKindSubInvocation
		}
		if swap.CallDepth != wantDepth || swap.CallKind != wantKind {
			t.Fatalf("callPath len %d: depth/kind = %d/%s, want %d/%s", len(callPath), swap.CallDepth, swap.CallKind, wantDepth, wantKind)
		}
		if swap.Function != fn || swap.Source != SourceName || swap.ContractID != MainnetRouter || swap.OpIndex != opIndex ||
			swap.Ledger != 62000296 || swap.TxHash != "tx" || swap.TxSource != "GTX" || swap.OpSource != "GOP" || !swap.ClosedAt.Equal(closed) {
			t.Fatalf("identity fields = %+v", swap)
		}
	})
}

// FuzzDecodeRouterArgsRaw feeds arbitrary arg strings: no panic, every
// refusal is one of the two sentinel errors, and an accepted call's
// amounts are exactly the i128s an independent XDR read finds in args[0..1].
func FuzzDecodeRouterArgsRaw(f *testing.F) {
	for _, a := range realRouterArgs(f) {
		f.Add(a[0], a[1], a[2], a[3], a[4], a[5])
	}
	f.Add("set_pair_fee", "", "", "", "", "")
	f.Fuzz(func(t *testing.T, fn, a0, a1, a2, a3, a4 string) {
		swap, err := decodeRouterArgs(fn, []string{a0, a1, a2, a3, a4}, MainnetRouter, 1, "tx", 0, "", "", time.Time{}, nil)
		if err != nil {
			if !errors.Is(err, ErrMalformedArgs) && !errors.Is(err, ErrUnknownFunction) {
				t.Fatalf("unclassified refusal: %v", err)
			}
			return
		}
		if fn != FnSwapExactTokensForTokens && fn != FnSwapTokensForExactTokens {
			t.Fatalf("accepted function %q", fn)
		}
		read := func(s string) *big.Int {
			var sv sdkxdr.ScVal
			if err := sdkxdr.SafeUnmarshalBase64(s, &sv); err != nil || sv.Type != sdkxdr.ScValTypeScvI128 {
				t.Fatalf("accepted non-i128 amount %q", s)
			}
			return refI128(int64(sv.I128.Hi), uint64(sv.I128.Lo))
		}
		in, out := read(a0), read(a1)
		if fn == FnSwapTokensForExactTokens {
			in, out = out, in
		}
		if swap.AmountIn.BigInt().Cmp(in) != 0 || swap.AmountOut.BigInt().Cmp(out) != 0 {
			t.Fatalf("in/out = %s/%s, want %s/%s", swap.AmountIn, swap.AmountOut, in, out)
		}
		if len(swap.Path) < 2 {
			t.Fatalf("accepted path %v", swap.Path)
		}
	})
}

// FuzzCallSig: the sig is a pure function of (function, recipient, path,
// amounts) — equal economic content always collides (auth-tree dedup) and
// changing any one economic field never does.
func FuzzCallSig(f *testing.F) {
	f.Add("GRECIPIENT", "CTOKENA", "CTOKENB", int64(1_000_000), int64(2_000_000), uint8(0), int64(1_700_000_000))
	f.Add("G", "C", "C", int64(-1), int64(0), uint8(3), int64(0))
	f.Fuzz(func(t *testing.T, recipient, p0, p1 string, in, out int64, which uint8, deadline int64) {
		base := RouterSwap{
			Function:  FnSwapExactTokensForTokens,
			Recipient: recipient,
			Path:      []string{p0, p1},
			AmountIn:  canonical.NewAmount(big.NewInt(in)),
			AmountOut: canonical.NewAmount(big.NewInt(out)),
		}
		sig := base.CallSig()
		if len(sig) != 32 {
			t.Fatalf("len(sig) = %d", len(sig))
		}
		dup := base
		dup.Path = append([]string(nil), base.Path...)
		dup.DeadlineTs = time.Unix(deadline, 0)
		dup.CallPath, dup.CallDepth, dup.CallKind = []string{"CX", "CY"}, 1, CallKindSubInvocation
		dup.Ledger, dup.TxHash, dup.OpIndex = 9, "other", 4
		if dup.CallSig() != sig {
			t.Fatal("sig depends on a non-economic field")
		}
		changed := base
		changed.Path = append([]string(nil), base.Path...)
		switch which % 6 {
		case 0:
			changed.Function = FnSwapTokensForExactTokens
		case 1:
			changed.Recipient += "X"
		case 2:
			changed.Path[1] += "X"
		case 3:
			changed.AmountIn = canonical.NewAmount(new(big.Int).Add(big.NewInt(in), big.NewInt(1)))
		case 4:
			changed.AmountOut = canonical.NewAmount(new(big.Int).Sub(big.NewInt(out), big.NewInt(1)))
		case 5:
			changed.Path = append(changed.Path, p1)
		}
		if changed.CallSig() == sig {
			t.Fatalf("economic change %d collided", which%6)
		}
	})
}
