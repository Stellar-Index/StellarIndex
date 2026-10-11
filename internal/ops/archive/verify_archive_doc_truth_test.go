package archive

import (
	"strings"
	"testing"
)

// TestDoc_TierDDescribedAsPeerOnly_RLT304: Tier D is pure peer-vs-peer (no local hash; R2/R3 have no mirror), so docs must not claim a local comparison.
func TestDoc_TierDDescribedAsPeerOnly_RLT304(t *testing.T) {
	t.Parallel()

	topology := readRepoFile(t, "docs/architecture/ha-plan.md")
	if strings.Contains(topology, "against the local view") || strings.Contains(topology, "against the local chain") {
		t.Error("ha-plan.md: Tier D is described as comparing against a local ledger " +
			"hash, but verifyArchivePeers (verify_archive.go) never reads one — it only cross-compares " +
			"peers against each other (RLT-304)")
	}
}
