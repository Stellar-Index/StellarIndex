package explorer

import (
	"strings"
	"testing"
)

// A fee bump ingested before the fee-bump layer was captured is indexed under its outer hash only, so a miss on an
// inner hash is not proof of absence: every not-found answer says so, whatever the tip's freshness.
func TestTxNotFoundDetail_DisclosesUnindexedInnerHashes(t *testing.T) {
	for _, c := range []struct{ stale, tipOK bool }{{false, true}, {true, true}, {true, false}} {
		got := txNotFoundDetail(64_000_000, c.stale, c.tipOK)
		if !strings.Contains(got, "inner hash") || !strings.Contains(got, "outer (fee-bump) hash") {
			t.Fatalf("stale=%v tipOK=%v: detail %q does not disclose unindexed inner hashes", c.stale, c.tipOK, got)
		}
	}
}
