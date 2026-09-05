package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
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
}

func TestRWAListingPipelineMatchesTheAssetsListing(t *testing.T) {
	listing := serverCallsIn(t, "assets.go", "handleAssetListFromAssets")
	rwa := serverCallsIn(t, "rwa.go", "rwaListingRows")
	if len(listing) == 0 || len(rwa) == 0 {
		t.Fatalf("parsed no calls (listing=%v rwa=%v) — the guard is not reading what it thinks", listing, rwa)
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
		t.Errorf("rwaListingRows omits %v from the /v1/assets pipeline (or runs it out of order).\n"+
			"required: %v\nrwa:      %v\n"+
			"Add the call, or record the divergence in listingOnlyPipelineCalls with its reason.",
			missing, want, rwa)
	}

	for call, reason := range listingOnlyPipelineCalls {
		if !slices.Contains(listing, call) {
			t.Errorf("listingOnlyPipelineCalls entry %q is stale — handleAssetListFromAssets no longer calls it (reason recorded: %s)",
				call, reason)
		}
	}
}

// serverCallsIn returns, in source order, the names of the `s.<name>(…)`
// method calls made in one function of one file in this package.
func serverCallsIn(t *testing.T, file, fn string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
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
