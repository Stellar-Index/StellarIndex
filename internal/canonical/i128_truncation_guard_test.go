// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

// This file is the REAL i128-truncation guard ADR-0003 long claimed
// to have (the "custom golangci analyzer" its 2026-06-12 reality note
// admits never existed). It walks every non-test Go package in the
// repo with go/types and FAILS on lossy numeric conversions of the
// hi/lo words of xdr.Int128Parts / xdr.UInt128Parts / xdr.Int256Parts
// / xdr.UInt256Parts — the KALIEN-class bug where int64(parts.Lo)
// silently discards the high 64 bits of a Soroban amount.
//
// What counts as a violation:
//
//   - a conversion of `<x>.Lo` / `<x>.Hi` / `<x>.HiHi|HiLo|LoHi|LoLo`
//     (receiver typed as one of the xdr 128/256-bit parts structs) to
//     any numeric type OTHER than the field's own underlying type.
//     `uint64(p.Lo)` and `int64(p.Hi)` (on Int128Parts) are the
//     correct FromInt128Parts decode shape and pass; `int64(p.Lo)`,
//     `int(p.Lo)`, `float64(p.Hi)`, … are sign-reinterpreting /
//     narrowing / precision-losing and fail.
//   - a `MustI128()` / `MustU128()` / `MustI256()` / `MustU256()`
//     result used as the operand of a numeric conversion.
//   - either of the above reached through a local: `lo := p.Lo;
//     int64(lo)` is the same truncation and is judged the same way,
//     however many plain assignments sit between the word and the
//     conversion (see collectWordTaint).
//   - an `Int64()` / `Uint64()` / `Float64()` call on a math/big Int,
//     Rat or Float. A 128-bit amount widened into big.* is narrowed
//     again there, and the go/types receiver type is the only thing
//     that sees it: `new(big.Float).SetInt(total).Float64()` has no
//     parts word or Must* call left in the expression. Every such site
//     is judged, so each deliberate one carries its reason.
//
// Escape hatch: a `//i128:ok <reason>` comment on the same line (or
// the line above) exempts a site. Reasons are mandatory; stale
// markers (ones that exempt nothing) fail the test so the allowlist
// can only shrink. See scripts/ci/lint-i128.sh for the fast grep
// sibling and scripts/ci/lint-migrations.sh for the SQL-side guard.

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// partTypes are the go-stellar-sdk XDR structs whose word fields a
// lossy conversion truncates. Matched by (type name, pkg path suffix).
var partTypes = map[string]bool{
	"Int128Parts":  true,
	"UInt128Parts": true,
	"Int256Parts":  true,
	"UInt256Parts": true,
}

// partFields are the 64-bit word fields of the parts structs.
var partFields = map[string]bool{
	"Lo": true, "Hi": true,
	"HiHi": true, "HiLo": true, "LoHi": true, "LoLo": true,
}

// mustAccessors flagged when their result feeds a numeric conversion.
var mustAccessors = map[string]bool{
	"MustI128": true, "MustU128": true,
	"MustI256": true, "MustU256": true,
}

// bigNarrowers are the math/big accessors that return a machine number:
// Int64/Uint64 wrap silently outside their range, Float64 rounds.
var bigNarrowers = map[string]bool{"Int64": true, "Uint64": true, "Float64": true}

// bigNumberTypes are the math/big types an amount is widened into.
var bigNumberTypes = map[string]bool{"Int": true, "Rat": true, "Float": true}

var i128OkMarker = regexp.MustCompile(`^\s*i128:ok\s+\S+`)

// guardScope is the import-path segment set the repo-wide guards
// protect — the same directories scripts/ci/lint-i128.sh scans. A load
// error there is fatal: the guard cannot vouch for code it could not
// type-check. One outside it (scripts/ tooling, test/ harnesses) is
// reported and that package skipped, so a scratch file cannot take the
// guard down with it.
var guardScope = []string{"/internal/", "/cmd/", "/pkg/"}

// minGuardedPackages is the floor on in-scope packages the loader must
// return (132 when set). A silently narrowed load — a changed pattern, a
// moved module boundary — must not green the guard on a near-empty tree.
const minGuardedPackages = 100

func inGuardScope(pkgPath, module string) bool {
	rel := strings.TrimPrefix(pkgPath, module)
	for _, seg := range guardScope {
		if strings.HasPrefix(rel, seg) {
			return true
		}
	}
	return false
}

// partitionLoadErrors keeps the packages the guards can walk and splits
// the load failures by scope: in-scope failures are fatal, out-of-scope
// ones are skipped with their reason.
func partitionLoadErrors(pkgs []*packages.Package, module string) (keep []*packages.Package, fatal, skipped []string) {
	for _, p := range pkgs {
		if len(p.Errors) == 0 {
			keep = append(keep, p)
			continue
		}
		msg := fmt.Sprintf("package %s failed to load: %v", p.PkgPath, p.Errors)
		if inGuardScope(p.PkgPath, module) {
			fatal = append(fatal, msg)
		} else {
			skipped = append(skipped, msg)
		}
	}
	return keep, fatal, skipped
}

// guardedCount is how many of pkgs are in guardScope.
func guardedCount(pkgs []*packages.Package, module string) int {
	n := 0
	for _, p := range pkgs {
		if inGuardScope(p.PkgPath, module) {
			n++
		}
	}
	return n
}

// loadRepoPackages type-checks every non-test package in the repo
// once (parse once, types per package — packages.Load batches this).
func loadRepoPackages(t *testing.T) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo |
			packages.NeedModule,
		Dir:   repoRoot(),
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	module := ""
	for _, p := range pkgs {
		if p.Module != nil {
			module = p.Module.Path
			break
		}
	}
	if module == "" {
		t.Fatal("packages.Load returned no package with a module path")
	}
	keep, fatal, skipped := partitionLoadErrors(pkgs, module)
	for _, m := range skipped {
		t.Logf("skipping out-of-scope %s", m)
	}
	for _, m := range fatal {
		t.Error(m)
	}
	if len(fatal) > 0 {
		t.FailNow()
	}
	if n := guardedCount(keep, module); n < minGuardedPackages {
		t.Fatalf("only %d in-scope packages loaded (floor %d) — the load has been narrowed and the guard would be vacuous", n, minGuardedPackages)
	}
	return keep
}

func repoRoot() string { return "../.." }

// markerLines returns the file lines carrying an //i128:ok marker.
func markerLines(fset *token.FileSet, f *ast.File) map[int]bool {
	out := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimPrefix(c.Text, "//")
			if i128OkMarker.MatchString(text) {
				out[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	return out
}

// isPartsType reports whether t (after pointer deref) is one of the
// xdr 128/256-bit parts structs.
func isPartsType(t types.Type) bool {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return partTypes[named.Obj().Name()] && strings.HasSuffix(named.Obj().Pkg().Path(), "/xdr")
}

// basicKind resolves a type to its underlying basic kind, or
// types.Invalid when it is not a basic type.
func basicKind(t types.Type) types.BasicKind {
	if b, ok := t.Underlying().(*types.Basic); ok {
		return b.Kind()
	}
	return types.Invalid
}

// wordTaint is what an operand is known to carry: a parts-struct word
// (field name + the word's own type) or a Must[IU]128/256() result.
type wordTaint struct {
	field    string
	fieldTyp types.Type
	accessor string
}

// classifyWord recognises the three spellings of the same operand — the
// word itself, a Must* call, or a local bound to either — so a conversion
// is judged the same whichever the author picked. A guard that only knew
// the inline shape was defeated by `lo := p.Lo; int64(lo)`.
func classifyWord(info *types.Info, tainted map[types.Object]wordTaint, expr ast.Expr) (wordTaint, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.CallExpr:
		if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok && mustAccessors[sel.Sel.Name] {
			return wordTaint{accessor: sel.Sel.Name}, true
		}
	case *ast.SelectorExpr:
		if !partFields[e.Sel.Name] {
			return wordTaint{}, false
		}
		if recv, ok := info.Types[e.X]; ok && isPartsType(recv.Type) {
			return wordTaint{field: e.Sel.Name, fieldTyp: info.TypeOf(e)}, true
		}
	case *ast.Ident:
		if obj := info.ObjectOf(e); obj != nil {
			tn, ok := tainted[obj]
			return tn, ok
		}
	}
	return wordTaint{}, false
}

// eachBinding calls fn for every `name := value`, `name = value` and
// `var name = value` pair in n (pairwise forms only; a tuple-returning
// call binds nothing the guard can name).
func eachBinding(n ast.Node, fn func(lhs ast.Expr, rhs ast.Expr)) {
	switch s := n.(type) {
	case *ast.AssignStmt:
		if len(s.Lhs) == len(s.Rhs) {
			for i := range s.Lhs {
				fn(s.Lhs[i], s.Rhs[i])
			}
		}
	case *ast.ValueSpec:
		if len(s.Names) == len(s.Values) {
			for i := range s.Names {
				fn(s.Names[i], s.Values[i])
			}
		}
	}
}

// collectWordTaint records every object bound to a parts word, a Must*
// result, or another tainted object, iterating to a fixpoint so a chain
// of locals is seen through. Flow-insensitive on purpose: a local that
// ever held a word is treated as the word (escape with //i128:ok).
func collectWordTaint(info *types.Info, files []*ast.File) map[types.Object]wordTaint {
	tainted := map[types.Object]wordTaint{}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				eachBinding(n, func(lhs, rhs ast.Expr) {
					id, ok := ast.Unparen(lhs).(*ast.Ident)
					if !ok {
						return
					}
					obj := info.ObjectOf(id)
					if obj == nil {
						return
					}
					if _, seen := tainted[obj]; seen {
						return
					}
					if tn, ok := classifyWord(info, tainted, rhs); ok {
						tainted[obj] = tn
						changed = true
					}
				})
				return true
			})
		}
	}
	return tainted
}

// checkConversion inspects one call expression; when it is a lossy
// numeric conversion of a parts-struct word (or a Must* accessor
// result), directly or through a tainted local, it returns a violation
// message, else "".
func checkConversion(info *types.Info, tainted map[types.Object]wordTaint, call *ast.CallExpr) string {
	if len(call.Args) != 1 {
		return ""
	}
	tv, ok := info.Types[call.Fun]
	if !ok || !tv.IsType() {
		return "" // not a conversion
	}
	target, ok := tv.Type.Underlying().(*types.Basic)
	if !ok || target.Info()&types.IsNumeric == 0 {
		return ""
	}
	tn, ok := classifyWord(info, tainted, call.Args[0])
	if !ok {
		return ""
	}
	via := ""
	if id, isIdent := ast.Unparen(call.Args[0]).(*ast.Ident); isIdent {
		via = fmt.Sprintf(" (through local %q)", id.Name)
	}

	// (b) Must[IU]128/256() result fed into a numeric conversion — never
	// a correct decode.
	if tn.accessor != "" {
		return fmt.Sprintf("%s(…%s())%s — a 128-bit accessor result must go through canonical.FromInt128Parts/FromUInt128Parts, never a numeric conversion", target.Name(), tn.accessor, via)
	}

	// (a) lossy conversion of a parts-struct word field.
	if basicKind(tn.fieldTyp) == target.Kind() {
		return "" // lossless same-width same-sign conversion (the correct decode shape)
	}
	return fmt.Sprintf("%s(<x>.%s)%s truncates/reinterprets a 128-bit word (field is %s) — decode via canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)) or the FromUInt* siblings", target.Name(), tn.field, via, tn.fieldTyp)
}

// isBigNumber reports whether t (after pointer deref) is big.Int,
// big.Rat or big.Float.
func isBigNumber(t types.Type) bool {
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path() == "math/big" && bigNumberTypes[named.Obj().Name()]
}

// checkBigNarrowing returns a violation message when call narrows a
// math/big value to a machine number via Int64/Uint64/Float64, else "".
func checkBigNarrowing(info *types.Info, call *ast.CallExpr) string {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || len(call.Args) != 0 || !bigNarrowers[sel.Sel.Name] {
		return ""
	}
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.MethodVal || !isBigNumber(s.Recv()) {
		return ""
	}
	return fmt.Sprintf("%s.%s() narrows an arbitrary-precision value to a machine number — keep amounts in canonical.Amount / *big.Int / *big.Rat end to end and render with FloatString", s.Recv(), sel.Sel.Name)
}

// TestI128TruncationGuard — ADR-0003. Repo-wide go/types walk
// rejecting int64/float/narrowing conversions of i128/u128/i256/u256
// words. Every finding must be fixed or carry an //i128:ok marker
// with a reason.
func TestI128TruncationGuard(t *testing.T) {
	pkgs := loadRepoPackages(t)

	type site struct {
		pos token.Position
		msg string
	}
	var violations []site
	usedMarkers := map[string]bool{}
	allMarkers := map[string]token.Position{}

	for _, pkg := range pkgs {
		tainted := collectWordTaint(pkg.TypesInfo, pkg.Syntax)
		for _, f := range pkg.Syntax {
			markers := markerLines(pkg.Fset, f)
			for line := range markers {
				key := fmt.Sprintf("%s:%d", pkg.Fset.Position(f.Pos()).Filename, line)
				allMarkers[key] = token.Position{Filename: pkg.Fset.Position(f.Pos()).Filename, Line: line}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				msg := checkConversion(pkg.TypesInfo, tainted, call)
				if msg == "" {
					msg = checkBigNarrowing(pkg.TypesInfo, call)
				}
				if msg == "" {
					return true
				}
				pos := pkg.Fset.Position(call.Pos())
				if markers[pos.Line] || markers[pos.Line-1] {
					mLine := pos.Line
					if !markers[mLine] {
						mLine = pos.Line - 1
					}
					usedMarkers[fmt.Sprintf("%s:%d", pos.Filename, mLine)] = true
					return true
				}
				violations = append(violations, site{pos: pos, msg: msg})
				return true
			})
		}
	}

	for _, v := range violations {
		t.Errorf("%s: %s (ADR-0003; annotate with `//i128:ok <reason>` ONLY if genuinely non-monetary)", v.pos, v.msg)
	}
	for key, pos := range allMarkers {
		if !usedMarkers[key] {
			t.Errorf("%s: stale //i128:ok marker — it exempts no conversion on its own or the next line; remove it", pos)
		}
	}
}

// TestI128TruncationGuard_PositiveControl proves the DETECTOR still FIRES on
// the exact KALIEN-class truncation it exists to catch. Without it,
// TestI128TruncationGuard asserts only the ABSENCE of violations against the
// live tree — so a silent rot of partTypes / partFields / mustAccessors /
// isPartsType (an SDK field rename, a moved Int128Parts, a changed /xdr path)
// would make the guard return "" for every site and pass on an effectively
// empty tree: "detector works, tree clean" becomes indistinguishable from
// "detector broken, finds nothing" (audit W6-tst-2, the "a guard that never
// fails is decorative" class).
//
// It is fully self-contained — a synthetic package whose path ends in "/xdr"
// with a locally-defined Int128Parts, so it needs no importer and cannot go
// stale against the real SDK. It exercises every fragile table the real
// detector depends on: partTypes (the struct name), partFields (Hi/Lo),
// isPartsType (the /xdr suffix + Named-type resolution), the field-kind
// comparison, and mustAccessors (the Must* branch).
func TestI128TruncationGuard_PositiveControl(t *testing.T) {
	const src = `package xdr

type Int128Parts struct {
	Hi int64
	Lo uint64
}

type Amount struct{}

func (Amount) MustI128() int64 { return 0 }

func sink() {
	var p Int128Parts
	var a Amount
	_ = int64(p.Lo)         // TRUNCATE: sign-reinterpret + is the KALIEN bug
	_ = float64(p.Hi)       // TRUNCATE: precision loss above 2^53
	_ = int32(p.Lo)         // TRUNCATE: narrowing
	_ = uint64(p.Lo)        // OK: the correct FromInt128Parts low-word shape
	_ = int64(p.Hi)         // OK: the correct FromInt128Parts high-word shape
	_ = int64(a.MustI128()) // TRUNCATE: Must* result fed to a conversion

	lo := p.Lo
	_ = int64(lo)  // TRUNCATE: the same word, bound to a local first
	_ = uint64(lo) // OK: the correct shape through a local
	lo2 := lo
	_ = int32(lo2) // TRUNCATE: two locals deep
	var hi int64 = p.Hi
	_ = float64(hi) // TRUNCATE: var-declared local
	m := a.MustI128()
	_ = float64(m) // TRUNCATE: Must* result bound to a local
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "xdr.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	// The package PATH must end in "/xdr" so isPartsType's suffix check
	// matches — this is one of the fragile predicates under test.
	if _, err := (&types.Config{}).Check("example.test/fake/xdr", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check synthetic source: %v", err)
	}
	tainted := collectWordTaint(info, []*ast.File{f})

	fired := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var b strings.Builder
		if err := printer.Fprint(&b, fset, call); err != nil {
			t.Fatalf("render call: %v", err)
		}
		fired[b.String()] = checkConversion(info, tainted, call) != ""
		return true
	})

	mustFire := []string{
		"int64(p.Lo)", "float64(p.Hi)", "int32(p.Lo)", "int64(a.MustI128())",
		"int64(lo)", "int32(lo2)", "float64(hi)", "float64(m)",
	}
	mustPass := []string{"uint64(p.Lo)", "int64(p.Hi)", "uint64(lo)"}
	for _, k := range mustFire {
		if seen, ok := fired[k]; !ok {
			t.Fatalf("positive-control expr %s never inspected — synthetic source drifted", k)
		} else if !seen {
			t.Errorf("DETECTOR ROT (W6-tst-2): checkConversion did NOT fire on %s — the i128 guard no longer catches the truncation class it exists for; check partTypes/partFields/mustAccessors/isPartsType against the current SDK", k)
		}
	}
	for _, k := range mustPass {
		if fired[k] {
			t.Errorf("checkConversion FALSE-fired on the correct decode shape %s — it would reject valid FromInt128Parts code", k)
		}
	}
}

// TestI128TruncationGuard_BigNarrowingPositiveControl proves the math/big
// sink fires on the widen-then-narrow spellings that carry no parts word
// or Must* call — the aggregate VWAP weight's original
// `new(big.Float).SetInt(totalQuote).Float64()` among them — and stays
// silent on same-named methods of other types.
func TestI128TruncationGuard_BigNarrowingPositiveControl(t *testing.T) {
	const src = `package sink

import "math/big"

type gauge struct{}

func (gauge) Float64() float64 { return 0 }

func sink(total, part *big.Int, r big.Rat) {
	_, _ = new(big.Float).SetInt(total).Float64()
	_, _ = new(big.Rat).SetFrac(part, total).Float64()
	_ = total.Int64()
	_ = total.Uint64()
	_, _ = r.Float64()
	_ = r.Num().Int64()
	_ = gauge{}.Float64()
	_ = total.IsInt64()
	_ = r.FloatString(7)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sink.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	if _, err := (&types.Config{Importer: importer.Default()}).Check("example.test/sink", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check synthetic source: %v", err)
	}

	fired := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var b strings.Builder
		if err := printer.Fprint(&b, fset, call); err != nil {
			t.Fatalf("render call: %v", err)
		}
		fired[b.String()] = checkBigNarrowing(info, call) != ""
		return true
	})

	mustFire := []string{
		"new(big.Float).SetInt(total).Float64()", "new(big.Rat).SetFrac(part, total).Float64()",
		"total.Int64()", "total.Uint64()", "r.Float64()", "r.Num().Int64()",
	}
	mustPass := []string{"gauge{}.Float64()", "total.IsInt64()", "r.FloatString(7)", "new(big.Float).SetInt(total)"}
	for _, k := range mustFire {
		if seen, ok := fired[k]; !ok {
			t.Fatalf("positive-control expr %s never inspected — synthetic source drifted", k)
		} else if !seen {
			t.Errorf("checkBigNarrowing did NOT fire on %s — a math/big value reaches a machine-number sink unjudged", k)
		}
	}
	for _, k := range mustPass {
		if seen, ok := fired[k]; !ok {
			t.Fatalf("control expr %s never inspected — synthetic source drifted", k)
		} else if seen {
			t.Errorf("checkBigNarrowing FALSE-fired on %s", k)
		}
	}
}
