package ingest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange is the
// wiring guard: the helper above is only worth anything if the replay
// command actually calls it and then refreshes the aggregates. A replay
// re-projects trades into a historical range, and the CAGG refresh
// policies only roll forward over their own start_offset window (five
// minutes for prices_1m), so nothing else will ever materialize those
// buckets — the rows land in the hypertable and no served read can
// reach them.
//
// The calls live one hop down, in rematerializeReplayedRange (split out
// to keep projectorReplay under the cyclomatic limit), so BOTH hops are
// pinned: a helper that refreshes perfectly and is never reached from
// the command is the same defect as no helper at all.
//
// Read from the AST rather than from a list so the assertion cannot
// drift from the code it describes.
func TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projector.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projector.go: %v", err)
	}
	const why = "a replay rewrites trades in a historical range, and the continuous aggregates over them only roll forward, so the re-projected rows stay invisible to /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception"
	hops := []struct {
		fn    string
		wants []string
	}{
		{"projectorReplay", []string{"rematerializeReplayedRange"}},
		{"rematerializeReplayedRange", []string{"awaitProjectorCursor", "refreshCAGGsForChunk"}},
	}
	for _, hop := range hops {
		called, ok := callsInFunc(file, hop.fn)
		if !ok {
			t.Fatalf("%s is gone from projector.go — this guard has moved", hop.fn)
		}
		for _, want := range hop.wants {
			if !called[want] {
				t.Errorf("%s never calls %s — %s", hop.fn, want, why)
			}
		}
	}
}

// callsInFunc returns the set of plain-identifier callees inside the
// named top-level function, and whether that function exists.
func callsInFunc(file *ast.File, name string) (map[string]bool, bool) {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		called := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
			}
			return true
		})
		return called, true
	}
	return nil, false
}
