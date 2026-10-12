// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// The explorer's /methodology page is hand-written prose covering a superset of /v1/methodology, so it is not generated.
// This file renders the real endpoint and page and fails when they disagree on a fact both state (e.g. VWAP is
// unfiltered by default).

const methodologyPagePath = "web/explorer/src/app/methodology/page.tsx"

// anomaliesPagePath is the /anomalies page's REASONS list, with the same drift risk as
// /methodology but for the freeze-reason vocabulary.
const anomaliesPagePath = "web/explorer/src/app/anomalies/page.tsx"

// freezeEventsSourcePath is mapFreezeReason's home. It is unexported (package timescale), so
// the test greps its source for the string literals it returns, the only way an external test
// package can pin an unexported function's range.
const freezeEventsSourcePath = "internal/storage/timescale/freeze_events.go"

// TestAnomaliesPage_ReasonsAreReachable pins that every freeze reason the /anomalies page lists is a real
// mapFreezeReason return value; `single_source` and `manual` must not appear since the mapper never writes them.
func TestAnomaliesPage_ReasonsAreReachable(t *testing.T) {
	page := methodologyReadRepoFile(t, anomaliesPagePath)
	mapper := methodologyReadRepoFile(t, freezeEventsSourcePath)

	nameRe := regexp.MustCompile(`name:\s*'([a-z_]+)'`)
	names := nameRe.FindAllStringSubmatch(page, -1)
	if len(names) == 0 {
		t.Fatalf("%s: no REASONS entries found — the scan is broken, not the prose", anomaliesPagePath)
	}
	for _, m := range names {
		name := m[1]
		returnRe := regexp.MustCompile(`return\s*"` + regexp.QuoteMeta(name) + `"`)
		if !returnRe.MatchString(mapper) {
			t.Errorf("%s lists freeze reason %q, but mapFreezeReason (%s) never returns it — "+
				"a reader would look for a reason the automated mapper cannot write",
				anomaliesPagePath, name, freezeEventsSourcePath)
		}
	}
}

func methodologyRepoRoot(t *testing.T) string {
	t.Helper()
	// internal/api/v1 -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

func methodologyReadRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(methodologyRepoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// servedMethodology renders GET /v1/methodology through the real handler and returns the decoded `data`, so the test
// sees what a client sees, including request-time derivations.
func servedMethodology(t *testing.T) map[string]any {
	t.Helper()
	ts := httpTestServer(t, v1.New(v1.Options{}))
	resp := mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/methodology = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode methodology: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatal("methodology served an empty data object — every assertion below would be vacuous")
	}
	return env.Data
}

// adrFrontMatterTitle returns the `title:` line of docs/adr/<id>-*.md,
// or "" when no such ADR exists.
func adrFrontMatterTitle(t *testing.T, root, numericID string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "docs", "adr", numericID+"-*.md"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	for _, line := range strings.Split(string(b), "\n")[:min(12, len(strings.Split(string(b), "\n")))] {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "title:"); ok {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), `"'`))
		}
	}
	return ""
}

// TestMethodologyPage_ADRCitationsResolve: the page may cite ADRs the endpoint does not and vice versa, but must
// never cite an ADR that does not exist.
func TestMethodologyPage_ADRCitationsResolve(t *testing.T) {
	root := methodologyRepoRoot(t)
	page := methodologyReadRepoFile(t, methodologyPagePath)

	cites := regexp.MustCompile(`<ADRRef\s+id="(\d{4})"`).FindAllStringSubmatch(page, -1)
	if len(cites) == 0 {
		t.Fatalf("%s cites no ADRs — either the page changed shape or this scan is broken", methodologyPagePath)
	}
	seen := map[string]bool{}
	for _, m := range cites {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		if adrFrontMatterTitle(t, root, m[1]) == "" {
			t.Errorf("%s cites ADR-%s, which has no docs/adr/%s-*.md", methodologyPagePath, m[1], m[1])
		}
	}
}

// TestMethodologyPage_SourceClassesMatchTheEndpoint pins the source-class list both enumerate ("one of four source
// classes"); the endpoint defines the count and names, and the page must move with it.
func TestMethodologyPage_SourceClassesMatchTheEndpoint(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)
	data := servedMethodology(t)

	rawClasses, ok := data["source_classes"].([]any)
	if !ok || len(rawClasses) == 0 {
		t.Fatal("methodology served no source_classes[]")
	}
	var served []string
	for _, raw := range rawClasses {
		c, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("source class is not an object: %#v", raw)
		}
		name, _ := c["name"].(string)
		if name == "" {
			t.Fatalf("source class has no name: %#v", c)
		}
		served = append(served, name)
	}
	sort.Strings(served)

	for _, name := range served {
		if !strings.Contains(page, fmt.Sprintf("term: '%s'", name)) {
			t.Errorf("/v1/methodology serves source class %q but %s never names it — "+
				"the page's taxonomy is behind the API's",
				name, methodologyPagePath)
		}
	}

	// The stated count must match too, or the page can quietly describe
	// four classes while listing five.
	numerals := map[int]string{
		2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight",
	}
	want, ok := numerals[len(served)]
	if !ok {
		t.Fatalf("no numeral for %d source classes — extend the map", len(served))
	}
	m := regexp.MustCompile(`one of (\w+)\s*\n?\s*source`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s no longer states how many source classes there are "+
			"(expected prose of the form \"one of four source classes\") — "+
			"either restore the claim or drop this assertion deliberately",
			methodologyPagePath)
	}
	if m[1] != want {
		t.Errorf("%s says a venue carries one of %s source classes; /v1/methodology serves %d (%s)",
			methodologyPagePath, m[1], len(served), want)
	}
}

// TestMethodologyPage_DefersOnOperatorConfiguredFacts: the stablecoin-to-fiat proxy map is operator config, so the page
// must point at the served field rather than restate a mapping list that goes stale.
func TestMethodologyPage_DefersOnOperatorConfiguredFacts(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)

	if !strings.Contains(page, "stablecoin_fiat_proxy") {
		t.Errorf("%s no longer points at `stablecoin_fiat_proxy` on /v1/methodology — "+
			"the peg map is operator configuration and cannot be stated in prose",
			methodologyPagePath)
	}

	// A hard-coded arrow enumeration is the shape the stale list took.
	// Any asset code followed by `→ USD`/`→ EUR` is the page asserting
	// a peg it cannot know.
	if m := regexp.MustCompile(`[A-Z]{3,6},?\s*(?:[A-Z]{3,6},?\s*)*→\s*(?:USD|EUR|MXN)`).
		FindString(page); m != "" {
		t.Errorf("%s hard-codes a stablecoin peg mapping (%q); "+
			"the live map is served as aggregation.stablecoin_fiat_proxy",
			methodologyPagePath, strings.TrimSpace(m))
	}
}

// TestMethodologyPage_OutlierClaimMatchesTheEndpoint: the default sigma applies to one endpoint and the others are
// unfiltered, so an unqualified "filtered before the average" on the page would claim a protection /v1/vwap lacks.
func TestMethodologyPage_OutlierClaimMatchesTheEndpoint(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)
	data := servedMethodology(t)

	agg, ok := data["aggregation"].(map[string]any)
	if !ok {
		t.Fatal("methodology served no aggregation object")
	}
	filter, ok := agg["outlier_filter"].(map[string]any)
	if !ok {
		t.Fatal("methodology served no aggregation.outlier_filter")
	}
	filtered, _ := filter["endpoint"].(string)
	if filtered == "" {
		t.Fatal("aggregation.outlier_filter carries no endpoint")
	}
	note, _ := filter["note"].(string)
	if !strings.Contains(note, "UNFILTERED") {
		t.Fatalf("the served outlier note no longer says which endpoints are unfiltered "+
			"(%q) — this test's premise changed; re-derive it", note)
	}

	// Whichever endpoint the API says it filters, the page must name it
	// as the filtered one.
	if !strings.Contains(page, filtered) {
		t.Errorf("/v1/methodology says the default outlier filter applies to %s, "+
			"but %s never names that endpoint", filtered, methodologyPagePath)
	}
	// …and must name the unfiltered surfaces rather than implying the
	// filter is global.
	for _, unfiltered := range []string{"/v1/vwap"} {
		if !strings.Contains(note, unfiltered) {
			continue // the served note stopped naming it; nothing to mirror
		}
		if !strings.Contains(page, unfiltered) {
			t.Errorf("/v1/methodology says %s defaults to unfiltered, but %s never says so",
				unfiltered, methodologyPagePath)
		}
	}
	// The specific false sentence, kept as a tripwire: it survived four
	// months and a correction to the Go copy.
	if strings.Contains(page, "Outliers are filtered before the average") {
		t.Errorf("%s still claims outliers are filtered before the average; "+
			"/v1/methodology says /v1/vwap defaults to UNFILTERED",
			methodologyPagePath)
	}
}

// The next two tests never open the page: they hold /v1/methodology to its own contents, since page-vs-endpoint checks are
// worthless if the endpoint disagrees with itself. They check the served bytes via servedMethodology.

// ─── the remaining claims the page and the endpoint both make ───────

// TestMethodologyPage_FormulaMatchesTheServedPriceMethod pins the headline formula against
// `aggregation.price_method`. It targets the formula block, since VWAP/TWAP appear all over the page.
func TestMethodologyPage_FormulaMatchesTheServedPriceMethod(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)
	data := servedMethodology(t)

	agg, ok := data["aggregation"].(map[string]any)
	if !ok {
		t.Fatal("methodology served no aggregation object")
	}
	method, _ := agg["price_method"].(string)
	if method == "" {
		t.Fatal("methodology served no aggregation.price_method")
	}

	m := regexp.MustCompile(`<Formula>\s*([A-Za-z]+)\s*=`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s no longer opens its price section with a <Formula> naming the method — "+
			"either restore it or drop this assertion deliberately", methodologyPagePath)
	}
	if !strings.EqualFold(m[1], method) {
		t.Errorf("%s prints the formula for %s; /v1/methodology serves price_method=%q",
			methodologyPagePath, m[1], method)
	}
}

// TestMethodologyPage_VWAPEligibilityMatchesTheEndpoint: the page states eligibility as a single class (exchange), true
// only while one class contributes; admitting a second would make the formula describe a subset.
func TestMethodologyPage_VWAPEligibilityMatchesTheEndpoint(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)
	data := servedMethodology(t)

	rawClasses, ok := data["source_classes"].([]any)
	if !ok || len(rawClasses) == 0 {
		t.Fatal("methodology served no source_classes[]")
	}
	var contributors []string
	for _, raw := range rawClasses {
		c, _ := raw.(map[string]any)
		name, _ := c["name"].(string)
		if flag, _ := c["contributes_to_vwap"].(bool); flag {
			contributors = append(contributors, name)
		}
	}
	sort.Strings(contributors)
	if len(contributors) == 0 {
		t.Fatal("no served class contributes to the VWAP — the page's whole VWAP section " +
			"describes a number nothing feeds; re-derive this test's premise")
	}

	m := regexp.MustCompile(`with class =[\s\S]{0,300}?>\s*([a-z_]+)\s*<`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s no longer states which class a trade must carry to enter the VWAP — "+
			"either restore the claim or drop this assertion deliberately", methodologyPagePath)
	}
	if len(contributors) != 1 || contributors[0] != m[1] {
		t.Errorf("%s says a VWAP trade comes from class %q; /v1/methodology serves %d "+
			"contributing class(es): %s", methodologyPagePath, m[1],
			len(contributors), strings.Join(contributors, ", "))
	}
}

// TestMethodologyPage_ClassVenueNamesAgree: a venue named in a class description must be registered under THAT class, and the
// page's entry must name every venue the endpoint's entry names. The reverse is not asserted (prose describes some by
// category); descriptions name venues by registry id where the brand is ambiguous (`soroswap-router`, not "Soroswap Router").
func TestMethodologyPage_ClassVenueNamesAgree(t *testing.T) {
	page := methodologyReadRepoFile(t, methodologyPagePath)
	data := servedMethodology(t)

	rawSources, ok := data["sources"].([]any)
	if !ok || len(rawSources) == 0 {
		t.Fatal("methodology served no sources[] — there would be no venue to match")
	}
	classOf := map[string]string{}
	ids := make([]string, 0, len(rawSources))
	for _, raw := range rawSources {
		s, _ := raw.(map[string]any)
		name, _ := s["name"].(string)
		class, _ := s["class"].(string)
		if name == "" {
			t.Fatalf("served source has no name: %#v", raw)
		}
		classOf[name] = class
		ids = append(ids, name)
	}

	rawClasses, ok := data["source_classes"].([]any)
	if !ok || len(rawClasses) == 0 {
		t.Fatal("methodology served no source_classes[]")
	}

	matched := 0
	for _, raw := range rawClasses {
		c, _ := raw.(map[string]any)
		class, _ := c["name"].(string)
		desc, _ := c["description"].(string)
		if desc == "" {
			t.Errorf("/v1/methodology serves class %q with no description", class)
			continue
		}
		pageDef := methodologyPageClassDef(t, page, class)

		named := namedVenues(desc, ids)
		for _, venue := range sortedKeys(named) {
			matched++
			if got := classOf[venue]; got != class {
				t.Errorf("/v1/methodology's %q description names %q, which the same response "+
					"registers under class %q", class, venue, got)
			}
			if len(namedVenues(pageDef, []string{venue})) == 0 {
				t.Errorf("/v1/methodology's %q description names venue %q; the %q entry in %s "+
					"does not — the two copies of this paragraph have drifted",
					class, venue, class, methodologyPagePath)
			}
		}
	}
	if matched == 0 {
		t.Fatal("no served class description named a single registered venue — the scan is " +
			"broken, not the prose; every assertion above was vacuous")
	}
}

// methodologyPageClassDef returns the `def:` string the page's
// source-class DefList carries for one class term.
func methodologyPageClassDef(t *testing.T, page, term string) string {
	t.Helper()
	re := regexp.MustCompile(`term:\s*'` + regexp.QuoteMeta(term) + `'\s*,\s*def:\s*'((?:[^'\\]|\\.)*)'`)
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s has no DefList entry for source class %q — /v1/methodology serves it",
			methodologyPagePath, term)
	}
	return m[1]
}

// namedVenues returns which ids the text names as whole words, case-insensitively. Longest id first, blanking each
// match, so `soroswap-router` is not misread as the exchange-class `soroswap` (`\b` treats the hyphen as a boundary).
func namedVenues(text string, ids []string) map[string]bool {
	ordered := append([]string(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	found := map[string]bool{}
	for _, id := range ordered {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(id) + `\b`)
		if !re.MatchString(text) {
			continue
		}
		found[id] = true
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			return strings.Repeat("\x00", len(m))
		})
	}
	return found
}

// sortedKeys returns a map's keys in sorted order, so a failure
// message lists the same thing in the same order every run.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
