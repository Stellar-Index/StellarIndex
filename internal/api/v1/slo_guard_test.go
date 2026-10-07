package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
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
// (the realistic regression is wiring `s.Explorer`/`s.TokenSupply` straight
// into a handler); if a lake dependency is ever threaded through a helper,
// add the helper here. The forbidden fields are [sloLakeBackedFields];
// TestSLOLakeFieldsCoverLakeWiring fails when main.go wires a lake reader
// into a Server field that set does not name.
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
	forbidden := sloLakeBackedFields

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
				for _, sel := range lakeFieldReads(fn.Body, recv, forbidden) {
					t.Errorf("SLO'd handler %s reads lake-backed field %s at %s — the p95≤200ms routes must never depend on the ClickHouse lake (ADR-0050 §3a; see this test's doc comment)",
						fn.Name.Name, sel.Sel.Name, fset.Position(sel.Pos()))
				}
			}
		}
	}
	for name, found := range sloHandlers {
		if !found {
			t.Errorf("SLO'd handler %s not found in package — renamed? Update this guard alongside slo.yml, or the invariant silently un-pins", name)
		}
	}
}

// TestSLORoutesMatchTheCacheBand pins that the SLO burn-rate rules' route
// set (sloHandlers above, mirrored in slo.yml's `route=~` regex) must be a
// subset of middleware.SLOPriceRoutes, the single source of truth
// cachecontrol.go's short-cache-band switch and its own probe-TTL test
// share. A route that pages on the p95 latency SLO but isn't in
// SLOPriceRoutes would silently sit outside the cache band the SLO's
// freshness assumption depends on — exactly the drift possible between
// this file, slo.yml and the probe-TTL test's hand-typed list.
func TestSLORoutesMatchTheCacheBand(t *testing.T) {
	inBand := make(map[string]bool, len(middleware.SLOPriceRoutes))
	for _, p := range middleware.SLOPriceRoutes {
		inBand[p] = true
	}

	// The route each sloHandlers entry above answers, kept in lockstep
	// with that map's comments.
	sloRoutes := []string{
		"/v1/price",
		"/v1/price/batch",
		"/v1/oracle/latest",
		"/v1/oracle/lastprice",
		"/v1/oracle/prices",
		"/v1/oracle/x_last_price",
	}
	for _, route := range sloRoutes {
		if !inBand[route] {
			t.Errorf("SLO'd route %s is not in middleware.SLOPriceRoutes — TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe will not bound it", route)
		}
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "configs/prometheus/rules.r1/slo.yml"))
	if err != nil {
		t.Fatalf("read slo.yml: %v", err)
	}
	m := regexp.MustCompile(`route=~"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("slo.yml: no route=~\"...\" regex found to check against SLOPriceRoutes")
	}
	for _, route := range strings.Split(string(m[1]), "|") {
		if !inBand[route] {
			t.Errorf("slo.yml route %s is not in middleware.SLOPriceRoutes", route)
		}
	}
}

// lakeFieldReads returns the reads of a forbidden field off recv, as
// `recv.X` or through the embedded Options (`recv.Options.X`).
func lakeFieldReads(body ast.Node, recv string, forbidden map[string]bool) []*ast.SelectorExpr {
	var out []*ast.SelectorExpr
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		x := sel.X
		if inner, ok := x.(*ast.SelectorExpr); ok && inner.Sel.Name == "Options" {
			x = inner.X
		}
		if id, ok := x.(*ast.Ident); ok && id.Name == recv {
			out = append(out, sel)
		}
		return true
	})
	return out
}

func TestLakeFieldReadsCatchesOptionsForm(t *testing.T) {
	const src = `package v1
func (s *Server) direct()  { _ = s.Explorer }
func (s *Server) viaOpts() { _ = s.Options.Explorer }
func (s *Server) other()   { _ = s.Options.Logger; _ = s.Logger }`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"direct": 1, "viaOpts": 1, "other": 0}
	for _, d := range f.Decls {
		fn := d.(*ast.FuncDecl)
		got := len(lakeFieldReads(fn.Body, "s", map[string]bool{"Explorer": true}))
		if got != want[fn.Name.Name] {
			t.Errorf("%s: %d flagged reads, want %d", fn.Name.Name, got, want[fn.Name.Name])
		}
	}
}
