package archive

import (
	"strings"
	"testing"
)

// RLT-304: "Local Tier D" was documented as comparing sampled
// checkpoint hashes against a locally-held ledger hash.
// verifyArchivePeers never reads one — it fetches each peer's
// history-XXXXXXXX.json and cross-compares the rest against one peer
// picked as reference, pure peer-vs-peer consensus. R2/R3, the
// regions Tier D exists for, have no local /srv/history-archive
// mirror to hold such a hash in the first place (that's Tier B,
// R1-only). A doc that claims a local comparison promises a security
// property Tier D doesn't have.
//
// ha-plan.md is a living architecture doc, corrected in place.
// ADR-0016's Decision and Invariant state the peer-only mechanism.
func TestDoc_TierDDescribedAsPeerOnly_RLT304(t *testing.T) {
	t.Parallel()

	topology := readRepoFile(t, "docs/architecture/ha-plan.md")
	if strings.Contains(topology, "against the local view") || strings.Contains(topology, "against the local chain") {
		t.Error("ha-plan.md: Tier D is described as comparing against a local ledger " +
			"hash, but verifyArchivePeers (verify_archive.go) never reads one — it only cross-compares " +
			"peers against each other (RLT-304)")
	}
}
