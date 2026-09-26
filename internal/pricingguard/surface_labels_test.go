package pricingguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// metricsReadme is the operator reference whose surface table this test
// pins to the code.
const metricsReadme = "../../docs/reference/metrics/README.md"

// substanceWithheldHeading opens the README section that holds the ONE
// surface table the four price-serve gate counters share.
const substanceWithheldHeading = "### `stellarindex_price_serve_substance_withheld_total`"

// TestPriceServeSurfaceLabelsAreDocumented pins the README's `surface`
// table to the labels the code actually emits, in both directions. The
// documented enums drifted both ways (listing/transitive/twap/vwap/chart
// emitted but undocumented), so an operator reading the reference could
// not tell which paths withhold.
//
// Emitted = every string constant passed as the `surface` argument of a
// non-test function that declares a `surface` parameter, anywhere under
// internal/ and cmd/. A call that forwards its own `surface` parameter
// is followed at ITS callers; any other non-constant argument fails,
// because a label the scan cannot read is a label nobody can document.
func TestPriceServeSurfaceLabelsAreDocumented(t *testing.T) {
	emitted := emittedSurfaceLabels(t, "../../internal", "../../cmd")
	if len(emitted) < 10 {
		t.Fatalf("found only %d surface labels (%v) — the scan is broken, and a guard over an empty set passes forever",
			len(emitted), emitted)
	}
	t.Logf("emitted surface labels: %v", emitted)
	documented := documentedSurfaceLabels(t)
	for _, s := range emitted {
		if !slices.Contains(documented, s) {
			t.Errorf("surface %q is emitted but not in the %s table in %s", s, substanceWithheldHeading, metricsReadme)
		}
	}
	for _, s := range documented {
		if !slices.Contains(emitted, s) {
			t.Errorf("surface %q is documented in %s but no call site emits it", s, metricsReadme)
		}
	}
}

// TestSubstanceFloorLabelsAreDocumented pins every `floor` label value.
func TestSubstanceFloorLabelsAreDocumented(t *testing.T) {
	section := readmeSection(t)
	for _, f := range []SubstanceFloor{FloorBuckets, FloorSpan, FloorVolume, FloorVolumeUnvalued} {
		if !strings.Contains(section, "`"+string(f)+"`") {
			t.Errorf("floor %q is emitted but not documented under %s", f, substanceWithheldHeading)
		}
	}
}

func readmeSection(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(metricsReadme)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, substanceWithheldHeading)
	if start < 0 {
		t.Fatalf("%s: heading %s not found", metricsReadme, substanceWithheldHeading)
	}
	rest := doc[start+len(substanceWithheldHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

var surfaceRow = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")

func documentedSurfaceLabels(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range surfaceRow.FindAllStringSubmatch(readmeSection(t), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("no `surface` table rows under %s", substanceWithheldHeading)
	}
	return out
}

// surfacePackage is one directory's parsed non-test files.
type surfacePackage struct {
	files  []*ast.File
	consts map[string]string
}

func emittedSurfaceLabels(t *testing.T, roots ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string]*surfacePackage{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)
			if pkgs[dir] == nil {
				pkgs[dir] = &surfacePackage{consts: map[string]string{}}
			}
			pkgs[dir].files = append(pkgs[dir].files, f)
			collectStringConsts(f, pkgs[dir].consts)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	params := surfaceParamIndex(pkgs)
	seen := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				idx, ok := params[calleeName(call)]
				if !ok || idx >= len(call.Args) {
					return true
				}
				switch v, how := surfaceValue(call.Args[idx], p.consts); how {
				case "value":
					seen[v] = true
				case "unresolved":
					t.Errorf("%s: surface argument %s is neither a string constant nor a forwarded `surface` parameter",
						fset.Position(call.Pos()), v)
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func collectStringConsts(f *ast.File, into map[string]string) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						into[name.Name] = v
					}
				}
			}
		}
	}
}

// surfaceParamIndex maps each function or method name whose `surface`
// parameter reaches the gate metrics to that parameter's position. The
// seed is this package's own functions; a function elsewhere joins when
// it forwards its `surface` parameter into one already in the set.
func surfaceParamIndex(pkgs map[string]*surfacePackage) map[string]int {
	type decl struct {
		fn  *ast.FuncDecl
		idx int
	}
	var decls []decl
	out := map[string]int{}
	for dir, p := range pkgs {
		seed := filepath.Base(dir) == "pricingguard"
		for _, f := range p.files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if idx := surfaceParam(fn); idx >= 0 {
					if seed {
						out[fn.Name.Name] = idx
					} else {
						decls = append(decls, decl{fn, idx})
					}
				}
			}
		}
	}
	for grew := true; grew; {
		grew = false
		for _, d := range decls {
			if _, ok := out[d.fn.Name.Name]; ok || !forwardsSurface(d.fn, out) {
				continue
			}
			out[d.fn.Name.Name] = d.idx
			grew = true
		}
	}
	return out
}

func surfaceParam(fn *ast.FuncDecl) int {
	i := 0
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			if name.Name == "surface" {
				return i
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	return -1
}

// forwardsSurface reports whether fn passes its own `surface` parameter
// as the surface argument of a function already in reach.
func forwardsSurface(fn *ast.FuncDecl, reach map[string]int) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		if idx, ok := reach[calleeName(call)]; ok && idx < len(call.Args) {
			if id, ok := call.Args[idx].(*ast.Ident); ok && id.Name == "surface" {
				found = true
			}
		}
		return true
	})
	return found
}

func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// surfaceValue resolves a surface argument: ("label", "value"),
// ("", "forwarded") for a function's own surface parameter, or
// (expression, "unresolved").
func surfaceValue(arg ast.Expr, consts map[string]string) (string, string) {
	switch a := arg.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(a.Value); err == nil && a.Kind == token.STRING {
			return v, "value"
		}
	case *ast.Ident:
		if a.Name == "surface" {
			return "", "forwarded"
		}
		if v, ok := consts[a.Name]; ok {
			return v, "value"
		}
		return a.Name, "unresolved"
	}
	return "expression", "unresolved"
}
