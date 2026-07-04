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
//     result used directly as the operand of a numeric conversion.
//
// Escape hatch: a `//i128:ok <reason>` comment on the same line (or
// the line above) exempts a site. Reasons are mandatory; stale
// markers (ones that exempt nothing) fail the test so the allowlist
// can only shrink. See scripts/ci/lint-i128.sh for the fast grep
// sibling and scripts/ci/lint-migrations.sh for the SQL-side guard.

import (
	"fmt"
	"go/ast"
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

var i128OkMarker = regexp.MustCompile(`^\s*i128:ok\s+\S+`)

// loadRepoPackages type-checks every non-test package in the repo
// once (parse once, types per package — packages.Load batches this).
func loadRepoPackages(t *testing.T) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:   repoRoot(),
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			t.Fatalf("package %s failed to load: %v", p.PkgPath, e)
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("packages.Load returned no packages")
	}
	return pkgs
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

// checkConversion inspects one call expression; when it is a lossy
// numeric conversion of a parts-struct word (or a Must* accessor
// result) it returns a violation message, else "".
func checkConversion(info *types.Info, call *ast.CallExpr) string {
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
	arg := ast.Unparen(call.Args[0])

	// (b) Must[IU]128/256() result fed straight into a numeric
	// conversion — never a correct decode.
	if inner, ok := arg.(*ast.CallExpr); ok {
		if sel, ok := ast.Unparen(inner.Fun).(*ast.SelectorExpr); ok && mustAccessors[sel.Sel.Name] {
			return fmt.Sprintf("%s(…%s()) — a 128-bit accessor result must go through canonical.FromInt128Parts/FromUInt128Parts, never a numeric conversion", target.Name(), sel.Sel.Name)
		}
	}

	// (a) lossy conversion of a parts-struct word field.
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok || !partFields[sel.Sel.Name] {
		return ""
	}
	recv, ok := info.Types[sel.X]
	if !ok || !isPartsType(recv.Type) {
		return ""
	}
	fieldKind := basicKind(info.TypeOf(sel))
	if fieldKind == target.Kind() {
		return "" // lossless same-width same-sign conversion (the correct decode shape)
	}
	return fmt.Sprintf("%s(<x>.%s) truncates/reinterprets a 128-bit word (field is %s) — decode via canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)) or the FromUInt* siblings", target.Name(), sel.Sel.Name, info.TypeOf(sel))
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
				msg := checkConversion(pkg.TypesInfo, call)
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
