package explorer

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The account_movements' address/counterparty columns are keyed on
// G-strkey equality (baseAccountAddress resolves every muxed classic
// counterparty to its base G before write), so a query using the M-strkey
// an exchange hands out as a deposit address must resolve to the same G
// rather than 400ing.
func TestParseAccountStrkey_ResolvesMuxedToBaseG(t *testing.T) {
	h := newProbeHandler(nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+searchTestMuxedM+"/movements", nil)
	r.SetPathValue("g_strkey", searchTestMuxedM)
	rec := httptest.NewRecorder()

	got, ok := h.parseAccountStrkey(rec, r)

	if !ok {
		t.Fatalf("parseAccountStrkey rejected muxed address %s (status %d), want resolved", searchTestMuxedM, rec.Code)
	}
	if got != searchTestMuxedG {
		t.Fatalf("parseAccountStrkey(%s) = %q, want base G-address %q", searchTestMuxedM, got, searchTestMuxedG)
	}
}
