// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestAdminRoutes_AllMountedThroughHandleAdmin scans every non-test source
// under internal/api/v1 so a /v1/admin/ route registered any other way
// (s.mux.HandleFunc, a sub-package Mount) fails CI rather than review.
func TestAdminRoutes_AllMountedThroughHandleAdmin(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, callee, ok := muxSpecCall(n)
			if !ok || !strings.HasPrefix(spec.path, "/v1/admin/") {
				return true
			}
			found++
			if callee != "handleAdmin" {
				t.Errorf("%s: %s %s is mounted via %s — /v1/admin/ routes must be mounted with s.handleAdmin so the operator tier and X-Reason are enforced at the mount",
					fset.Position(n.Pos()), spec.method, spec.path, callee)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api/v1: %v", err)
	}
	if found == 0 {
		t.Fatal("no /v1/admin/ routes found — the scan is broken and would pass forever")
	}
}

// muxSpecCall reports the "METHOD /path" literal a call passes as its first
// argument and the name of the function it calls.
func muxSpecCall(n ast.Node) (mountedRoute, string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return mountedRoute{}, "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return mountedRoute{}, "", false
	}
	spec, err := strconv.Unquote(lit.Value)
	if err != nil {
		return mountedRoute{}, "", false
	}
	m := muxRoute.FindStringSubmatch(spec)
	if m == nil {
		return mountedRoute{}, "", false
	}
	var callee string
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		callee = fn.Sel.Name
	case *ast.Ident:
		callee = fn.Name
	}
	return mountedRoute{m[1], m[2]}, callee, true
}

// TestHandleAdmin_GatesBeforeTheHandler pins the mount-level guard with a
// handler that does no checks of its own, as a new admin route might.
func TestHandleAdmin_GatesBeforeTheHandler(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		subject *auth.Subject
		reason  string
		want    int
	}{
		{"anonymous", http.MethodGet, nil, "", http.StatusUnauthorized},
		{"apikey tier read", http.MethodGet, &auth.Subject{Identifier: "c", Tier: auth.TierAPIKey}, "", http.StatusForbidden},
		{"apikey tier write with reason", http.MethodPost, &auth.Subject{Identifier: "c", Tier: auth.TierAPIKey}, "r", http.StatusForbidden},
		{"operator write without reason", http.MethodPost, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "", http.StatusBadRequest},
		{"operator write with reason", http.MethodPost, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "r", http.StatusTeapot},
		{"operator read without reason", http.MethodGet, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "", http.StatusTeapot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{mux: http.NewServeMux()}
			s.handleAdmin(tc.method+" /v1/admin/probe", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			})
			req := httptest.NewRequest(tc.method, "/v1/admin/probe", nil)
			if tc.subject != nil {
				req = req.WithContext(auth.WithSubject(req.Context(), *tc.subject))
			}
			if tc.reason != "" {
				req.Header.Set("X-Reason", tc.reason)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
