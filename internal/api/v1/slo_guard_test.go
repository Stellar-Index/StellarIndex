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
// (the realistic regression is wiring `s.explorer`/`s.tokenSupply` straight
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

// TestSLORoutesMatchTheCacheBand pins #820: the SLO burn-rate rules' route
// set (sloHandlers above, mirrored in slo.yml's `route=~` regex) must be a
// subset of middleware.SLOPriceRoutes, the single source of truth
// cachecontrol.go's short-cache-band switch and its own probe-TTL test
// share. A route that pages on the p95 latency SLO but isn't in
// SLOPriceRoutes would silently sit outside the cache band the SLO's
// freshness assumption depends on — exactly the drift #820 found between
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
