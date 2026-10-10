package chops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// panickingCallDecoder claims every call and panics on one function name in
// Decode, or in Matches when panicInMatches is set — the shape of a decoder
// bug the live dispatcher recovers per input.
type panickingCallDecoder struct {
	badFunc        string
	panicInMatches bool
}

func (panickingCallDecoder) Name() string { return "panicky" }
func (d panickingCallDecoder) Matches(_, fn string) bool {
	if d.panicInMatches && fn == d.badFunc {
		var m map[string]int
		m["boom"]++ // nil-map write: a real runtime panic, not a sentinel
	}
	return true
}

func (d panickingCallDecoder) Decode(cc dispatcher.ContractCallContext) ([]consumer.Event, error) {
	if !d.panicInMatches && cc.FunctionName == d.badFunc {
		var s []int
		_ = s[3] // index out of range
	}
	return []consumer.Event{stubCallEvent{}}, nil
}

// TestDecodeContractCallTree_PanicIsBlindNotCrash pins, for the
// ContractCall census (band, soroswap-router): a panicking Matches or Decode
// must be recorded as a blind spot on its ledger and the rest of the tree
// still decoded, not unwind through compute-completeness.
func TestDecodeContractCallTree_PanicIsBlindNotCrash(t *testing.T) {
	const ledger uint32 = 52_000_001
	op := clickhouse.ContractCallOp{Ledger: ledger, TxHash: "bb", Source: "GSOURCE"}
	calls := []dispatcher.ContractCall{
		{ContractID: "CAAA", FunctionName: "swap"},
		{ContractID: "CAAA", FunctionName: "relay"},
		{ContractID: "CAAA", FunctionName: "swap"},
	}
	for _, inMatches := range []bool{false, true} {
		blind := completeness.NewBlindTracker()
		var emitted int
		err := decodeContractCallTree(op, calls, panickingCallDecoder{badFunc: "relay", panicInMatches: inMatches}, blind,
			func(uint32, consumer.Event) error { emitted++; return nil })
		if err != nil {
			t.Fatalf("panicInMatches=%v: decodeContractCallTree: %v", inMatches, err)
		}
		if emitted != 2 {
			t.Errorf("panicInMatches=%v: emitted = %d, want 2 (the two healthy calls)", inMatches, emitted)
		}
		got := blind.Result()
		if got.UndecodableMatched != 1 || len(got.Ledgers) != 1 || got.Ledgers[0] != ledger {
			t.Errorf("panicInMatches=%v: blind = %+v, want 1 undecodable on ledger %d", inMatches, got, ledger)
		}
	}
}

// panickingGatedDecoder is the mock gated decoder whose creation-event
// Decode panics for one announced child.
type panickingGatedDecoder struct {
	*mockGatedDecoder
	badChild string
}

func (d panickingGatedDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if mockTopic0(ev) == "create" && ev.Value == d.badChild {
		panic("unexpected creation-event shape")
	}
	return d.mockGatedDecoder.Decode(ev)
}

// completenessWriterFiles are chops files that decode on a WRITE path
// (ch-rebuild / ch-reproject / backfills), not a completeness oracle; their
// panic policy is not the audit's and is out of this guard's scope.
var completenessWriterFiles = map[string]bool{
	"ch_rebuild.go":                 true,
	"ch_reproject.go":               true,
	"classic_movements_backfill.go": true,
	"ch_cap67_movements.go":         true,
	"projected_rebuild.go":          true,
}

// TestChopsDecoderCallsAreGuarded is the class guard: outside the
// named writer files, every decoder Matches / Decode / DecodeCounted call in
// internal/ops/chops must run inside a completeness.Guard closure, so a new
// oracle loop cannot reintroduce an unguarded decoder on the audit path.
func TestChopsDecoderCallsAreGuarded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || completenessWriterFiles[f] {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checkGuardedDecoderCalls(t, fset, file)
	}
}

func checkGuardedDecoderCalls(t *testing.T, fset *token.FileSet, file *ast.File) {
	t.Helper()
	pkgs := map[string]bool{}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pkgs[name] = true
	}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Matches" && sel.Sel.Name != "Decode" && sel.Sel.Name != "DecodeCounted") {
			return true
		}
		if id, isIdent := sel.X.(*ast.Ident); isIdent && pkgs[id.Name] {
			return true // package function (strkey.Decode), not a decoder method
		}
		if _, isCall := sel.X.(*ast.CallExpr); isCall {
			return true // json.NewDecoder(r).Decode — a stream reader, not a decoder
		}
		if !insideGuard(stack) {
			t.Errorf("%s: decoder %s call outside completeness.Guard", fset.Position(call.Pos()), sel.Sel.Name)
		}
		return true
	})
}

func insideGuard(stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		if _, ok := stack[i].(*ast.FuncLit); !ok {
			continue
		}
		c, ok := stack[i-1].(*ast.CallExpr)
		if !ok {
			continue
		}
		if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Guard" {
			if id, ok := s.X.(*ast.Ident); ok && id.Name == "completeness" {
				return true
			}
		}
	}
	return false
}

// The ContractCall census and its writer share decodeContractCallTree and both
// soft-fail per call, so a malformed call nets to zero in the diff; only the
// blind tracker can surface it. A clean tree must stay silent.
func TestDecodeContractCallTree_BlindTracksMalformedCalls(t *testing.T) {
	const badLedger uint32 = 51_000_123
	op := clickhouse.ContractCallOp{Ledger: badLedger, TxHash: "aa", Source: "GSOURCE", OpIndex: 0}
	calls := []dispatcher.ContractCall{
		{ContractID: "CAAA", FunctionName: "swap"},
		{ContractID: "CAAA", FunctionName: "relay"},
		{ContractID: "CAAA", FunctionName: "swap"},
	}

	blind := completeness.NewBlindTracker()
	var emitted int
	if err := decodeContractCallTree(op, calls, stubContractCallDecoder{badFunc: "relay"}, blind,
		func(uint32, consumer.Event) error { emitted++; return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2 (the malformed call is skipped by BOTH sides)", emitted)
	}
	got := blind.Result()
	if !got.Any() {
		t.Fatal("the malformed call was skipped silently; projection_ok would be certified on a ledger with a dropped row")
	}
	if got.UndecodableMatched != 1 {
		t.Errorf("UndecodableMatched = %d, want 1", got.UndecodableMatched)
	}
	if len(got.Ledgers) != 1 || got.Ledgers[0] != badLedger {
		t.Errorf("Ledgers = %v, want [%d]", got.Ledgers, badLedger)
	}

	clean := completeness.NewBlindTracker()
	if err := decodeContractCallTree(clickhouse.ContractCallOp{Ledger: 42}, calls[:1], stubContractCallDecoder{badFunc: "none"}, clean,
		func(uint32, consumer.Event) error { return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}
	if clean.Result().Any() {
		t.Errorf("a clean call tree reported blind spots: %+v", clean.Result())
	}
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
