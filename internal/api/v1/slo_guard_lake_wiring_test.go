package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// sloLakeBackedFields are the Server fields TestSLORoutesNeverTouchTheLake
// forbids in an SLO'd handler: every field backed by ClickHouse (the
// `er`/`sr` fan-out in cmd/stellarindex-api/main.go) plus the explorer
// sub-handler, which is built from the whole Options and so cannot be
// derived. Add any future cross-region-proxy or S3-fallback client field by
// hand; lake-wired fields are enforced by TestSLOLakeFieldsCoverLakeWiring.
var sloLakeBackedFields = map[string]bool{
	"Explorer":              true,
	"explorerHandler":       true,
	"Supply":                true,
	"TokenSupply":           true,
	"ContractStorageSupply": true,
	"TokenDecimals":         true,
	"TokenSymbol":           true,
	"IssuerAuthFlags":       true,
	// The readiness checks include the ClickHouse ping (clickhouseReadyChecks).
	"ReadyChecks":      true,
	"LakeWatermark":    true,
	"ProtocolActivity": true,
	"DEXTVL":           true,
	"SDEXOrderBook":    true,
}

// TestSLOLakeFieldsCoverLakeWiring derives, from the production wiring, every
// Server field a ClickHouse reader reaches and requires sloLakeBackedFields
// to name it. The hand-kept list alone is opt-in: a lake reader threaded into
// a new Server field stays invisible to the SLO guard until someone
// remembers to add it.
func TestSLOLakeFieldsCoverLakeWiring(t *testing.T) {
	mainGo := filepath.Join("..", "..", "..", "cmd", "stellarindex-api", "main.go")
	lakeOptions := lakeWiredOptionKeys(t, mainGo)
	optToField := optionToServerFields(t, "server.go")

	derived := map[string]bool{}
	for opt := range lakeOptions {
		// Options is embedded in Server, so a field with no derived copy in
		// server.go lands under its own name.
		fields := optToField[opt]
		if len(fields) == 0 {
			fields = []string{opt}
		}
		for _, f := range fields {
			derived[f] = true
		}
	}
	// Non-vacuity: these come from the two lake dials (explorer and supply
	// readers); a derivation that misses them is not reading the wiring.
	for _, known := range []string{"Explorer", "TokenSupply", "DEXTVL"} {
		if !derived[known] {
			t.Fatalf("derivation missed lake-wired field %q (derived %v) — the scan no longer follows main.go's wiring",
				known, derived)
		}
	}
	for f := range derived {
		if !sloLakeBackedFields[f] {
			t.Errorf("Server field %q is wired from a ClickHouse reader in cmd/stellarindex-api/main.go "+
				"but is missing from sloLakeBackedFields, so an SLO'd handler could read it unguarded", f)
		}
	}
}

// lakeWiredOptionKeys returns the v1.Options keys whose value is reached,
// through plain assignments, from the first result of a clickhouse.* call.
func lakeWiredOptionKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	tainted := map[string]bool{}
	// A variable declared as a clickhouse reader is one whatever fills it,
	// including a helper that dials inside closures.
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && isClickHouseType(vs.Type) {
			for _, id := range vs.Names {
				tainted[id.Name] = true
			}
		}
		return true
	})
	for changed := true; changed; {
		changed = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" || tainted[id.Name] || !taintsLHS(as, i, tainted) {
					continue
				}
				tainted[id.Name] = true
				changed = true
			}
			return true
		})
	}
	keys := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isPkgSelector(cl.Type, "v1", "Options") {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && mentionsAnyIdent(kv.Value, tainted) {
				keys[key.Name] = true
			}
		}
		return true
	})
	return keys
}

// isClickHouseType reports whether t is clickhouse.X or *clickhouse.X.
func isClickHouseType(t ast.Expr) bool {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	sel, ok := t.(*ast.SelectorExpr)
	return ok && isPkgSelector(sel, "clickhouse", sel.Sel.Name)
}

// taintsLHS reports whether as's i-th left-hand side receives a lake value:
// the first result of a clickhouse.* call (never its error), or an
// expression that mentions an already-tainted identifier.
func taintsLHS(as *ast.AssignStmt, i int, tainted map[string]bool) bool {
	if len(as.Rhs) == 1 && len(as.Lhs) > 1 {
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || i != 0 {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && isPkgSelector(sel, "clickhouse", sel.Sel.Name)
	}
	return i < len(as.Rhs) && mentionsAnyIdent(as.Rhs[i], tainted)
}

// optionToServerFields maps each Options field to the Server fields
// assigned from `opts.<Field>` in path, as a composite-literal key or an
// `s.<field> = …` statement.
func optionToServerFields(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string][]string{}
	record := func(field string, value ast.Expr) {
		ast.Inspect(value, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "opts" {
					out[sel.Sel.Name] = append(out[sel.Sel.Name], field)
				}
			}
			return true
		})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			if key, ok := x.Key.(*ast.Ident); ok {
				record(key.Name, x.Value)
			}
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
				return true
			}
			if sel, ok := x.Lhs[0].(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
					record(sel.Sel.Name, x.Rhs[0])
				}
			}
		}
		return true
	})
	return out
}

func mentionsAnyIdent(e ast.Expr, names map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && names[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

func isPkgSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
