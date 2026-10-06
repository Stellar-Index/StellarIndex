// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLateTradeRefreshWindows_SingleWriter pins the clear guard's premise:
// cagg_late_refresh_windows has one writer, LateTradeRefresher, in one
// process, the single stellarindex-indexer unit. The in-flight guard that
// stops a clear racing an uncommitted write sees only its own process's
// writers; a second writer process would rely on the gen guard alone.
func TestLateTradeRefreshWindows_SingleWriter(t *testing.T) {
	root := filepath.Join("..", "..")
	want := map[string][]string{
		"RecordCAGGLateRefreshWindow": {"internal/pipeline/late_trade_refresh.go"},
		"ClearCAGGLateRefreshWindow":  {"internal/pipeline/late_trade_refresh.go"},
		"NewLateTradeRefresher":       {"cmd/stellarindex-indexer/main.go"},
	}
	got := map[string][]string{}
	fset := token.NewFileSet()
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if _, tracked := want[name]; tracked && !slices.Contains(got[name], rel) {
					got[name] = append(got[name], rel)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for name, files := range want {
		if !slices.Equal(got[name], files) {
			t.Errorf("%s is called from %v, want only %v: cagg_late_refresh_windows must keep a single writer", name, got[name], files)
		}
	}

	if _, err := os.Stat(filepath.Join(root, "deploy", "systemd", "stellarindex-indexer.service")); err != nil {
		t.Errorf("the single indexer unit is gone: %v", err)
	}
	for _, dir := range []string{"deploy", "configs"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(d.Name(), "stellarindex-indexer@") {
				t.Errorf("%s: a templated indexer unit runs more than one writer process", path)
			}
			return nil
		})
	}
}
