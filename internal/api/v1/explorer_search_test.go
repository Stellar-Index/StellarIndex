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

func TestExplorer_Search_EmptyQuery400(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/search?q="); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
