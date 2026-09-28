// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// keyHeaderName matches a header name, or the identifier holding one, that
// carries a credential.
var keyHeaderName = regexp.MustCompile(`(?i)^authorization$|api[-_]?key|keyheader`)

// keylessClientLiterals counts the http.Client literals without a
// CheckRedirect that may share a file with a keyed request, because the
// client they build never sends the key.
var keylessClientLiterals = map[string]int{
	"divergence/supply.go": 1, // StellarDashboardReference: an unauthenticated public API
}

// TestKeyedRequestsUseAKeyedClient — a file that puts an API key in a
// request header must not build an http.Client whose redirect policy is
// net/http's default; that client re-sends the key to wherever a 3xx points.
func TestKeyedRequestsUseAKeyedClient(t *testing.T) {
	got := unguardedKeyedClients(t, filepath.Join("..", "..", "internal"))
	for file, n := range got {
		if n > keylessClientLiterals[file] {
			t.Errorf("%s: %d http.Client literal(s) without CheckRedirect beside a keyed request; use httpx.NewKeyedClient", file, n)
		}
	}
	if len(got) == 0 {
		t.Error("scan found no candidate at all; the walker is not reading the tree")
	}
}

func TestKeyedRequestsUseAKeyedClient_CatchesTheDefect(t *testing.T) {
	got := unguardedKeyedClients(t, filepath.Join("testdata", "keyedguard"))
	if got["bad.go"] != 1 || got["good.go"] != 0 || len(got) != 2 {
		t.Fatalf("guard verdict on testdata = %v, want map[bad.go:1 good.go:0]", got)
	}
}

// unguardedKeyedClients maps each non-test file (relative to root) that
// sets a key header to its count of http.Client literals lacking
// CheckRedirect. A keyed file with none still appears, with 0.
func unguardedKeyedClients(t *testing.T, root string) map[string]int {
	t.Helper()
	out := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "testdata" && path != root {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		if setsKeyHeader(f) {
			rel, _ := filepath.Rel(root, path)
			out[filepath.ToSlash(rel)] = unguardedClientLiterals(f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func setsKeyHeader(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Add") {
			return true
		}
		switch arg := call.Args[0].(type) {
		case *ast.BasicLit:
			name, _ := strconv.Unquote(arg.Value)
			found = found || keyHeaderName.MatchString(name)
		case *ast.Ident:
			found = found || keyHeaderName.MatchString(arg.Name)
		case *ast.SelectorExpr:
			found = found || keyHeaderName.MatchString(arg.Sel.Name)
		}
		return true
	})
	return found
}

func unguardedClientLiterals(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Client" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "CheckRedirect" {
					return true
				}
			}
		}
		n++
		return true
	})
	return n
}
