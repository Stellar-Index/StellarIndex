package controlwiring

import (
	"strings"
	"testing"
)

// ─── SL09: Environment= defaults must not out-order EnvironmentFile= ──
//
// verify-archive-tier-b's units say "Operators override via
// /etc/default/stellarindex-ops", but systemd resolves a repeated
// environment key in FILE ORDER — the last directive for a given key
// wins. When EnvironmentFile= appeared before the hardcoded
// Environment= defaults, any VERIFY_ARCHIVE_* key an operator set in
// /etc/default/stellarindex-ops was silently discarded by the default
// declared after it, defeating the override the comment promises.
//
// The rule is mechanical: for each VERIFY_ARCHIVE_* key set via
// Environment=, EnvironmentFile= must appear EARLIER in the file (so
// the operator's value, if present, is the one applied last).
var verifyArchiveTierBUnits = []string{
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2",
	"deploy/systemd/verify-archive-tier-b.service",
}

func TestVerifyArchiveTierB_EnvironmentFilePrecedesDefaults(t *testing.T) {
	t.Parallel()
	for _, rel := range verifyArchiveTierBUnits {
		body := readRepoFile(t, rel)

		envFileIdx := -1
		firstDefaultIdx := -1
		for i, raw := range strings.Split(body, "\n") {
			line := strings.TrimSpace(raw)
			if strings.HasPrefix(line, "#") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "EnvironmentFile="):
				if envFileIdx == -1 {
					envFileIdx = i
				}
			case strings.HasPrefix(line, "Environment=VERIFY_ARCHIVE_"):
				if firstDefaultIdx == -1 {
					firstDefaultIdx = i
				}
			}
		}

		if envFileIdx == -1 {
			t.Fatalf("%s: no EnvironmentFile= directive found", rel)
		}
		if firstDefaultIdx == -1 {
			t.Fatalf("%s: no Environment=VERIFY_ARCHIVE_* default found", rel)
		}
		if envFileIdx < firstDefaultIdx {
			t.Errorf("%s: EnvironmentFile= (directive line %d) precedes the "+
				"VERIFY_ARCHIVE_* Environment= defaults (first at line %d). "+
				"systemd applies same-key settings in file order, so the "+
				"hardcoded default overrides any operator value from "+
				"/etc/default/stellarindex-ops instead of the reverse",
				rel, envFileIdx, firstDefaultIdx)
		}
	}
}
