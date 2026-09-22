package v1

import (
	"os"
	"strings"
	"testing"
)

// TestDEXTVLTotal_NoStaleIssueCitation guards RLT-429: issue #338 (D4 DEX
// TVL surface) closed 2026-08-29, and dex_tvl_total.go's comments and the
// classic-liquidity-pools exclusion Reason must not cite it as if it were
// still open tracking work.
func TestDEXTVLTotal_NoStaleIssueCitation(t *testing.T) {
	src, err := os.ReadFile("dex_tvl_total.go")
	if err != nil {
		t.Fatalf("read dex_tvl_total.go: %v", err)
	}
	if strings.Contains(string(src), "#338") {
		t.Fatal("dex_tvl_total.go still cites closed issue #338 (RLT-429)")
	}
}
