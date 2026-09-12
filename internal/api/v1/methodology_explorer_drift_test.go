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

// ─── /v1/methodology and the page users actually read ───────────────
//
// The API serves a machine-readable methodology document. The
// explorer's /methodology page is hand-written prose covering a
// SUPERSET of it: latency SLOs, numeric precision and the freeze
// policy have no endpoint counterpart, and the page is deliberately
// editorial about them. So the page is not generated from the
// endpoint, and it should not be.
//
// What that costs is a silent-divergence risk on the parts they DO
// both state, and it has already been paid. The endpoint's
// outlier-filter note was corrected on 2026-08-04 — /v1/vwap and
// /v1/twap default to UNFILTERED, and volume-weighting is not an
// outlier defence — while the page went on telling readers their VWAP
// was MAD-filtered. Nothing failed, because prose is not compiled.
// The page is the copy a reader trusts, so the page was the copy that
// was wrong.
//
// This file is the compile step for the overlap. It renders the real
// endpoint, reads the real page, and fails when they disagree on a
// fact both state. Prose that the endpoint does not carry stays free.

const methodologyPagePath = "web/explorer/src/app/methodology/page.tsx"

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

// servedMethodology renders GET /v1/methodology through the real
// handler and returns the decoded `data` object. Reading the endpoint
// rather than the source keeps the test honest about what a CLIENT
// sees, including anything the handler derives at request time.
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

// TestMethodology_ReferenceTitlesMatchTheADRs holds the endpoint's own
// reference list to the documents it points at.
//
// A reference list is a promise about where a link goes. ADR-0007 was
// served as "Aggregation policy + cache-key contract"; the real 0007
// is "Redis as hot-path cache + rate-limit + ephemeral state". Both
// the id and the URL were right, so nothing 404'd — the reader simply
// arrived at a different document than the one they were told to
// expect. Paraphrasing a title is what makes that possible, so the
// titles are now verbatim and this test keeps them that way.
func TestMethodology_ReferenceTitlesMatchTheADRs(t *testing.T) {
	root := methodologyRepoRoot(t)
	data := servedMethodology(t)

	refs, ok := data["references"].([]any)
	if !ok || len(refs) == 0 {
		t.Fatal("methodology served no references[]")
	}
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("reference is not an object: %#v", raw)
		}
		id, _ := ref["id"].(string)
		title, _ := ref["title"].(string)
		numeric := strings.TrimPrefix(id, "ADR-")
		actual := adrFrontMatterTitle(t, root, numeric)
		if actual == "" {
			t.Errorf("/v1/methodology references %s but docs/adr/%s-*.md does not exist", id, numeric)
			continue
		}
		if title != actual {
			t.Errorf("/v1/methodology serves %s as %q; the ADR's own title is %q — "+
				"quote the document, do not paraphrase it", id, title, actual)
		}
	}
}

// TestMethodologyPage_ADRCitationsResolve keeps the hand-written page
// from citing an ADR that does not exist. The page cites ADRs the
// endpoint does not (0003, 0009) and vice versa; that divergence is
// fine — a dangling citation is not.
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

// TestMethodologyPage_SourceClassesMatchTheEndpoint pins the one list
// the page and the endpoint both enumerate.
//
// The page tells the reader a venue carries "one of four source
// classes" and then names them. That count and those names are the
// endpoint's to define. If a class is added, renamed or dropped, the
// page must move with it — otherwise a reader is handed a taxonomy
// the API no longer uses.
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

// TestMethodologyPage_DefersOnOperatorConfiguredFacts guards the facts
// that are DEPLOYMENT CONFIGURATION rather than design.
//
// The stablecoin→fiat proxy map is set by the operator. The page used
// to hard-code eleven mappings (USDT, DAI, PYUSD, USDP, EURC, EUROC,
// EUROB, MXNe …) while the deployment served exactly one. A list like
// that cannot be kept true by editing prose — it goes stale the next
// time an operator changes config — so the page must point at the
// served field instead of restating it.
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

// TestMethodologyPage_OutlierClaimMatchesTheEndpoint is the regression
// test for the divergence that motivated this file.
//
// The endpoint says the default sigma applies to ONE endpoint and that
// the other two are unfiltered. The page said outliers are "filtered
// before the average" with no qualification — a reader sizing a trade
// off /v1/vwap would have believed a protection that is not on.
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
	for _, unfiltered := range []string{"/v1/vwap", "/v1/twap"} {
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
			"/v1/methodology says /v1/vwap and /v1/twap default to UNFILTERED",
			methodologyPagePath)
	}
}

// ─── the document against itself ────────────────────────────────────
//
// The two tests below never open the page. They hold /v1/methodology
// to its own contents, because the page-vs-endpoint checks above are
// worth nothing if the endpoint already disagrees with itself — and
// it did. Both were found by reading the served bytes rather than the
// source, which is why servedMethodology renders the real handler.

// TestMethodology_EveryServedSourceClassIsDescribed makes
// `source_classes` the complete glossary for the `class` field on
// every row of `sources`.
//
// `sources` is the whole registry — the same rows /v1/sources serves.
// `source_classes` is what a consumer looks a row's `class` up in. A
// class that appears on a row but not in the glossary is a term the
// document uses without defining, and that is exactly what shipped:
// four classes described, seven served, and eight router / lending /
// bridge venues labelled with a word nothing in the response
// explained. Nothing caught it because the count was asserted
// nowhere and repeated everywhere — the page said "one of four", the
// spec said "the four source classes", and the Go type's own godoc
// said "the four class buckets". Three surfaces agreeing with each
// other is not the same as any of them agreeing with the registry.
func TestMethodology_EveryServedSourceClassIsDescribed(t *testing.T) {
	data := servedMethodology(t)

	rawClasses, ok := data["source_classes"].([]any)
	if !ok || len(rawClasses) == 0 {
		t.Fatal("methodology served no source_classes[]")
	}
	described := map[string]bool{}
	for _, raw := range rawClasses {
		c, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("source class is not an object: %#v", raw)
		}
		name, _ := c["name"].(string)
		if name == "" {
			t.Fatalf("source class has no name: %#v", raw)
		}
		described[name] = true
	}

	rawSources, ok := data["sources"].([]any)
	if !ok || len(rawSources) == 0 {
		t.Fatal("methodology served no sources[] — there would be no class to check")
	}
	venuesByClass := map[string][]string{}
	for _, raw := range rawSources {
		s, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("source is not an object: %#v", raw)
		}
		name, _ := s["name"].(string)
		class, _ := s["class"].(string)
		if class == "" {
			t.Errorf("/v1/methodology serves source %q with no class", name)
			continue
		}
		venuesByClass[class] = append(venuesByClass[class], name)
	}
	if len(venuesByClass) == 0 {
		t.Fatal("no served source carried a class — this assertion would be vacuous")
	}

	for _, class := range sortedKeys(venuesByClass) {
		if described[class] {
			continue
		}
		venues := venuesByClass[class]
		sort.Strings(venues)
		t.Errorf("/v1/methodology serves %d source(s) of class %q (%s) but source_classes[] "+
			"never defines that class — the document uses a term it does not explain",
			len(venues), class, strings.Join(venues, ", "))
	}
}

// TestMethodology_ClassVWAPFlagMatchesItsSources holds the glossary's
// `contributes_to_vwap` against the per-source `include_in_vwap` it
// summarises.
//
// The class flags are hand-written literals in methodology.go; the
// source flags come from the registry. Nothing derives one from the
// other, so they can disagree — and "what feeds the price" is read
// off the one-line class summary far more often than off the 30-row
// table beneath it.
//
// Only the two directions that are actually wrong are asserted. A
// class marked as not contributing may not contain a contributing
// venue (the dangerous direction: the summary would be a false
// negative about what moves the price), and a class marked as
// contributing must contain at least one (or the claim is empty).
// A contributing class holding SOME non-contributing venue is
// legitimate — a newly added exchange can sit in the registry with
// include_in_vwap=false until it is trusted — so that is not an
// error.
func TestMethodology_ClassVWAPFlagMatchesItsSources(t *testing.T) {
	data := servedMethodology(t)

	rawClasses, ok := data["source_classes"].([]any)
	if !ok || len(rawClasses) == 0 {
		t.Fatal("methodology served no source_classes[]")
	}
	contributes := map[string]bool{}
	for _, raw := range rawClasses {
		c, _ := raw.(map[string]any)
		name, _ := c["name"].(string)
		flag, ok := c["contributes_to_vwap"].(bool)
		if name == "" || !ok {
			t.Fatalf("source class carries no name/contributes_to_vwap: %#v", raw)
		}
		contributes[name] = flag
	}

	rawSources, ok := data["sources"].([]any)
	if !ok || len(rawSources) == 0 {
		t.Fatal("methodology served no sources[] — there would be no flag to check")
	}
	contributingVenues := map[string][]string{}
	checked := 0
	for _, raw := range rawSources {
		s, _ := raw.(map[string]any)
		name, _ := s["name"].(string)
		class, _ := s["class"].(string)
		included, ok := s["include_in_vwap"].(bool)
		if !ok {
			t.Errorf("/v1/methodology serves source %q with no include_in_vwap", name)
			continue
		}
		checked++
		if included {
			contributingVenues[class] = append(contributingVenues[class], name)
		}
	}
	if checked == 0 {
		t.Fatal("no served source carried include_in_vwap — this assertion would be vacuous")
	}

	for _, class := range sortedKeys(contributes) {
		venues := contributingVenues[class]
		sort.Strings(venues)
		switch {
		case !contributes[class] && len(venues) > 0:
			t.Errorf("/v1/methodology says class %q does not contribute to the VWAP, but serves "+
				"%d venue(s) of that class with include_in_vwap=true (%s)",
				class, len(venues), strings.Join(venues, ", "))
		case contributes[class] && len(venues) == 0:
			t.Errorf("/v1/methodology says class %q contributes to the VWAP, but no served venue "+
				"of that class has include_in_vwap=true — the claim is empty", class)
		}
	}
}

// ─── the remaining claims the page and the endpoint both make ───────

// TestMethodologyPage_FormulaMatchesTheServedPriceMethod pins the
// headline formula.
//
// `aggregation.price_method` is the API's statement of how the served
// price is derived; the page prints a formula and names it. A plain
// substring search for the method would be worthless here — the page
// mentions VWAP and TWAP many times over — so the assertion is
// against the formula block specifically, which is the page's actual
// claim about what the number is.
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

// TestMethodologyPage_VWAPEligibilityMatchesTheEndpoint holds the
// page's eligibility rule to the served contributor set.
//
// The page states the rule as a single class ("each trade i is from a
// source with class = exchange"). That phrasing is only true while
// exactly one class contributes, which is the endpoint's to decide.
// Admit a second contributing class and the page's formula silently
// starts describing a subset of the number it claims to define.
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

// TestMethodologyPage_ClassVenueNamesAgree holds the two copies of
// the same paragraph together.
//
// Each class is described twice in near-identical prose — once in
// methodology.go, once in the page's DefList — and both name venues.
// Two assertions, in the two directions that can be checked without
// guessing at prose:
//
//  1. a venue named in a class's served description must be
//     registered under THAT class in the same response, so the
//     document cannot describe a venue into the wrong bucket;
//  2. the page's entry for a class must name every venue the
//     endpoint's entry for it names, so the endpoint's copy cannot
//     gain a venue the page's copy never hears about.
//
// The reverse of (2) — every registered venue must be named — is
// deliberately NOT asserted. Both copies describe some venues by
// category rather than by name ("FX vendors", "canonical fiat
// rates"), and forcing an exhaustive list into prose would make the
// text worse and the gate noisier. The consequence is real and worth
// stating: a venue can join the registry without either copy
// mentioning it, and today several have (cryptocompare, sushiswap_v3,
// massive, exchangeratesapi, ecb, blend_emitter are all registered
// and named in neither copy). What is gated is that nothing NAMED is
// wrong.
//
// Because the scan matches registry ids, a description should name a
// venue by its id wherever the brand name is ambiguous — "Soroswap
// Router" would be read as the exchange-class `soroswap`, so the
// router class names `soroswap-router` instead.
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

// namedVenues returns which of `ids` the text names, as whole words
// and case-insensitively.
//
// Longest id first, blanking each match as it is claimed, so that
// `soroswap-router` takes its own text before a bare `soroswap` can
// be read out of the middle of it — `\b` treats the hyphen as a
// boundary, so without this the router class would look like it was
// describing an exchange-class venue.
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
