package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSameTokens(t *testing.T) {
	const base = "package p\n\n// Doc.\nfunc F() int { return 1 } // trailing\n"
	cases := []struct {
		name string
		b    string
		want bool
	}{
		{"comment reworded", "package p\n\n// Other doc.\nfunc F() int { return 1 }\n", true},
		{"comment deleted", "package p\n\nfunc F() int { return 1 }\n", true},
		{"block comment added", "package p\n\n/* a\nb */\nfunc F() int { return 1 }\n", true},
		{"code changed", "package p\n\n// Doc.\nfunc F() int { return 2 } // trailing\n", false},
		{"directive added", "package p\n\n// Doc.\n//go:noinline\nfunc F() int { return 1 } // trailing\n", false},
		{"build constraint added", "//go:build linux\n\npackage p\n\n// Doc.\nfunc F() int { return 1 } // trailing\n", false},
		{"example output changed", "package p\n\n// Output: 2\nfunc F() int { return 1 }\n", false},
		{"unparsable", "package p\n\nfunc F() int { return \"1 }\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameTokens([]byte(base), []byte(c.b)); got != c.want {
				t.Fatalf("sameTokens = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSameTokensCgoNeedsByteEquality(t *testing.T) {
	a := "package p\n\n// #include <a.h>\nimport \"C\"\n"
	b := "package p\n\n// #include <b.h>\nimport \"C\"\n"
	if sameTokens([]byte(a), []byte(b)) {
		t.Fatal("a cgo preamble change must count as a code change")
	}
}

func TestAffected(t *testing.T) {
	list := `m/a
m/b m/a
m/b [m/b.test] m/a m/x
m/b.test m/b [m/b.test] testing
m/c_test [m/c.test] m/b m/a
m/c
m/d m/x`
	got := affected(list, map[string]bool{"m/a": true})
	want := []string{"m/a", "m/b", "m/c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
}

// The guards PR CI must not skip: each walks production source that the
// import graph does not connect to its own package.
func TestGuardDirsFindsTheTreeWalkingGuards(t *testing.T) {
	dirs, err := guardDirs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, d := range dirs {
		have[d] = true
	}
	for _, want := range []string{
		"internal/canonical",         // i128 and wire-money guards
		"internal/storage/timescale", // trades-retention census
		"internal/pipeline",          // source-spec lockstep
		"internal/api/v1",            // premium and SLO guards
		"cmd/stellarindex-api",       // price-withholding wiring
	} {
		if !have[want] {
			t.Errorf("guardDirs misses %s; found %v", want, dirs)
		}
	}
}

func TestGuardDirsReadsTestFilesOnly(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/a/guard_test.go", "package a\nimport \"go/parser\"\n")
	write("internal/b/b.go", "package b\nimport \"go/parser\"\n")
	write("internal/c/c_test.go", "package c\n")
	write("cmd/d/testdata/x_test.go", "package x\nimport \"go/ast\"\n")
	write("cmd/e/load_test.go", "package e\nimport \"golang.org/x/tools/go/packages\"\n")
	got, err := guardDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cmd/e", "internal/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("guardDirs = %v, want %v", got, want)
	}
}

func TestTouchesProductionGo(t *testing.T) {
	for _, tc := range []struct {
		files []string
		want  bool
	}{
		{[]string{"internal/sources/blend/decode.go"}, true},
		{[]string{"docs/a.md", "cmd/stellarindex-api/main.go"}, true},
		{[]string{"pkg/client/types.go"}, true},
		{[]string{"internal/sources/blend/decode_test.go"}, false},
		{[]string{"scripts/ci/affected-go-pkgs/main.go", "web/explorer/a.ts"}, false},
		{nil, false},
	} {
		if got := touchesProductionGo(tc.files); got != tc.want {
			t.Errorf("touchesProductionGo(%v) = %v, want %v", tc.files, got, tc.want)
		}
	}
}

func TestCitationReaders(t *testing.T) {
	index := map[string]map[string][]string{
		"*.md": {
			"internal/a/a.go": {"docs/plan.md", "docs/other.md"},
			"internal/b/b.go": {"docs/plan.md"},
		},
		"*_test.go": {
			"plan.md":  {"internal/chops/plan_test.go"},
			"other.md": {"internal/chops/plan_test.go", "test/controlwiring/other_test.go"},
		},
	}
	grep := func(fixed, glob string) ([]string, error) { return index[glob][fixed], nil }
	got, err := citationReaders([]string{"internal/a/a.go", "internal/b/b.go", "docs/plan.md", "internal/c/c.go"}, grep)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/chops/plan_test.go", "test/controlwiring/other_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("citationReaders = %v, want %v", got, want)
	}
}

func TestWalkerReaders(t *testing.T) {
	// One reader per spelling a test uses to reach a directory it walks.
	hits := map[string]string{
		`/docs"`:           "internal/ops/supply/supply_audit_args_test.go",
		`"protocols"`:      "internal/api/v1/protocols_verification_pages_test.go",
		`/protocols/`:      "internal/api/v1/other_test.go",
		`"docs/`:           "internal/api/v1/adr_test.go",
		`"new-protocol.md`: "never/asked_test.go",
	}
	grep := func(fixed, glob string) ([]string, error) {
		if glob != "*_test.go" {
			t.Errorf("grep glob = %q, want *_test.go", glob)
		}
		if r, ok := hits[fixed]; ok {
			return []string{r}, nil
		}
		return nil, nil
	}
	got, err := walkerReaders("docs/protocols/new-protocol.md", grep)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/api/v1/adr_test.go",
		"internal/api/v1/other_test.go",
		"internal/api/v1/protocols_verification_pages_test.go",
		"internal/ops/supply/supply_audit_args_test.go",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("walkerReaders = %v, want %v", got, want)
	}
	if got, _ := walkerReaders("README.md", grep); len(got) != 0 {
		t.Errorf("a root file has no ancestor directory to walk, got %v", got)
	}
}
