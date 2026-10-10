package middleware

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// defaultPolicyAllowlist is every registered GET route that deliberately
// reaches policyForPath's default arm, with the reason. Adding a route
// here is a decision; forgetting to decide is what the test below fails on.
var defaultPolicyAllowlist = map[string]string{
	"/{$}":                             "API root link index; nothing depends on caching it",
	"/robots.txt":                      "handler sets its own Cache-Control",
	"/v1/contracts/{contract_id}/wasm": "handler sets its own Cache-Control",
	"/.well-known/security.txt":        "handler sets its own Cache-Control",
	"/errors/{slug}":                   "handler sets its own Cache-Control",
	"/errors/{$}":                      "handler sets its own Cache-Control",
	"/v1/coverage":                     "handler sets its own Cache-Control",
	"/v1/protocols":                    "handler sets its own Cache-Control",
	"/v1/protocols/{name}":             "handler sets its own Cache-Control",
	"/v1/ledger/tip":                   "handler sets its own Cache-Control",
	"/v1/livez/lake":                   "handler sets its own Cache-Control (no-store)",
	"/v1/ledger/stream":                "SSE stream; private, no-store is correct",
	"/v1/admin/accounts/{id}":          "operator-authenticated",
	"/v1/admin/status-notices":         "operator-authenticated",
	"/v1/signup/verify":                "handler sets its own Cache-Control (no-store confirmation page)",
}

// classified reports whether any explicit arm of policyForPath matches.
func classified(path string) bool {
	if _, ok := ledgerPolicy(path, true); ok {
		return true
	}
	if _, ok := shortBandPolicy(path, true); ok {
		return true
	}
	_, ok := routePolicy(path, true)
	return ok
}

var (
	getRoute    = regexp.MustCompile(`^GET (/\S*)$`)
	routeParam  = regexp.MustCompile(`\{[^}]+\}`)
	sampleParam = map[string]string{
		"{$}":    "",
		"{seq}":  "64000000",
		"{hash}": strings.Repeat("ab", 32),
	}
)

// samplePath turns a mux pattern into a concrete request path, giving the
// typed wildcards a value their route's regexp accepts.
func samplePath(pattern string) string {
	return routeParam.ReplaceAllStringFunc(pattern, func(p string) string {
		if v, ok := sampleParam[p]; ok {
			return v
		}
		return "x"
	})
}

// registeredGETPatterns parses the non-test sources of internal/api/v1 and
// its direct sub-packages and returns every "GET /..." string literal passed
// as a call argument (HandleFunc, Handle, handlePublic, public.Handle, ...).
func registeredGETPatterns(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, glob := range []string{"../*.go", "../*/*.go"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	set := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if m := getRoute.FindStringSubmatch(v); m != nil {
					set[m[1]] = true
				}
			}
			return true
		})
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
