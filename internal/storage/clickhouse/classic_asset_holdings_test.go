package clickhouse

import (
	"strings"
	"testing"
)

// TestTrustlineAssetsPageQuery_PoolExclusionIsExact is the fast default-suite
// guard for CA2-A14-correct-5 (the executing proof against real ClickHouse is
// test/integration/trustline_pool_prefix_test.go).
//
// The pre-fix predicate `NOT startsWith(asset, 'pool')` matches any credit
// asset string that merely starts with the substring "pool" (e.g.
// "poolX-GISSUER...", a valid case-sensitive Stellar asset code), silently
// dropping it from the registry walk alongside the two real pool-share
// spellings TrustLineAssetID emits.
func TestTrustlineAssetsPageQuery_PoolExclusionIsExact(t *testing.T) {
	t.Parallel()

	if strings.Contains(trustlineAssetsPageQuery, "startsWith(asset, 'pool')") {
		t.Fatalf("query still uses a prefix match on 'pool', which also excludes real credit assets whose code starts with \"pool\" (e.g. poolX): %s", trustlineAssetsPageQuery)
	}
	if !strings.Contains(trustlineAssetsPageQuery, "asset NOT LIKE 'pool:%'") {
		t.Errorf("query must exclude the pool:<hex> spelling exactly: %s", trustlineAssetsPageQuery)
	}
	if !strings.Contains(trustlineAssetsPageQuery, "asset != 'pool'") {
		t.Errorf("query must exclude the bare 'pool' spelling exactly: %s", trustlineAssetsPageQuery)
	}
}
