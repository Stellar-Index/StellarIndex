package explorer

import "testing"

// Fixtures shared with internal/canonical/strkey_test.go.
const (
	searchTestAccountG  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" // USDC issuer
	searchTestContractC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // XLM SAC
	searchTestMuxedM    = "MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK"
	searchTestMuxedG    = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
)

// GH-984: /v1/search must route account and contract hits to the canonical
// detail endpoints mounted in server.go (/v1/accounts/{g_strkey},
// /v1/contracts/{contract_id}), not the stale /v1/issuers/{g} and
// /v1/contracts/{c}/transfers hrefs, and must not claim a build-in-progress
// note for endpoints that have since shipped.
func TestClassifySearchRoutesToMountedEndpoints(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantKind string
		wantHref string
	}{
		{"account", searchTestAccountG, "account", "/v1/accounts/" + searchTestAccountG},
		{"contract", searchTestContractC, "contract", "/v1/contracts/" + searchTestContractC},
		{"muxed", searchTestMuxedM, "account", "/v1/accounts/" + searchTestMuxedG},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySearch(tc.query)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Href != tc.wantHref {
				t.Fatalf("Href = %q, want %q", got.Href, tc.wantHref)
			}
			if !got.Supported {
				t.Fatalf("Supported = false, want true: mounted endpoint should be marked supported")
			}
			if got.Note != "" && (got.Note == "full account view isn't built yet; this may be an issuer — check the linked issuer view, which 404s if it isn't" ||
				got.Note == "muxed address resolved to its underlying account "+searchTestMuxedG+"; full account view isn't built yet — check the linked issuer view, which 404s if it isn't") {
				t.Fatalf("Note = %q is a stale not-built-yet claim for a shipped endpoint", got.Note)
			}
		})
	}
}
