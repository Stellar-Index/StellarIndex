package v1

import (
	"net/http/httptest"
	"testing"
)

// TestHeavyReadCosts: an uncached read the client opts into is priced above the one pre-charged token.
func TestHeavyReadCosts(t *testing.T) {
	if got := sep41TransfersCost("", ""); got != 1 {
		t.Errorf("unfiltered transfers cost = %d, want 1", got)
	}
	for _, c := range [][2]string{{"GA", ""}, {"", "GB"}, {"GA", "GB"}} {
		if got := sep41TransfersCost(c[0], c[1]); got != 1+sep41TransfersAddressSurcharge {
			t.Errorf("transfers cost(from=%q,to=%q) = %d, want %d", c[0], c[1], got, 1+sep41TransfersAddressSurcharge)
		}
	}
	for q, want := range map[string]bool{"": false, "sparkline": false, "inception": true, "sparkline, inception": true} {
		r := httptest.NewRequest("GET", "/v1/markets", nil)
		v := r.URL.Query()
		v.Set("include", q)
		r.URL.RawQuery = v.Encode()
		if _, got := marketsIncludes(r); got != want {
			t.Errorf("include=%q inception = %v, want %v", q, got, want)
		}
	}
}
