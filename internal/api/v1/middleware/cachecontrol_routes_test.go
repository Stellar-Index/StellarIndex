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

// TestPolicyForPath_PublicRoutesOffTheDefault pins the public GET routes
// that used to reach the conservative default with nobody having chosen
// it. Each assertion is the band its data supports; the live-state trio
// stays private, no-store but through an explicit arm (the registry test
// below is what proves the arm exists).
func TestPolicyForPath_PublicRoutesOffTheDefault(t *testing.T) {
	const (
		short     = "public, max-age=10, s-maxage=15"
		current   = "public, max-age=30, s-maxage=60"
		catalogue = "public, max-age=60, s-maxage=300"
		private   = "private, no-store"
	)
	cid := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	tests := []struct{ path, want string }{
		{"/v1/contracts/" + cid, short},
		{"/v1/contracts/" + cid + "/interactions", short},
		{"/v1/contracts/" + cid + "/code-history", short},
		// Latest-N listing with no cursor: tip-advancing, not closed history.
		{"/v1/contracts/" + cid + "/transfers", short},
		{"/v1/status/notices", short},
		{"/v1/lending/pools/" + cid + "/reserves", current},
		{"/v1/external/assets", current},
		{"/v1/external/assets/bitcoin", current},
		{"/v1/markets/sources", catalogue},
		{"/v1/mev", catalogue},
		{"/v1/search", catalogue},
		// Live freeze state: must never be edge-cached.
		{"/v1/anomalies", private},
		{"/v1/divergence", private},
		{"/v1/divergence/series", private},
		// Not over-matched by the new patterns.
		{"/v1/contracts/" + cid + "/transfers/x", private},
		{"/v1/lending/pools/" + cid + "/reserves/x", private},
	}
	for _, tc := range tests {
		if got := policyForPath(tc.path, true); got != tc.want {
			t.Errorf("policyForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	for _, p := range []string{"/v1/anomalies", "/v1/divergence", "/v1/divergence/series"} {
		if !classified(p) {
			t.Errorf("%s reaches the default arm; its no-store must be an explicit decision", p)
		}
	}
}

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
	"/v1/signup/verify":                "single-use email token consumption",
}

// TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated walks the GET
// patterns the v1 server and its sub-packages register and fails on any
// that reaches the default arm without an allowlist entry, and on any
// allowlist entry that no longer does. A new public route therefore cannot
// silently inherit `private, no-store` again.
func TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated(t *testing.T) {
	patterns := registeredGETPatterns(t)
	if len(patterns) < 50 {
		t.Fatalf("found only %d GET routes — the source scan is broken, and a "+
			"guard that finds nothing to check passes forever", len(patterns))
	}
	seen := map[string]bool{}
	for _, pat := range patterns {
		seen[pat] = true
		path := samplePath(pat)
		_, allowed := defaultPolicyAllowlist[pat]
		switch {
		case !classified(path) && !allowed:
			t.Errorf("GET %s (probed as %s) reaches policyForPath's default arm: give it an explicit arm, or add it to defaultPolicyAllowlist with a reason", pat, path)
		case classified(path) && allowed:
			t.Errorf("GET %s is in defaultPolicyAllowlist but now matches an explicit arm — remove the stale entry", pat)
		}
	}
	for pat := range defaultPolicyAllowlist {
		if !seen[pat] {
			t.Errorf("defaultPolicyAllowlist names %s, which is no longer a registered GET route", pat)
		}
	}
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
