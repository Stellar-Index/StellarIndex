// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// Lockstep checks for the smaller methodology pages; helpers live in
// methodology_rwa_doc_lockstep_test.go.

func TestMethodologyReadmeIndexesEveryPage(t *testing.T) {
	readme := readMethodologyDoc(t, "README.md")
	pages, err := filepath.Glob(filepath.Join(methodologyDir, "*.md"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("glob methodology pages: %v (%d)", err, len(pages))
	}
	for _, p := range pages {
		name := filepath.Base(p)
		if name != "README.md" && !strings.Contains(readme, "]("+name+")") {
			t.Errorf("README.md does not link %s", name)
		}
	}
}

// usdMarketValue divides by 10^decimals and the served price is not the
// orchestrator's filtered VWAP; the circulating figure is clamped at 0.
func TestXLMSupplyDocFormulaMatchesCode(t *testing.T) {
	doc := readMethodologyDoc(t, "xlm-circulating-supply.md")
	mc := regexp.MustCompile(`(?m)^market_cap_usd\s*=\s*(.+)$`).FindStringSubmatch(doc)
	if mc == nil {
		t.Fatal("no market_cap_usd formula line")
	}
	if !strings.Contains(mc[1], "÷ 10^decimals") || strings.Contains(mc[1], "VWAP") {
		t.Errorf("market_cap_usd formula %q must divide by 10^decimals and must not name a VWAP", mc[1])
	}
	cs := regexp.MustCompile(`(?m)^circulating_supply\s*=\s*(.+)$`).FindStringSubmatch(doc)
	if cs == nil || !strings.HasPrefix(cs[1], "max(0,") {
		t.Errorf("circulating_supply formula must carry the zero clamp, got %v", cs)
	}
}

// The order applyListingValuations and listingValuationCandidate decide
// in. A status earlier here wins over any later one on the same row.
var listingValuationDecisionOrder = []string{
	ListingValuationMarketCapPublished,
	ListingValuationMarketPriceObserved,
	ListingValuationUnavailable,
	ListingValuationNotListed,
	ListingValuationNoListingPrice,
	ListingValuationPriceExpired,
	ListingValuationPriceNotPositive,
	ListingValuationNoSupply,
}

func TestListingValuationDocConditionOrderMatchesCode(t *testing.T) {
	doc := readMethodologyDoc(t, "listing-priced-valuation.md")
	sec := mdSection(t, doc, "## What has to be true")
	var got []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(sec, -1) {
		for _, st := range listingValuationDecisionOrder {
			if m[1] == st && !seen[st] {
				seen[st] = true
				got = append(got, st)
			}
		}
	}
	if !reflect.DeepEqual(got, listingValuationDecisionOrder) {
		t.Errorf("conditions name statuses in order %v, the code decides in %v", got, listingValuationDecisionOrder)
	}
}

// documentedMiddlewareOrder reads the stack order from middleware/doc.go:
// the tab-indented comment block whose lines carry "→".
func documentedMiddlewareOrder(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("middleware", "doc.go"))
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "//\t") && strings.Contains(line, "→") {
			block = append(block, strings.TrimPrefix(line, "//\t"))
		}
	}
	var names []string
	for _, n := range strings.Split(strings.Join(block, " "), "→") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestMiddlewareStackMatchesPackageDoc: middleware/doc.go is the one statement of request-path order and had drifted;
// with every optional middleware wired, the built stack must equal the documented list.
func TestMiddlewareStackMatchesPackageDoc(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	var optional middleware.Middleware = pass
	s := &Server{
		Options:        Options{CORS: optional, Auth: optional, KeyPolicy: optional, RequireEmailVerified: optional, UsageTracker: optional, MonthlyQuota: optional, RateLimit: optional, TouchUsage: optional, SessionAuth: optional},
		mux:            http.NewServeMux(),
		publicRoutes:   middleware.NewPublicRoutes(),
		requestTimeout: time.Second,
	}
	var built []string
	for _, e := range s.middlewareStack() {
		built = append(built, e.name)
	}
	documented := documentedMiddlewareOrder(t)
	if len(documented) == 0 {
		t.Fatal("parsed no names from middleware/doc.go — the parser is not finding the order block")
	}
	if !slices.Equal(built, documented) {
		t.Fatalf("middleware/doc.go order drifted from Server.middlewareStack:\n built:      %v\n documented: %v", built, documented)
	}
}
