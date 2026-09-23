package v1_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// TestProductionScamGateIsPairAware pins the type assertion in
// v1.scamWithheld to the type that actually runs in production.
//
// scamWithheld falls back to the BASE-ONLY question for a gate that
// does not implement the pair form. That fallback is a migration seam
// for gates written before the pair question existed — if the
// production gate ever stopped satisfying it, every handler would
// quietly go back to serving a flagged issuer's price through the quote
// leg with no compile error and no failing behavioural test that used a
// fake. This assertion is what makes that impossible.
func TestProductionScamGateIsPairAware(t *testing.T) {
	var gate v1.PriceScamGate = (*pricingguard.ScamGate)(nil)
	if _, ok := gate.(v1.PriceScamPairGate); !ok {
		t.Fatal("*pricingguard.ScamGate no longer implements v1.PriceScamPairGate — " +
			"v1.scamWithheld would silently fall back to the base-only question on " +
			"every handler, re-opening the reciprocal-price bypass (F002)")
	}
}

// TestScamGateIsAskedThePairQuestion fails when any production file
// under internal/api/v1 — this package or any subpackage — can ask the
// scam gate the BASE-ONLY question, or asks a pair decider the same leg
// twice (which is the base-only question in effect).
//
// What it proves: every selector named `Withheld` is a finding whatever
// its receiver — a Server field of any name, a parameter, a local alias,
// a method value — except the one migration fallback inside this
// package's scamWithheld; and no call to a pair decider passes the same
// expression as base and quote. There is no exemption list.
//
// What it does NOT prove: that every price surface consults the gate. A
// surface that asks no gate at all has no call for this scan to match;
// that coverage is per-surface (see pricingguard's package doc) and, for
// reader-backed seams, cmd/stellarindex-api's TestPriceServingSeamsAreGated
// and TestV1VWAPCacheSeamsAreGated.
func TestScamGateIsAskedThePairQuestion(t *testing.T) {
	fset := token.NewFileSet()
	files := parseScamGateTree(t, fset, ".")

	subpackageFiles := 0
	for _, f := range files {
		if f.dir != "." {
			subpackageFiles++
		}
	}
	// internal/api/v1 has subpackages; a walk that sees none has stopped
	// recursing, which is where a base-only call could hide.
	if subpackageFiles == 0 {
		t.Fatal("scanned no subpackage files — the walk no longer recurses, the guard is broken")
	}

	findings, fallbacks := scamGateFindings(fset, files)
	for _, msg := range findings {
		t.Error(msg)
	}
	// The fallback is the one real `.Withheld` call; not seeing it means
	// the predicate no longer matches the shape it exists to catch.
	if fallbacks != 1 {
		t.Errorf("found %d base-only fallbacks inside scamWithheld, want exactly 1 — "+
			"the predicate no longer matches the real call shape", fallbacks)
	}
	t.Logf("scanned %d files (%d in subpackages) for base-only scam-gate consultations (0 permitted outside scamWithheld)",
		len(files), subpackageFiles)
}

// scamGateChokepointFixture mirrors v1.scamWithheld for the shape tests.
const scamGateChokepointFixture = `
func scamWithheld(ctx context.Context, gate PriceScamGate, base, quote canonical.Asset, surface string) bool {
	if pair, ok := gate.(PriceScamPairGate); ok {
		return pair.WithheldPair(ctx, base, quote, surface)
	}
	return gate.Withheld(ctx, base, surface)
}
`

// TestScamGateGuardCatchesEveryReintroductionShape runs the guard's
// predicate over each way a base-only consultation can be spelled, so a
// narrowed predicate fails here rather than going green over the tree.
func TestScamGateGuardCatchesEveryReintroductionShape(t *testing.T) {
	shapes := []struct {
		name, dir, body string
	}{
		{"server field", ".", `func (s *Server) h(ctx context.Context, base canonical.Asset) bool { return s.scam.Withheld(ctx, base, "x") }`},
		{"helper parameter", ".", `func h(ctx context.Context, g PriceScamGate, base canonical.Asset) bool { return g.Withheld(ctx, base, "x") }`},
		{"renamed field", ".", `func (s *Server) h(ctx context.Context, base canonical.Asset) bool { return s.scamGate.Withheld(ctx, base, "x") }`},
		{"local alias", ".", `func (s *Server) h(ctx context.Context, base canonical.Asset) bool { g := s.scam; return g.Withheld(ctx, base, "x") }`},
		{"method value", ".", `func (s *Server) h(ctx context.Context, base canonical.Asset) bool { f := s.scam.Withheld; return f(ctx, base, "x") }`},
		{"package-level method expression", ".", `var ask = (*pricingguard.ScamGate).Withheld`},
		{"pair helper, same leg twice", ".", `func (s *Server) h(ctx context.Context, base canonical.Asset) bool { return scamWithheld(ctx, s.scam, base, base, "x") }`},
		{"withheldBy, same leg twice", ".", `func (s *Server) h(ctx context.Context, a canonical.Asset) pricingguard.Withholding { return withheldBy(ctx, s.substance, s.scam, a, a, "x") }`},
		{"WithheldPair, same leg twice", ".", `func h(ctx context.Context, g PriceScamPairGate, a canonical.Asset) bool { return g.WithheldPair(ctx, a, a, "x") }`},
		{"chokepoint copy in a subpackage", "inner", scamGateChokepointFixture},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			files := []scamGateFile{
				parseScamGateSource(t, fset, ".", scamGateChokepointFixture),
				parseScamGateSource(t, fset, tc.dir, tc.body),
			}
			findings, _ := scamGateFindings(fset, files)
			if len(findings) != 1 {
				t.Fatalf("guard reported %d findings for %q, want 1: %v", len(findings), tc.body, findings)
			}
		})
	}

	t.Run("chokepoint and pair calls are clean", func(t *testing.T) {
		fset := token.NewFileSet()
		files := []scamGateFile{
			parseScamGateSource(t, fset, ".", scamGateChokepointFixture),
			parseScamGateSource(t, fset, ".", `func (s *Server) h(ctx context.Context, p canonical.Pair) bool { return scamWithheld(ctx, s.scam, p.Base, p.Quote, "x") }`),
		}
		findings, fallbacks := scamGateFindings(fset, files)
		if len(findings) != 0 || fallbacks != 1 {
			t.Fatalf("clean fixture: findings=%v fallbacks=%d, want none and 1", findings, fallbacks)
		}
	})
}

// scamPairLegs names each pair decider reachable from internal/api/v1 and
// the argument positions of its base and quote legs.
var scamPairLegs = map[string][2]int{
	"scamWithheld":     {2, 3},
	"withheldBy":       {3, 4},
	"WithheldPair":     {1, 2},
	"PriceWithheld":    {3, 4},
	"PriceWithholding": {3, 4},
}

type scamGateFile struct {
	dir string // relative to the scan root; "." is package v1 itself
	f   *ast.File
}

// parseScamGateTree parses every non-test Go file under root, skipping
// what the go tool skips (testdata, and directories starting with . or _).
func parseScamGateTree(t *testing.T, fset *token.FileSet, root string) []scamGateFile {
	t.Helper()
	var files []scamGateFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		files = append(files, scamGateFile{dir: rel, f: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatal("scanned no package files — the guard is broken, not the code clean")
	}
	return files
}

func parseScamGateSource(t *testing.T, fset *token.FileSet, dir, body string) scamGateFile {
	t.Helper()
	f, err := parser.ParseFile(fset, filepath.Join(dir, "fixture.go"), "package v1\n"+body, 0)
	if err != nil {
		t.Fatalf("parse fixture %q: %v", body, err)
	}
	return scamGateFile{dir: dir, f: f}
}

// scamGateFindings returns one message per base-only consultation and
// same-leg pair question in files, and how many `.Withheld` selectors sit
// inside package v1's scamWithheld (the permitted migration fallback).
func scamGateFindings(fset *token.FileSet, files []scamGateFile) (findings []string, fallbacks int) {
	for _, file := range files {
		for _, decl := range file.f.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			inChokepoint := isFunc && file.dir == "." && fn.Recv == nil && fn.Name.Name == "scamWithheld"
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name != "Withheld" {
						return true
					}
					if inChokepoint {
						fallbacks++
						return true
					}
					findings = append(findings, fmt.Sprintf("%s: asks the scam gate the BASE-ONLY question (%s) "+
						"— call scamWithheld(ctx, gate, base, quote, surface) instead. Keyed on the base alone, "+
						"this surface republishes a flagged issuer's withheld price as its exact reciprocal "+
						"whenever the client names it as the quote (F002).",
						fset.Position(n.Pos()), types.ExprString(n)))
				case *ast.CallExpr:
					if msg := sameLegPairCall(fset, n); msg != "" {
						findings = append(findings, msg)
					}
				}
				return true
			})
		}
	}
	return findings, fallbacks
}

// sameLegPairCall reports a pair decider asked about one leg twice: the
// fold over legs then degenerates to the base-only question.
func sameLegPairCall(fset *token.FileSet, call *ast.CallExpr) string {
	var name string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	default:
		return ""
	}
	legs, ok := scamPairLegs[name]
	if !ok || len(call.Args) <= legs[1] {
		return ""
	}
	base, quote := types.ExprString(call.Args[legs[0]]), types.ExprString(call.Args[legs[1]])
	if base != quote {
		return ""
	}
	return fmt.Sprintf("%s: %s is asked about %s as BOTH legs — that is the base-only question "+
		"in effect; pass the market's real quote.", fset.Position(call.Pos()), name, base)
}
