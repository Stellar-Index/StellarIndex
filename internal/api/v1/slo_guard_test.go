package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSLORoutesNeverTouchTheLake pins the multi-region plan's §3a invariant
// (ADR-0050): no SLO'd route may depend on the ClickHouse lake — or, in the
// future, any cross-region proxy or S3 fallback. The p95≤200ms/p99≤500ms SLO
// (ADR-0009, enforced by configs/prometheus/rules.r1/slo.yml over exactly
// these routes) holds precisely BECAUSE the pricing path is always local
// Redis+Timescale; a change that wires a lake reader into one of these
// handlers would put a multi-second-budget dependency onto a 200ms-budget
// route and, under the multi-region design, a WAN hop onto the money path.
//
// Mechanism: a direct-body AST tripwire — every SLO'd handler's body is
// walked for selector reads of the Server's ClickHouse-backed fields. This
// intentionally checks the handler bodies, not the transitive call graph
// (the realistic regression is wiring `s.explorer`/`s.tokenSupply` straight
// into a handler); if a lake dependency is ever threaded through a helper,
// add the helper here. When a new lake-backed field is added to Server (see
// cmd/stellarindex-api/main.go's `er`/`sr` fan-out), add it to `forbidden`.
func TestSLORoutesNeverTouchTheLake(t *testing.T) {
	// The exact route→handler set the Prometheus SLO burn-rate rules cover.
	sloHandlers := map[string]bool{
		"handlePrice":            false, // /v1/price
		"handlePriceBatch":       false, // GET /v1/price/batch
		"handlePriceBatchPost":   false, // POST /v1/price/batch
		"handleOracleLatest":     false, // /v1/oracle/latest
		"handleOracleLastPrice":  false, // /v1/oracle/lastprice
		"handleOraclePrices":     false, // /v1/oracle/prices
		"handleOracleXLastPrice": false, // /v1/oracle/x_last_price
	}
	// Server fields backed by ClickHouse (the `er`/`sr` fan-out in
	// cmd/stellarindex-api/main.go) plus the explorer sub-handler. Extend
	// with any future cross-region-proxy or S3-fallback client field.
	forbidden := map[string]bool{
		"explorer":            true,
		"explorerHandler":     true,
		"supply":              true,
		"tokenSupply":         true,
		"tokenDecimals":       true,
		"lakeWatermarkReader": true,
		"protocolActivity":    true,
		"dexTVL":              true,
		"sdexOrderBook":       true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil {
					continue
				}
				if _, tracked := sloHandlers[fn.Name.Name]; !tracked {
					continue
				}
				sloHandlers[fn.Name.Name] = true
				recv := ""
				if len(fn.Recv.List) > 0 && len(fn.Recv.List[0].Names) > 0 {
					recv = fn.Recv.List[0].Names[0].Name
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != recv {
						return true
					}
					if forbidden[sel.Sel.Name] {
						t.Errorf("SLO'd handler %s reads lake-backed field %s.%s at %s — the p95≤200ms routes must never depend on the ClickHouse lake (ADR-0050 §3a; see this test's doc comment)",
							fn.Name.Name, recv, sel.Sel.Name, fset.Position(sel.Pos()))
					}
					return true
				})
			}
		}
	}
	for name, found := range sloHandlers {
		if !found {
			t.Errorf("SLO'd handler %s not found in package — renamed? Update this guard alongside slo.yml, or the invariant silently un-pins", name)
		}
	}
}
