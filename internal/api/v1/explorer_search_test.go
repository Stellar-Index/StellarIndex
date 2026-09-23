package v1_test

import (
	"net/http"
	"net/url"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

func TestExplorer_Search_Classifies(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	cases := []struct {
		q    string
		kind string
	}{
		{testTxHash, "transaction"},
		{"63017000", "ledger"},
		{"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "account"},
		{"CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP", "contract"},
		{"native", "asset"},
		{"fiat:USD", "asset"},
		{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "asset"},
		{"not a real thing!!", "unknown"},
	}
	for _, tc := range cases {
		resp := mustGet(t, base+"/v1/search?q="+url.QueryEscape(tc.q))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("q=%q: status = %d", tc.q, resp.StatusCode)
			continue
		}
		var body struct {
			Data v1.SearchResultView `json:"data"`
		}
		mustDecode(t, resp, &body)
		if body.Data.Kind != tc.kind {
			t.Errorf("q=%q: kind = %q, want %q (href=%q)", tc.q, body.Data.Kind, tc.kind, body.Data.Href)
		}
	}
}

// TestExplorer_Search_AccountNotClaimedSupported is T174: classifySearch
// routes every valid-format G-address to /v1/issuers/{g}, but that endpoint
// only serves accounts that are actually issuers — most G-addresses aren't,
// and hit a 404 there. The classifier has no lake read (it's pure strkey
// shape matching), so it can't know whether this address is an issuer, and
// must not claim Supported=true for a lookup it hasn't verified resolves.
func TestExplorer_Search_AccountNotClaimedSupported(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	q := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	resp := mustGet(t, base+"/v1/search?q="+url.QueryEscape(q))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.SearchResultView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.Kind != "account" {
		t.Fatalf("kind = %q, want %q", body.Data.Kind, "account")
	}
	if body.Data.Supported {
		t.Errorf("Supported = true, want false: classifier cannot verify %q is an issuer before claiming its /v1/issuers/ href resolves", q)
	}
}

// TestExplorer_Search_MuxedResolvesToAccount: an exchange deposit address is
// a muxed M-strkey; search must resolve it to the underlying G (SEP-23 vector)
// and echo the M as the query, not fall through to "unknown".
func TestExplorer_Search_MuxedResolvesToAccount(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	const m = "MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK"
	const g = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	resp := mustGet(t, base+"/v1/search?q="+url.QueryEscape(m))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.SearchResultView `json:"data"`
	}
	mustDecode(t, resp, &body)
	d := body.Data
	if d.Kind != "account" || d.Canonical != g || d.Query != m || d.Href != "/v1/issuers/"+g {
		t.Fatalf("got kind=%q canonical=%q query=%q href=%q; want account/%s/%s//v1/issuers/%s",
			d.Kind, d.Canonical, d.Query, d.Href, g, m, g)
	}
	if d.Supported {
		t.Errorf("Supported = true, want false (same unverified issuer href as a G query)")
	}
}

func TestExplorer_Search_EmptyQuery400(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/search?q="); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
