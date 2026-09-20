// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestLoadRepoPackages_ScopesLoadErrors pins the loader shared by the
// i128-truncation and AssetType-exhaustiveness guards: a package that
// fails to type-check is fatal only when it is one the guards protect
// (internal/, cmd/, pkg/); a failing scripts/ or test/ tool is skipped,
// not a reason to take both guards down. Packages that load are kept
// whatever their path, and only in-scope ones count toward the floor.
func TestLoadRepoPackages_ScopesLoadErrors(t *testing.T) {
	const module = "example.test/m"
	broken := []packages.Error{{Msg: "undefined: x", Kind: packages.TypeError}}
	pkgs := []*packages.Package{
		{PkgPath: module + "/internal/sources/decoder", Errors: broken},
		{PkgPath: module + "/cmd/tool", Errors: broken},
		{PkgPath: module + "/scripts/dev/scratch", Errors: broken},
		{PkgPath: module + "/pkg/client"},
		{PkgPath: module + "/test/chaos"},
	}
	keep, fatal, skipped := partitionLoadErrors(pkgs, module)

	if got := paths(keep); got != "example.test/m/pkg/client example.test/m/test/chaos" {
		t.Errorf("kept %q, want only the two packages that loaded", got)
	}
	if len(fatal) != 2 || !strings.Contains(fatal[0], "/internal/sources/decoder") || !strings.Contains(fatal[1], "/cmd/tool") {
		t.Errorf("fatal = %q, want the internal/ and cmd/ failures, in order", fatal)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "/scripts/dev/scratch") || !strings.Contains(skipped[0], "undefined: x") {
		t.Errorf("skipped = %q, want the scripts/ failure with its reason", skipped)
	}
	if n := guardedCount(keep, module); n != 1 {
		t.Errorf("guardedCount = %d, want 1 (pkg/client; test/chaos is out of scope)", n)
	}
	if inGuardScope(module+"/internals/x", module) {
		t.Error("inGuardScope matched /internals/ — the segment check must be exact")
	}
}

func paths(pkgs []*packages.Package) string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.PkgPath)
	}
	return strings.Join(out, " ")
}
