package clickhouse

import (
	"strings"
	"testing"
)

// TestTrustlineAssetsPageQuery_PoolExclusionIsExact is the fast guard for the pool-share exclusion; the
// executing proof is test/integration/clickhouse_entries_test.go. `NOT startsWith(asset, 'pool')` would drop
// valid credit assets like "poolX-GISSUER...", so only the two real pool-share spellings may be excluded.
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
