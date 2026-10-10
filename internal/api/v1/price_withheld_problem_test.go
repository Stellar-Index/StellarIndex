package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The withheld 404 must carry the reason as a member, so clients branch
// on it rather than string-matching the title.
func TestWritePriceWithheldProblem_CarriesReasonMember(t *testing.T) {
	asset := canonical.NativeAsset()
	quote := defaultPriceQuote
	cases := map[PriceWithheldReason]string{
		PriceWithheldSubstance:         "substance",
		PriceWithheldScamIssuer:        "scam_issuer",
		PriceWithheldUpstreamLeg:       "upstream_leg",
		PriceWithheldUnattributed:      "unattributed",
		PriceWithheldManipulationGuard: "manipulation_guard",
		PriceWithheldFXLeg:             "fx_leg_unavailable",
		"":                             "unattributed",
	}
	for in, want := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil)
		writePriceWithheldProblem(rec, req, asset, quote, in)

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("reason %q: decode: %v", in, err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("reason %q: status %d, want 404", in, rec.Code)
		}
		if got := body["reason"]; got != want {
			t.Errorf("reason %q: body reason = %v, want %q", in, got, want)
		}
	}
}
