// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package astguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Const is one package-level constant read from source: the type it declares or carries, and its value when
// that is a string literal.
type Const struct {
	Name, Type, File string
	Value            string
	IsString         bool
}

// PackageConsts reads the constants of every non-test .go file in dir: files in name order, constants in
// declaration order. A grouped const block carries the previous spec's values and type into a spec that omits
// its expression list, as the language does; a spec with its own values carries nothing.
func PackageConsts(t testing.TB, dir string) []Const {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []Const
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		out = append(out, FileConsts(t, file)...)
	}
	return out
}

// FileConsts is PackageConsts for one file.
func FileConsts(t testing.TB, path string) []Const {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []Const
	for _, decl := range parsed.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var (
			carriedType   string
			carriedValues []ast.Expr
		)
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if len(vs.Values) > 0 {
				carriedType = ""
				if id, ok := vs.Type.(*ast.Ident); ok {
					carriedType = id.Name
				}
				carriedValues = vs.Values
			}
			for i, name := range vs.Names {
				c := Const{Name: name.Name, Type: carriedType, File: path}
				if i < len(carriedValues) {
					if lit, ok := carriedValues[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							c.Value, c.IsString = s, true
						}
					}
				}
				out = append(out, c)
			}
		}
	}
	return out
}

// Keep returns the constants keep selects, failing t on one that is not a string literal: a guard reading a
// vocabulary from source cannot see such a value and must be extended before it is served.
func Keep(t testing.TB, consts []Const, keep func(Const) bool) []Const {
	t.Helper()
	var out []Const
	for _, c := range consts {
		if !keep(c) {
			continue
		}
		if !c.IsString {
			t.Fatalf("%s: const %s is not a string literal; extend the guard", c.File, c.Name)
		}
		out = append(out, c)
	}
	return out
}

// Typed is Keep for the constants declared or carried with the named type.
func Typed(t testing.TB, consts []Const, typeName string) []Const {
	t.Helper()
	return Keep(t, consts, func(c Const) bool { return c.Type == typeName })
}

// Values lists the constants' values in order.
func Values(consts []Const) []string {
	out := make([]string, 0, len(consts))
	for _, c := range consts {
		out = append(out, c.Value)
	}
	return out
}

// ByName maps constant name to value.
func ByName(consts []Const) map[string]string {
	out := make(map[string]string, len(consts))
	for _, c := range consts {
		out[c.Name] = c.Value
	}
	return out
}
