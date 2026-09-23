package v1

import (
	"os"
	"strings"
	"testing"
)

// TestNoDangling1219Reference guards RSWP-118: "#1219" was never a real
// PR/issue in this repo when the stablecoin-fiat-proxy-fallback comments
// below were written, and GitHub has since assigned #1219 to a real but
// unrelated open issue (oracle_unparsed_metric_test incrementing the
// dropped-row counter itself instead of exercising the reader). A reader
// following the citation now lands on that unrelated issue instead of a
// clean 404 -- a more confusing failure than the dangling reference it
// replaces. See docs/adr/0026-stablecoin-fiat-proxy-late-binding.md's
// Amendment for the ADR-side citation, which is left intact as historical
// record rather than stripped.
func TestNoDangling1219Reference(t *testing.T) {
	for _, f := range []string{
		"oracle_sep40.go",
		"oracle_sep40_test.go",
		"twap_test.go",
		"ohlc_test.go",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "#1219") {
			t.Errorf("%s still cites #1219; it now resolves to an unrelated open issue, not the stablecoin-fiat-proxy fallback family", f)
		}
	}
}
