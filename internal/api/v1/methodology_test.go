package v1_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// TestMethodology_BaselineShape pins the wire shape's required
// keys + version. Adding optional fields is fine; flipping the
// version string or removing required fields is a breaking
// change and must be coordinated with pkg/client + the explorer.
func TestMethodology_BaselineShape(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var env struct {
		Data v1.Methodology `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Version == "" {
		t.Error("version is empty")
	}
	if env.Data.Aggregation.PriceMethod != "vwap" {
		t.Errorf("price_method = %q, want vwap", env.Data.Aggregation.PriceMethod)
	}
	if env.Data.Aggregation.ClosedBucketWindowSeconds <= 0 {
		t.Errorf("closed_bucket_window_seconds = %d, want > 0", env.Data.Aggregation.ClosedBucketWindowSeconds)
	}
	// Every class the registry can stamp on a source must appear,
	// exactly one of which (exchange) contributes_to_vwap=true.
	//
	// Derived from the registry rather than listed literally here: a
	// hard-coded four in this very test is part of how the served
	// document came to describe four classes while `sources` served
	// seven. Four surfaces agreed with each other and none of them
	// with external.Registry.
	wantClasses := map[string]bool{}
	for _, md := range external.Registry {
		wantClasses[string(md.Class)] = md.Class == external.ClassExchange
	}
	if len(wantClasses) == 0 {
		t.Fatal("external.Registry stamps no classes — this assertion would be vacuous")
	}
	if len(env.Data.SourceClasses) != len(wantClasses) {
		t.Fatalf("source_classes = %d entries, want %d (%s)",
			len(env.Data.SourceClasses), len(wantClasses),
			strings.Join(sortedKeys(wantClasses), ", "))
	}
	for _, sc := range env.Data.SourceClasses {
		want, ok := wantClasses[sc.Name]
		if !ok {
			t.Errorf("unexpected class %q", sc.Name)
			continue
		}
		if sc.ContributesToVWAP != want {
			t.Errorf("class %q contributes_to_vwap = %v, want %v", sc.Name, sc.ContributesToVWAP, want)
		}
		if sc.Description == "" {
			t.Errorf("class %q has empty description", sc.Name)
		}
	}

	// Sources must come from external.Registry — the live ingest
	// venues. Pin the must-have rows; the exact length grows as
	// new sources land, so we don't pin it.
	gotSources := map[string]v1.MethodologySource{}
	for _, s := range env.Data.Sources {
		gotSources[s.Name] = s
	}
	for _, name := range []string{"sdex", "soroswap", "binance", "coinbase", "reflector-dex"} {
		if _, ok := gotSources[name]; !ok {
			t.Errorf("expected source %q in Methodology.Sources", name)
		}
	}

	// References must include the four ADRs that govern the
	// served-price contract.
	gotADRs := map[string]bool{}
	for _, ref := range env.Data.References {
		gotADRs[ref.ID] = true
	}
	for _, want := range []string{"ADR-0007", "ADR-0015", "ADR-0019"} {
		if !gotADRs[want] {
			t.Errorf("expected reference %q in Methodology.References", want)
		}
	}
}

// TestMethodology_SourcesCarryOnChain pins that /v1/methodology's
// per-source rows carry `on_chain`, matching /v1/sources.
// A consumer reading class=exchange off /v1/methodology
// cannot otherwise tell a dispatcher-path Stellar venue (sdex,
// soroswap) from an off-chain reference feed (binance, coinbase).
func TestMethodology_SourcesCarryOnChain(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Methodology `json:"data"`
	}
	mustDecode(t, resp, &env)

	gotSources := map[string]v1.MethodologySource{}
	for _, s := range env.Data.Sources {
		gotSources[s.Name] = s
	}

	onChain, ok := gotSources["sdex"]
	if !ok {
		t.Fatal("expected source \"sdex\" in Methodology.Sources")
	}
	if !onChain.OnChain {
		t.Error("sdex OnChain = false, want true")
	}

	offChain, ok := gotSources["binance"]
	if !ok {
		t.Fatal("expected source \"binance\" in Methodology.Sources")
	}
	if offChain.OnChain {
		t.Errorf("binance OnChain = true, want false")
	}
}

// TestMethodology_SurfacesStablecoinPegConfig confirms operator-
// declared USD pegs round-trip through the response. Empty when
// the operator hasn't declared any.
func TestMethodology_SurfacesStablecoinPegConfig(t *testing.T) {
	usdc, err := canonical.NewClassicAsset(
		"USDC",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.Methodology `json:"data"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data.Aggregation.StablecoinFiatProxy) != 1 {
		t.Fatalf("StablecoinFiatProxy = %d entries, want 1", len(env.Data.Aggregation.StablecoinFiatProxy))
	}
	got := env.Data.Aggregation.StablecoinFiatProxy[0]
	if got.AssetID != usdc.String() {
		t.Errorf("AssetID = %q, want %q", got.AssetID, usdc.String())
	}
	if got.PegsTo != "fiat:USD" {
		t.Errorf("PegsTo = %q, want fiat:USD", got.PegsTo)
	}
}

// TestMethodology_EmptyStablecoinList is the no-pegs deployment
// case — the field is present (so consumers don't crash on
// missing key lookups) but empty.
func TestMethodology_EmptyStablecoinList(t *testing.T) {
	srv := v1.New(v1.Options{}) // no USDPeggedClassics
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.Methodology `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Aggregation.StablecoinFiatProxy == nil {
		t.Error("StablecoinFiatProxy = nil, want empty slice (consumers index safely)")
	}
	if len(env.Data.Aggregation.StablecoinFiatProxy) != 0 {
		t.Errorf("StablecoinFiatProxy = %d entries, want 0", len(env.Data.Aggregation.StablecoinFiatProxy))
	}
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
