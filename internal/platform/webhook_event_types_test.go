// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestWebhookEventTypesListsEveryConstant parses this package's source and
// requires every constant typed WebhookEventType to appear in
// WebhookEventTypes(). Go has no exhaustiveness check over string
// constants, so without this a sixth type would be invisible to every
// consumer of the list (GH-1348).
func TestWebhookEventTypesListsEveryConstant(t *testing.T) {
	declared := declaredWebhookEventTypes(t)
	if len(declared) == 0 {
		t.Fatal("found no WebhookEventType constants — the parser guard is not looking at the right files")
	}
	listed := map[string]bool{}
	for _, e := range WebhookEventTypes() {
		if listed[string(e)] {
			t.Errorf("WebhookEventTypes() lists %q twice", e)
		}
		listed[string(e)] = true
	}
	for name, value := range declared {
		if !listed[value] {
			t.Errorf("constant %s = %q is not in WebhookEventTypes(): subscription validation and metric seeding cannot see it", name, value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("WebhookEventTypes() has %d members, source declares %d constants", len(listed), len(declared))
	}
}

func TestIsWebhookEventType(t *testing.T) {
	for _, e := range WebhookEventTypes() {
		if !IsWebhookEventType(string(e)) {
			t.Errorf("IsWebhookEventType(%q) = false", e)
		}
	}
	for _, s := range []string{"", "incident", "INCIDENT.SEV1", "price.alert "} {
		if IsWebhookEventType(s) {
			t.Errorf("IsWebhookEventType(%q) = true", s)
		}
	}
}

// declaredWebhookEventTypes maps constant name → string value for every
// const spec in this package whose declared type is WebhookEventType.
func declaredWebhookEventTypes(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		collectEventTypeConsts(t, f, out)
	}
	return out
}

func collectEventTypeConsts(t *testing.T, f *ast.File, out map[string]string) {
	t.Helper()
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "WebhookEventType" {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("constant %s is not a string literal; extend the guard", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[name.Name] = v
			}
		}
	}
}
