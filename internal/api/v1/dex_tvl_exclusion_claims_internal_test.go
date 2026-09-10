// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The scope exclusions are PROSE ON A MONEY SURFACE. Every
// /v1/protocols response carries them in tvl_total.excluded, and
// handleProtocolTVL serves the matching one as the 404 detail on
// /v1/protocols/{name}/tvl — so a reader deciding whether the headline
// omits something they care about is reading these sentences, not the
// code.
//
// That makes a location claim inside one a published claim about the
// API's own shape, and those rot silently: the blend entry asserted
// until 2026-09-09 that lending supplied-value "is published
// per-protocol as bespoke.tvl_usd". No such field is on the wire —
// the lending protocol block carries event and user COUNTS only, and
// says so itself (timescale/bespoke_lending.go: "Pool-level figures
// are event/user COUNTS only"). The only lending tvl_usd the API
// serves is per-POOL, on GET /v1/lending/pools/{pool}/reserves. A
// reader who went looking for the excluded number was sent to a field
// that does not exist.
//
// The rule below is the narrowest one that catches that: an exclusion
// that claims we publish or serve the figure elsewhere must NAME the
// route, and the route must be one this server actually registers. An
// exclusion making no location claim (defindex's double-counting
// argument) is unconstrained.
//
// KNOWN LIMIT (#504): registered is not the same as ANSWERS. This guard
// parses route registrations out of server.go; it cannot tell a working
// route from one that 503s. The blend exclusion cited
// /v1/lending/pools/{pool}/reserves while that route timed out on the
// largest Blend pool, and this test was green throughout — a reader
// following the pointer got the 503 the exclusion was supposed to
// explain away. What that route ACTUALLY answers is now covered where it
// can be measured rather than parsed: test/integration's
// blend_reserves_current_state_test.go executes the reserve read against
// a real ClickHouse and bounds what it costs.

// exclusionLocationClaim matches the verbs that promise a reader they
// can go and read the number somewhere.
var exclusionLocationClaim = regexp.MustCompile(`\b(published|serves?|served)\b`)

// exclusionCitedRoute pulls the /v1/... paths out of a reason. The
// trailing class excludes the sentence punctuation a path can end on.
var exclusionCitedRoute = regexp.MustCompile(`/v1/[A-Za-z0-9/_{}.-]*[A-Za-z0-9}]`)

func TestDEXTVLExclusions_LocationClaimsCiteARegisteredRoute(t *testing.T) {
	t.Parallel()
	routes := registeredV1Routes(t)
	if len(routes) == 0 {
		t.Fatal("no GET /v1/... routes parsed out of server.go — this guard needs re-aiming")
	}

	for _, ex := range dexTVLScopeExclusions {
		if !exclusionLocationClaim.MatchString(ex.Reason) {
			continue
		}
		cited := exclusionCitedRoute.FindAllString(ex.Reason, -1)
		if len(cited) == 0 {
			t.Errorf("exclusion %q claims the figure is published or served elsewhere but names no "+
				"/v1 route; a reader cannot go and check it: %q", ex.Subject, ex.Reason)
			continue
		}
		for _, path := range cited {
			if !routes[path] {
				t.Errorf("exclusion %q cites %s, which this server does not register; "+
					"the reason points a reader at a surface that does not exist", ex.Subject, path)
			}
		}
	}
}

// TestDEXTVLExclusions_LendingPointsAtTheServedFigure pins the specific
// regression: the blend entry must send a reader to the endpoint that
// really carries a lending tvl_usd, and must not resurrect the
// bespoke.tvl_usd field that never shipped.
func TestDEXTVLExclusions_LendingPointsAtTheServedFigure(t *testing.T) {
	t.Parallel()
	var blend string
	for _, ex := range dexTVLScopeExclusions {
		if ex.Subject == "blend" {
			blend = ex.Reason
		}
		if strings.Contains(ex.Reason, "bespoke.tvl_usd") {
			t.Errorf("exclusion %q names bespoke.tvl_usd; the protocol bespoke block carries "+
				"event and user counts only, never a TVL figure", ex.Subject)
		}
	}
	if blend == "" {
		t.Fatal("no blend scope exclusion; the headline must always say why lending is omitted")
	}
	if !strings.Contains(blend, "/v1/lending/pools/{pool}/reserves") {
		t.Errorf("blend exclusion = %q; it must name /v1/lending/pools/{pool}/reserves, the only "+
			"surface serving a lending tvl_usd", blend)
	}
}

// registeredV1Routes reads the route patterns server.go hands to the
// mux. Source-level rather than behavioural because registerRoutes runs
// inside a Server whose construction needs live backends; the property
// worth protecting is only that a cited path is one of the strings this
// package routes at all. Same posture as the AST wiring guard in
// cmd/stellarindex-api.
func registeredV1Routes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		// Patterns are "METHOD /path" or bare "/path".
		if i := strings.LastIndex(pattern, " "); i >= 0 {
			pattern = pattern[i+1:]
		}
		if strings.HasPrefix(pattern, "/v1/") {
			out[strings.TrimSuffix(pattern, "/")] = true
		}
		return true
	})
	return out
}
