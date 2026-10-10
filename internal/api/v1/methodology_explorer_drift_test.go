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
// outlier-filter note says — /v1/vwap and
// /v1/twap default to UNFILTERED, and volume-weighting is not an
// outlier defence — while the page must not keep telling readers their VWAP
// was MAD-filtered. Nothing failed, because prose is not compiled.
// The page is the copy a reader trusts, so the page was the copy that
// was wrong.
//
// This file is the compile step for the overlap. It renders the real
// endpoint, reads the real page, and fails when they disagree on a
// fact both state. Prose that the endpoint does not carry stays free.

const methodologyPagePath = "web/explorer/src/app/methodology/page.tsx"

// anomaliesPagePath is the /anomalies page's REASONS list — the same
// drift risk as /methodology, for the freeze-reason vocabulary
// instead of the pricing vocabulary.
const anomaliesPagePath = "web/explorer/src/app/anomalies/page.tsx"

// freezeEventsSourcePath is mapFreezeReason's home. It is unexported
// (package timescale), so this test greps its source text for the
// string literals it actually returns rather than calling it — the
// only way an external test package can pin an unexported function's
// range.
const freezeEventsSourcePath = "internal/storage/timescale/freeze_events.go"

// TestAnomaliesPage_ReasonsAreReachable pins that the /anomalies
// page's REASONS list must not include `single_source` and `manual`,
// neither of which mapFreezeReason ever returns — a reader could look
// for a freeze reason on the timeline that the automated mapper is
// structurally incapable of writing. Every name the page lists as a
// freeze reason must be one of mapFreezeReason's real return values.
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
// the API does not use.
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

// ─── the document against itself ────────────────────────────────────
//
// The two tests below never open the page. They hold /v1/methodology
// to its own contents, because the page-vs-endpoint checks above are
// worth nothing if the endpoint already disagrees with itself — and
// it did. Both were found by reading the served bytes rather than the
// source, which is why servedMethodology renders the real handler.

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
