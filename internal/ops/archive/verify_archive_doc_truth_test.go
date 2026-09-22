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
// multi-region-topology.md is a living architecture doc, corrected
// in place. docs/adr/0016 is an immutable decision record — its
// original "Decision" text is left as-is per repo convention (see
// its own raidz2 amendment); the correction is an appended amendment
// referencing this finding, so we check for that instead of absence
// of the old phrase.
func TestDoc_TierDDescribedAsPeerOnly_RLT304(t *testing.T) {
	t.Parallel()

	topology := readRepoFile(t, "docs/architecture/infrastructure/multi-region-topology.md")
	if strings.Contains(topology, "against the local view") || strings.Contains(topology, "against the local chain") {
		t.Error("multi-region-topology.md: Tier D is described as comparing against a local ledger " +
			"hash, but verifyArchivePeers (verify_archive.go) never reads one — it only cross-compares " +
			"peers against each other (RLT-304)")
	}

	adr0016 := readRepoFile(t, "docs/adr/0016-per-region-storage-strategy.md")
	if !strings.Contains(adr0016, "RLT-304") {
		t.Error("docs/adr/0016-per-region-storage-strategy.md: no amendment correcting the " +
			"\"Local Tier D\" bullet's local-comparison claim against verifyArchivePeers' actual, " +
			"peer-only mechanism (RLT-304)")
	}
}
