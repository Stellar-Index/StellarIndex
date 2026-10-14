// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package astguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seam scans built on Mentions must see a read however it is spelled, not only as r.store.X(...).
func TestMentions_SeesEverySpelling(t *testing.T) {
	cases := []struct {
		name, src string
		want      bool
	}{
		{"direct call", `func (r R) f() { r.store.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"method value", `func (r R) f() { g := r.store.LatestClosedVWAP1mForPair; g(ctx, a, b) }`, true},
		{"method passed as an argument", `func (r R) f() { apply(r.store.RecentClosedVWAP1mForPair) }`, true},
		{"store alias", `func (r R) f() { st := r.store; st.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"unrelated store read", `func (r R) f() { r.store.LatestTrade(ctx, a, b) }`, false},
		{"same name as a bare identifier", `func (r R) f() { LatestClosedVWAP1mForPair(ctx, a, b) }`, false},
	}
	for _, c := range cases {
		fn := Planted(t, c.src)
		contains := Mentions(fn, func(name string) bool { return strings.Contains(name, "ClosedVWAP") })
		exact := MentionsAny(fn.Body, "LatestClosedVWAP1mForPair", "RecentClosedVWAP1mForPair")
		if contains != c.want || exact != c.want {
			t.Errorf("%s: Mentions %v, MentionsAny %v, want %v", c.name, contains, exact, c.want)
		}
	}
}

func TestParseFile_ReadsTheFileWithItsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte("package x\n\n// Doc.\nfunc F() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fset, f := ParseFile(t, path)
	if len(f.Comments) != 1 || len(f.Decls) != 1 {
		t.Fatalf("got %d comment group(s), %d decl(s); want 1 and 1", len(f.Comments), len(f.Decls))
	}
	if got := fset.Position(f.Decls[0].Pos()).Line; got != 4 {
		t.Errorf("decl line = %d, want 4", got)
	}
}
