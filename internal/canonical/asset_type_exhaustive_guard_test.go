// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

// This file is the exhaustive-switch guard named in ADR-0010's Invariant
// (docs/adr/0010-off-chain-fiat-representation.md).
//
// golangci-lint's `exhaustive` linter runs repo-wide with `default-signifies-exhaustive: true`.
// Strict mode (`false`) would flood findings on enums this repo does not own (reflect.Kind
// walkers, etc.), and the linter cannot scope strictness to one named type. So this narrow guard
// exists instead:
//
//   - It walks the repo with go/types (loadRepoPackages/repoRoot, shared with
//     TestI128TruncationGuard) and looks only at switches whose tag type is canonical.AssetType,
//     so it runs at full strictness with no noise.
//   - It catches what `default-signifies-exhaustive: true` cannot: a `default:` that is empty or
//     a bare `fallthrough`/`break`. golangci counts the mere presence of a default as exhaustive,
//     but a silently empty default on an unhandled variant is no safer than none.
//
// The declared-variant set is discovered from the canonical package's go/types Scope (every
// package-level const of type AssetType), never hardcoded, so a new AssetType variant cannot
// escape the guard by being missing from a list here.
//
// Escape hatch: the same `//exhaustive:ignore` marker the linter honors, on the line directly
// above the switch (see internal/storage/timescale/usd_volume_quote_spec.go, an
// intentionally-partial switch with prose naming which variants fall through and why). A switch
// already justified to the linter needs no second justification here.
import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// assetTypeIgnoreMarker matches the golangci-lint `exhaustive`
// linter's own directive syntax so this guard and that linter share
// one escape hatch.
const assetTypeIgnoreMarker = "exhaustive:ignore"

// discoverAssetTypeVariants returns the wire-string value of every
// package-level `const` of type canonical.AssetType, found by
// scanning the loaded canonical package's own types.Scope. Dynamic
// on purpose — see the file doc comment.
func discoverAssetTypeVariants(t *testing.T, pkgs []*packages.Package) map[string]bool {
	t.Helper()
	variants := map[string]bool{}
	for _, pkg := range pkgs {
		if !strings.HasSuffix(pkg.PkgPath, "/internal/canonical") || pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			c, ok := scope.Lookup(name).(*types.Const)
			if !ok {
				continue
			}
			named, ok := c.Type().(*types.Named)
			if !ok || named.Obj().Name() != "AssetType" {
				continue
			}
			if c.Val().Kind() != constant.String {
				continue
			}
			variants[constant.StringVal(c.Val())] = true
		}
	}
	return variants
}

// isAssetTypeNamed reports whether t is canonical.AssetType.
func isAssetTypeNamed(t types.Type) bool {
	if t == nil {
		return false
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == "AssetType" && obj.Pkg() != nil &&
		strings.HasSuffix(obj.Pkg().Path(), "/internal/canonical")
}

// isTrivialDefaultBody reports whether a `default:` clause body is
// empty, or consists solely of `fallthrough`/`break` — i.e. it
// documents no actual handling of the variants it silently covers.
func isTrivialDefaultBody(body []ast.Stmt) bool {
	for _, s := range body {
		bs, ok := s.(*ast.BranchStmt)
		if !ok || (bs.Tok != token.FALLTHROUGH && bs.Tok != token.BREAK) {
			return false
		}
	}
	return true
}

// assetTypeIgnoreLines returns the set of file lines carrying a
// `//exhaustive:ignore` marker — the same directive golangci-lint's
// `exhaustive` linter recognizes.
func assetTypeIgnoreLines(fset *token.FileSet, f *ast.File) map[int]bool {
	out := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if strings.HasPrefix(text, assetTypeIgnoreMarker) {
				out[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	return out
}

// checkAssetTypeSwitch inspects one switch statement already known
// to be tagged on canonical.AssetType. Returns a violation message,
// or "" if the switch either has a default clause with a real body
// or explicitly covers every declared variant.
func checkAssetTypeSwitch(info *types.Info, sw *ast.SwitchStmt, variants map[string]bool) string {
	covered := map[string]bool{}
	hasDefault := false
	hasRealDefault := false

	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		if cc.List == nil {
			hasDefault = true
			hasRealDefault = !isTrivialDefaultBody(cc.Body)
			continue
		}
		for _, expr := range cc.List {
			tv, ok := info.Types[expr]
			if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
				continue
			}
			covered[constant.StringVal(tv.Value)] = true
		}
	}

	if hasRealDefault {
		return ""
	}

	var missing []string
	for v := range variants {
		if !covered[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)

	if hasDefault {
		return fmt.Sprintf(
			"switch on canonical.AssetType has a default clause with no real body "+
				"(empty, or only fallthrough/break) and is missing explicit case(s) for: %s "+
				"— add real handling to the default, explicit cases for the missing variant(s), "+
				"or a `//exhaustive:ignore` with prose explaining the intentional gap",
			strings.Join(missing, ", "))
	}
	return fmt.Sprintf(
		"switch on canonical.AssetType has no default clause and is missing case(s) for: %s "+
			"— add explicit cases, a default with real handling, or a `//exhaustive:ignore` "+
			"with prose explaining the intentional gap",
		strings.Join(missing, ", "))
}

// TestAssetTypeExhaustiveGuard — ROADMAP #48 / ADR-0010. See the file
// doc comment for why this exists alongside the already-enabled
// golangci-lint `exhaustive` linter.
func TestAssetTypeExhaustiveGuard(t *testing.T) {
	pkgs := loadRepoPackages(t)

	variants := discoverAssetTypeVariants(t, pkgs)
	if len(variants) == 0 {
		t.Fatal("discoverAssetTypeVariants found zero canonical.AssetType constants — " +
			"package layout changed under this guard's feet")
	}

	type site struct {
		pos token.Position
		msg string
	}
	var violations []site

	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			ignoreLines := assetTypeIgnoreLines(pkg.Fset, f)
			ast.Inspect(f, func(n ast.Node) bool {
				sw, ok := n.(*ast.SwitchStmt)
				if !ok || sw.Tag == nil {
					return true
				}
				if !isAssetTypeNamed(pkg.TypesInfo.TypeOf(sw.Tag)) {
					return true
				}
				pos := pkg.Fset.Position(sw.Pos())
				if ignoreLines[pos.Line] || ignoreLines[pos.Line-1] {
					return true
				}
				if msg := checkAssetTypeSwitch(pkg.TypesInfo, sw, variants); msg != "" {
					violations = append(violations, site{pos: pos, msg: msg})
				}
				return true
			})
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		return violations[i].pos.String() < violations[j].pos.String()
	})
	for _, v := range violations {
		t.Errorf("%s: %s (ROADMAP #48; ADR-0010)", v.pos, v.msg)
	}
}
