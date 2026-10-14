// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package astguard

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestPackageConsts_WalksEveryNonTestFile: the walk sees a constant in any non-test file of the package, in
// file-name then declaration order, and never one from a _test.go file, which no handler can stamp.
func TestPackageConsts_WalksEveryNonTestFile(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"overlay.go":      "package supply\n\nconst BasisLater Basis = \"later\"\n",
		"supply.go":       "package supply\n\ntype Basis string\n\nconst (\n\tBasisA Basis = \"a\"\n\tOther = \"x\"\n)\n",
		"supply_test.go":  "package supply\n\nconst BasisTestOnly Basis = \"test_only\"\n",
		"zz_generated.go": "package supply\n\nconst BasisZ Basis = \"z\"\n",
	})
	got := Values(Typed(t, PackageConsts(t, dir), "Basis"))
	if want := "later,a,z"; strings.Join(got, ",") != want {
		t.Errorf("walk = %v, want [%s]", got, want)
	}
}

// TestTyped_FollowsTheLanguagesTypeCarrying: Go carries a type only into a spec that omits its expression
// list, so an untyped constant with its own value in the same group must not read as a granularity, while a
// spec with neither type nor value repeats the one above it, type included.
func TestTyped_FollowsTheLanguagesTypeCarrying(t *testing.T) {
	dir := writeFixture(t, map[string]string{"fixture.go": `package timescale

const (
	Granularity1m  HistoryGranularity = "1m"
	Granularity15m                    = "15m"
	granularityDocsURL                = "https://example.invalid/granularity"
	maxGranularityLabelLen            = 3
	Granularity1h  HistoryGranularity = "1h"
	Granularity1hAlias
	someOtherWireValue OtherEnum = "1d"
)

const loneUntypedConst = "5m"
`})
	got := ByName(Typed(t, PackageConsts(t, dir), "HistoryGranularity"))
	want := map[string]string{"Granularity1m": "1m", "Granularity1h": "1h", "Granularity1hAlias": "1h"}
	if !maps.Equal(got, want) {
		t.Errorf("typed consts = %v, want %v", got, want)
	}
}

// TestKeep_RefusesANonStringValue: a selected constant the guard cannot read fails loudly instead of
// vanishing from the vocabulary.
func TestKeep_RefusesANonStringValue(t *testing.T) {
	dir := writeFixture(t, map[string]string{"fixture.go": "package p\n\nconst Limit Tier = 3\n"})
	consts := PackageConsts(t, dir)
	if len(consts) != 1 || consts[0].IsString {
		t.Fatalf("consts = %+v, want one non-string const", consts)
	}
	ft := &fakeTB{TB: t}
	Typed(ft, consts, "Tier")
	if !ft.failed {
		t.Error("Typed accepted a non-string Tier constant")
	}
}

type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Fatalf(string, ...any) { f.failed = true }
func (f *fakeTB) Helper()               {}
