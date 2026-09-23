// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// cjk repeats a 3-byte code point n times: n code points, 3n bytes.
func cjk(n int) string { return strings.Repeat("名", n) }

// TestRequestMaxLengthCountsCodePoints pins each request-body maxLength
// to the unit the spec (JSON Schema: code points) and Postgres
// (length(): characters) both use. A byte count rejects a spec-valid
// CJK/emoji value at a third of its documented limit.
func TestRequestMaxLengthCountsCodePoints(t *testing.T) {
	type parseFn func(http.ResponseWriter, *http.Request) bool
	accountKey := func(w http.ResponseWriter, r *http.Request) bool { _, ok := parseCreateKeyRequest(w, r); return ok }
	adminKey := func(w http.ResponseWriter, r *http.Request) bool {
		_, ok := parseAdminCreateKeyRequest(w, r)
		return ok
	}
	override := func(w http.ResponseWriter, r *http.Request) bool {
		_, ok := parseAccountOverrideRequest(w, r)
		return ok
	}
	notice := func(w http.ResponseWriter, r *http.Request) bool { _, ok := parseCreateNoticeRequest(w, r); return ok }

	cases := []struct {
		name  string
		parse parseFn
		limit int
		body  func(v string) map[string]any
	}{
		{"account key label", accountKey, 128, func(v string) map[string]any { return map[string]any{"label": v} }},
		{"admin key label", adminKey, 128, func(v string) map[string]any {
			return map[string]any{"identifier": "acct:x", "label": v}
		}},
		{"suspended_reason", override, 500, func(v string) map[string]any {
			return map[string]any{"status": "suspended", "suspended_reason": v}
		}},
		{"notice title", notice, 200, func(v string) map[string]any {
			return map[string]any{"title": v, "body": "b", "severity": "minor"}
		}},
		{"notice body", notice, 5000, func(v string) map[string]any {
			return map[string]any{"title": "t", "body": v, "severity": "minor"}
		}},
	}
	for _, tc := range cases {
		for _, n := range []int{tc.limit, tc.limit + 1} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, n), func(t *testing.T) {
				raw, err := json.Marshal(tc.body(cjk(n)))
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
				ok := tc.parse(rec, req)
				if want := n <= tc.limit; ok != want {
					t.Fatalf("%d code points (%d bytes): ok=%v, want %v; status=%d body=%s",
						n, len(cjk(n)), ok, want, rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// byteLenCapAllowlist is every function under internal/api allowed a
// byte-counted string cap, keyed "<path under internal/api>:<func>". Each
// bounds an ASCII-only domain or a storage/log size, not a spec maxLength.
// An entry that exempts nothing fails the guard, so the list only shrinks.
var byteLenCapAllowlist = map[string]string{
	"v1/explorer/search.go:isLedgerSeq":                      "ASCII decimal ledger sequence",
	"v1/middleware/idempotency.go:Idempotency":               "header size bound on the stored key",
	"v1/middleware/ratelimit.go:RateLimit":                   "bucket key storage bound",
	"v1/middleware/ratelimit.go:RateLimitBySubject":          "bucket key storage bound",
	"v1/middleware/request_id.go:isSafeRequestID":            "ASCII-charset request id",
	"v1/middleware/touch_usage.go:truncateUserAgentForTouch": "stored UA byte budget, rune-safe cut",
	"v1/assets.go:parseAssetListFilters":                     "q cost bound over ASCII asset ids",
	"v1/assets.go:isValidClassicCode":                        "classic asset codes are 1-12 ASCII bytes",
	"v1/incidents.go:summaryFromMarkdown":                    "summary byte budget, rune-safe cut",
	"v1/dashboardpricealerts/handlers.go:validateAsset":      "ASCII asset-id grammar",
}

// TestNoByteLengthCapsOnRequestStrings is the class guard: under
// internal/api it fails on `len(s) > N` / `len(s) >= N` where s is a
// string and N >= 2 — the shape every byte-counted maxLength took. A spec
// maxLength counts code points: use utf8.RuneCountInString.
func TestNoByteLengthCapsOnRequestStrings(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks internal/api; skipped in -short")
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  "..",
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(pkgs) < 2 {
		t.Fatalf("loaded %d packages under internal/api — the guard did not run", len(pkgs))
	}
	used := map[string]bool{}
	var hits []string
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			t.Fatalf("%s: %v", p.PkgPath, p.Errors[0])
		}
		for _, f := range p.Syntax {
			for _, h := range byteLenCaps(p.Fset, p.TypesInfo, f) {
				if _, ok := byteLenCapAllowlist[h.key]; ok {
					used[h.key] = true
					continue
				}
				hits = append(hits, h.pos+" in "+h.key)
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("byte-length caps on strings (spec maxLength counts code points; use utf8.RuneCountInString):\n  %s",
			strings.Join(hits, "\n  "))
	}
	for k := range byteLenCapAllowlist {
		if !used[k] {
			t.Errorf("stale byteLenCapAllowlist entry %q exempts nothing; delete it", k)
		}
	}
}

type byteLenHit struct{ key, pos string }

func byteLenCaps(fset *token.FileSet, info *types.Info, f *ast.File) []byteLenHit {
	var hits []byteLenHit
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if be, ok := n.(*ast.BinaryExpr); ok && isStringLenCap(info, be) {
				pos := fset.Position(be.Pos())
				key := apiRelPath(pos.Filename) + ":" + fd.Name.Name
				hits = append(hits, byteLenHit{key: key, pos: pos.String()})
			}
			return true
		})
	}
	return hits
}

func apiRelPath(file string) string {
	file = filepath.ToSlash(file)
	if i := strings.LastIndex(file, "/internal/api/"); i >= 0 {
		return file[i+len("/internal/api/"):]
	}
	return file
}

func isStringLenCap(info *types.Info, be *ast.BinaryExpr) bool {
	if (be.Op != token.GTR && be.Op != token.GEQ) || !isStringLen(info, be.X) {
		return false
	}
	tv, ok := info.Types[be.Y]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return false
	}
	v, exact := constant.Int64Val(tv.Value)
	return exact && v >= 2
}

func isStringLen(info *types.Info, e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "len" {
		return false
	}
	if _, builtin := info.Uses[id].(*types.Builtin); !builtin {
		return false
	}
	b, ok := info.TypeOf(call.Args[0]).Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}
