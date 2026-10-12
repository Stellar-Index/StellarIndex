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

// TestMethodology_BaselineShape pins required keys and version. Optional fields are fine; changing the version or
// removing required fields is breaking and must be coordinated with pkg/client and the explorer.
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
	// Every class the registry can stamp on a source must appear, exactly one (exchange) with contributes_to_vwap=true.
	// Derived from the registry, since a hard-coded count lets the document and registry drift apart.
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

// TestMethodology_SourcesCarryOnChain pins `on_chain` on per-source rows, matching /v1/sources; otherwise class=exchange
// cannot distinguish a Stellar venue (sdex) from an off-chain feed (binance).
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

// TestMethodology_ReferenceTitlesMatchTheADRs holds the endpoint's reference titles verbatim to the documents they point
// at; a paraphrased title with a correct id and URL never 404s but misdescribes the target.
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

// TestMethodology_EveryServedSourceClassIsDescribed makes `source_classes` the complete glossary for every `class` on
// `sources` (the same rows /v1/sources serves); a class missing from it is a term used undefined.
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

// TestMethodology_ClassVWAPFlagMatchesItsSources holds the hand-written class `contributes_to_vwap` against registry
// `include_in_vwap`. A non-contributing class may hold no contributing venue; a contributing class needs at least one
// (some non-contributing is fine).
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
