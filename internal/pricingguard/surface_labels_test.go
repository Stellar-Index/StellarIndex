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

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// metricsReadme is the operator reference whose surface table this test
// pins to the code.
const metricsReadme = "../../docs/reference/metrics/README.md"

// substanceWithheldHeading opens the README section that holds the ONE
// surface table the four price-serve gate counters share.
const substanceWithheldHeading = "### `stellarindex_price_serve_substance_withheld_total`"

// TestSubstanceWithheldLabelsAreDocumented pins the README section for the four price-serve gate counters to the code,
// in both directions. `surface` labels are string constants passed as `surface` to non-test functions under internal/
// and cmd/ (a forwarded parameter is followed at its callers, any other non-constant argument fails); `floor` labels
// are the SubstanceFloor values.
func TestSubstanceWithheldLabelsAreDocumented(t *testing.T) {
	section := readmeSection(t)
	t.Run("surface", func(t *testing.T) {
		emitted := emittedSurfaceLabels(t, "../../internal", "../../cmd")
		if len(emitted) < 10 {
			t.Fatalf("found only %d surface labels (%v) — the scan is broken, and a guard over an empty set passes forever",
				len(emitted), emitted)
		}
		t.Logf("emitted surface labels: %v", emitted)
		documented := documentedSurfaceLabels(t, section)
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
	})
	t.Run("floor", func(t *testing.T) {
		for _, f := range []SubstanceFloor{FloorBuckets, FloorSpan, FloorVolume, FloorVolumeUnvalued} {
			if !strings.Contains(section, "`"+string(f)+"`") {
				t.Errorf("floor %q is emitted but not documented under %s", f, substanceWithheldHeading)
			}
		}
	})
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

func documentedSurfaceLabels(t *testing.T, section string) []string {
	t.Helper()
	var out []string
	for _, m := range surfaceRow.FindAllStringSubmatch(section, -1) {
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
			for _, c := range astguard.FileConsts(t, path) {
				if c.IsString {
					pkgs[dir].consts[c.Name] = c.Value
				}
			}
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
				// A name can carry several surface positions (pricingguard's
				// Gate.PriceWithheld and wiring.PriceWithheld differ); one must resolve.
				var unresolved string
				resolved := false
				for _, idx := range params[calleeName(call)] {
					if idx >= len(call.Args) {
						continue
					}
					switch v, how := surfaceValue(call.Args[idx], p.consts); how {
					case "value":
						seen[v] = true
						resolved = true
					case "forwarded":
						resolved = true
					default:
						if unresolved == "" {
							unresolved = v
						}
					}
				}
				if !resolved && unresolved != "" {
					t.Errorf("%s: surface argument %s is neither a string constant nor a forwarded `surface` parameter",
						fset.Position(call.Pos()), unresolved)
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

// surfaceParamIndex maps each function or method name whose `surface` parameter reaches the gate metrics to that
// parameter's positions. It is seeded by this package and grows with functions that forward `surface` into one
// already in the set.
func surfaceParamIndex(pkgs map[string]*surfacePackage) map[string][]int {
	type decl struct {
		fn  *ast.FuncDecl
		idx int
	}
	var decls []decl
	out := map[string][]int{}
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
						out[fn.Name.Name] = appendUnique(out[fn.Name.Name], idx)
					} else {
						decls = append(decls, decl{fn, idx})
					}
				}
			}
		}
	}
	joined := make([]bool, len(decls))
	for grew := true; grew; {
		grew = false
		for i, d := range decls {
			if joined[i] || !forwardsSurface(d.fn, out) {
				continue
			}
			joined[i] = true
			out[d.fn.Name.Name] = appendUnique(out[d.fn.Name.Name], d.idx)
			grew = true
		}
	}
	return out
}

func appendUnique(s []int, v int) []int {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
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
func forwardsSurface(fn *ast.FuncDecl, reach map[string][]int) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		for _, idx := range reach[calleeName(call)] {
			if idx >= len(call.Args) {
				continue
			}
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
