package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// The RWA surface reuses the /v1/assets post-query pipeline instead of
// computing its own valuations, which is the only reason it cannot
// publish a figure /v1/assets would withhold. That reuse is a
// hand-copied call sequence in rwaListingRows, so it can silently fall
// behind: a gate added to handleAssetListFromAssets would leave the RWA
// page serving the ungated figure, and nothing else in the suite would
// notice.
//
// This guard reads both functions and requires the RWA path to make
// every s.* call the listing path makes, in the same relative order.
// It is source-level on purpose — the drift it exists to catch is a
// missing line, which no behavioural test can see without a fixture for
// the specific gate that went missing.
//
// Calls the listing path makes that are genuinely not part of the
// valuation pipeline are named in listingOnlyPipelineCalls with the
// reason. That list is the conscious-decision register: growing it is
// how a deliberate divergence gets recorded, and the test fails if an
// entry there stops appearing in the listing path at all.
var listingOnlyPipelineCalls = map[string]string{
	// Cursor validation and the store read belong to a paged listing;
	// the RWA set is unpaged and reads per member issuer.
	"listAssetsExtAt": "the RWA path reads per member issuer, not one keyset page",
	// The sparkline is an opt-in query parameter on /v1/assets. The RWA
	// surface takes no query parameters and serves no series.
	"attachSparkline7dIfRequested": "no include= parameter on this surface, and no price series is served",
	// The logo overlay writes AssetDetail.Image, which this surface has
	// no field for. Running it would be a no-op with a per-page scan
	// attached. It is a DISPLAY fill, never a gate — recording it here
	// costs nothing that guards a number.
	"fillImagesFromSep1": "this surface serves no image field, so the overlay would write nothing",
	// The listing-priced valuation arm. Recorded as a divergence rather
	// than copied, and the reason is that the RWA surface ALREADY makes
	// this exact claim through its own, older path: rwaApplyContractReference
	// publishes a `reference` block carrying provenance
	// listing_platform_price, from the same directory, with a summary
	// basis that names the mixture in prose. Running this arm here would
	// attach a second block making the same statement under a different
	// field name, and the RWAAsset projection has no field to receive it
	// — so it would cost a directory read and a lake read per rebuild to
	// write something nothing serves.
	//
	// It is NOT a gate, which is the property this register exists to
	// protect: it can only ADD a figure to a row that has none, never
	// withhold one, so an arm missing it cannot publish anything
	// /v1/assets would refuse.
	"applyListingValuations": "the RWA surface publishes this claim already, through its own reference block (rwa_reference.go); it is additive, never a gate",
}

func TestRWAListingPipelineMatchesTheAssetsListing(t *testing.T) {
	assertRWAPipelineMatchesListing(t, "rwa.go", "rwaListingRows")
}

// TestRWAContractPipelineMatchesTheAssetsListing holds the CONTRACT arm
// to the same contract as the classic one.
//
// The contract arm reads its own rows — the listing spine cannot express
// the volume-gate-free read it needs — so it is a second hand-copied
// call sequence, with the same failure mode: a gate added to
// handleAssetListFromAssets would leave contract rows serving the
// ungated figure, and nothing behavioural would notice.
//
// Three of the pipeline's steps are no-ops on a contract row (no code,
// no G-issuer). They are still required to be CALLED. A no-op costs
// nothing, and the alternative — letting each arm skip the steps its
// author judged irrelevant — is how the two arms start disagreeing about
// which gates apply to a valuation.
func TestRWAContractPipelineMatchesTheAssetsListing(t *testing.T) {
	assertRWAPipelineMatchesListing(t, "rwa_contracts.go", "rwaContractListingRows")
}

func assertRWAPipelineMatchesListing(t *testing.T, file, fn string) {
	t.Helper()
	listing := serverCallsIn(t, "assets.go", "handleAssetListFromAssets")
	rwa := serverCallsIn(t, file, fn)
	if len(listing) == 0 || len(rwa) == 0 {
		t.Fatalf("parsed no calls (listing=%v %s=%v) — the guard is not reading what it thinks", listing, fn, rwa)
	}

	want := make([]string, 0, len(listing))
	for _, c := range listing {
		if _, skip := listingOnlyPipelineCalls[c]; skip {
			continue
		}
		want = append(want, c)
	}

	// Every required call present, in the listing's relative order.
	i := 0
	var missing []string
	for _, c := range want {
		j := slices.Index(rwa[i:], c)
		if j < 0 {
			missing = append(missing, c)
			continue
		}
		i += j + 1
	}
	if len(missing) > 0 {
		t.Errorf("%s omits %v from the /v1/assets pipeline (or runs it out of order).\n"+
			"required: %v\nrwa:      %v\n"+
			"Add the call, or record the divergence in listingOnlyPipelineCalls with its reason.",
			fn, missing, want, rwa)
	}

	for call, reason := range listingOnlyPipelineCalls {
		if !slices.Contains(listing, call) {
			t.Errorf("listingOnlyPipelineCalls entry %q is stale — handleAssetListFromAssets no longer calls it (reason recorded: %s)",
				call, reason)
		}
	}
}

// TestDirectoryTagsPrecedeTheListingValuationArm — RLT-313 / RLT-337.
//
// applyListingValuations refuses a row whose issuer carries a scam-class
// directory tag, and it reads that tag from AssetDetail.IssuerDirectoryTags
// — a field nothing populates except fillIssuerDirectoryTags. So the
// refusal is only alive where the directory call runs FIRST. It did not: on
// both unified listing phases the valuation arm ran three lines earlier, the
// refusal saw an empty slice on every row, and a directory-flagged issuer
// published `listing_reference.price_usd` and `listing_valuation.value_usd`
// underneath the `price_usd: null` that same tag had just produced.
//
// Source-level, like the RWA guard above and for the same reason: the defect
// is an ORDER, which no single-function behavioural test can see — each
// function is correct in isolation. Checked over every function in the
// package that makes both calls, so a new listing phase inherits the rule
// instead of re-discovering it.
func TestDirectoryTagsPrecedeTheListingValuationArm(t *testing.T) {
	const (
		tags      = "fillIssuerDirectoryTags"
		valuation = "applyListingValuations"
	)
	checked := 0
	for _, file := range packageGoFiles(t) {
		parsed := parseGoFile(t, file)
		for _, fn := range funcNamesIn(parsed) {
			calls := serverCallsInFile(parsed, fn)
			tagAt := slices.Index(calls, tags)
			valAt := slices.Index(calls, valuation)
			if tagAt < 0 || valAt < 0 {
				continue
			}
			checked++
			if tagAt > valAt {
				t.Errorf("%s:%s calls %s at step %d but %s only at step %d — "+
					"the valuation arm reads the tags that call writes, so its scam refusal is dead.\n"+
					"calls: %v", file, fn, valuation, valAt, tags, tagAt, calls)
			}
		}
	}
	// A guard that silently matched nothing would report a clean pass over an
	// empty set for the rest of this repository's life.
	if checked < 3 {
		t.Fatalf("the guard examined %d function(s) that make both calls; the two unified "+
			"listing phases and the catalogue-twin fan-out are known to. Either a phase lost "+
			"its directory call or this guard stopped reading the package", checked)
	}
}

// packageGoFiles lists the non-test .go files of this package.
func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// parseGoFile parses one file of this package. Callers walking many
// functions parse each file once: re-parsing per function made the
// package-wide order guards quadratic under -race.
func parseGoFile(t *testing.T, file string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return parsed
}

// funcNamesIn lists every function and method declared in one parsed file.
func funcNamesIn(parsed *ast.File) []string {
	var out []string
	for _, decl := range parsed.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
			out = append(out, fd.Name.Name)
		}
	}
	return out
}

// serverCallsIn returns, in source order, the names of the `s.<name>(…)`
// method calls made in one function of one file in this package.
func serverCallsIn(t *testing.T, file, fn string) []string {
	t.Helper()
	return serverCallsInFile(parseGoFile(t, file), fn)
}

// serverCallsInFile is serverCallsIn over an already-parsed file.
func serverCallsInFile(parsed *ast.File, fn string) []string {
	var out []string
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "s" {
				return true
			}
			// Field reads through s (s.logger.Warn, s.assetsReader.X)
			// are not pipeline steps; only direct s.method(…) calls are.
			if strings.Contains(sel.Sel.Name, ".") {
				return true
			}
			out = append(out, sel.Sel.Name)
			return true
		})
	}
	return out
}
