// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

// This file is the REAL exhaustive-switch guard ADR-0010 asked for
// and never got ("CI check (TODO(#0)) to assert switch-coverage
// exhaustiveness would be tidy; Go 1.21+ has analyzers/exhaustive
// that can enforce it" / "Go's exhaustive linter (if/when we enable
// it) will flag them automatically" — docs/adr/0010-off-chain-fiat-
// representation.md). ROADMAP #48.
//
// We DID already enable golangci-lint's `exhaustive` linter repo-wide
// (.golangci.yml) — but scoped to `default-signifies-exhaustive:
// true`, the low-noise setting. The 2026-07-05 BACKLOG #48 probe
// documented right there in .golangci.yml found that flipping to the
// strict form (`default-signifies-exhaustive: false`) to actually
// verify AssetType switches floods 12 findings repo-wide, only 3 of
// which are AssetType (the rest are reflect.Kind walkers and other
// enums this repo doesn't own) — golangci's `exhaustive` linter has
// no way to scope strictness to a single named type. That's why this
// exists as a second, narrow guard instead of a config tweak:
//
//   - It walks the repo with go/types (same plumbing as
//     TestI128TruncationGuard below/above in this package — see
//     loadRepoPackages/repoRoot, reused verbatim) and only ever looks
//     at switches whose tag type is canonical.AssetType. Zero noise
//     on any other enum, so it can run at full strictness.
//   - It also catches something `default-signifies-exhaustive: true`
//     structurally cannot: a switch that HAS a `default:` clause but
//     the clause is empty or a bare `fallthrough`/`break` — golangci
//     counts the mere PRESENCE of a default as "exhaustive" and never
//     looks inside it. A silently-empty default on a variant it
//     doesn't handle is no safer than no default at all.
//
// The declared-variant set is discovered dynamically from the
// canonical package's own go/types Scope (every package-level
// `const` of type AssetType) rather than hardcoded here — a 7th
// AssetType variant that forgets to update a hardcoded list in THIS
// file would recreate exactly the blind spot the guard exists to
// close.
//
// Escape hatch: this repo already has one for the `exhaustive`
// linter — a `//exhaustive:ignore` comment on the line directly above
// the switch (see internal/storage/timescale/usd_volume_quote_spec.go
// for the one existing, documented site: an intentionally-partial
// switch with prose above it explaining exactly which variants fall
// through and why). This guard honors the SAME marker rather than
// inventing a second one — a switch already justified to golangci's
// linter doesn't need a redundant justification to this one.
//
// PROBE (2026-07-09, per repo review-rule "gates that can't fail are
// decorative"): two scenarios, each applied then reverted before
// commit, `go test -run TestAssetTypeExhaustiveGuard
// ./internal/canonical/` run against each:
//
//  1. Temporarily deleted the `//exhaustive:ignore` line above
//     internal/storage/timescale/usd_volume_quote_spec.go's
//     QuoteUSDPegInfo switch (the one switch in the repo with no
//     default at all). Failed with "switch on canonical.AssetType
//     has no default clause and is missing case(s) for: crypto,
//     fiat, native, rwa" at that exact file:line.
//  2. Temporarily deleted `case AssetRWA:` from Asset.String() in
//     asset.go AND collapsed its `default: return "invalid-asset"`
//     to an empty `default:` (return moved after the switch, so
//     runtime behavior was unchanged — only the switch's shape
//     regressed). Failed with "switch on canonical.AssetType has a
//     default clause with no real body ... missing explicit case(s)
//     for: rwa" at asset.go's String().
//
// (1) proves the "no default, missing case" arm; (2) proves the
// "trivial default, missing case" arm — the one golangci's
// `exhaustive` linter structurally cannot detect (see above). Both
// reds were exact and specific, not generic; both reverted clean
// (`git diff` empty) before this file was committed.
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
