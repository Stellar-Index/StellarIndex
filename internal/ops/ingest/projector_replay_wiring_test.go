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
// Read from the AST rather than from a list so the assertion cannot
// drift from the code it describes.
func TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projector.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projector.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "projectorReplay" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("projectorReplay is gone from projector.go — this guard has moved")
	}
	called := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			called[id.Name] = true
		}
		return true
	})
	for _, want := range []string{"awaitProjectorCursor", "refreshCAGGsForChunk"} {
		if !called[want] {
			t.Errorf("projectorReplay never calls %s — a replay rewrites trades in a historical range, and the continuous aggregates over them only roll forward, so the re-projected rows stay invisible to /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception", want)
		}
	}
}
