//go:build k023evidence

package controlwiring

import (
	"regexp"
	"strings"
	"testing"
)

// ─── RLT-265: a unit's header may not contradict its directives ────
//
// The verify-archive units carry unusually long headers, and an
// operator installing the deploy/systemd copy by hand reads the header
// rather than the directives — it is the only place the trade-offs are
// written down. A header that states the OPPOSITE of what the file
// configures is worse than no header:
// deploy/systemd/verify-archive-tier-a.service explains that it "keeps
// a wall-clock cap because it has no watchdog wiring" while setting
// Type=notify, NotifyAccess=main and WatchdogSec=1h three dozen lines
// below. Read one way it tells the operator to add wiring the file
// already has before adopting r1's uncapped setting; read the other
// way it tells them a run has no liveness detection, so the 16h
// wall-clock cap is load-bearing and must stay — and a cap that fires
// mid-walk leaves the high-water unadvanced, making every subsequent
// run a full pass. That is the 2026-05-13 incident documented a few
// lines further down the same header.
//
// The rule is mechanical: if a comment claims the unit has no watchdog
// wiring, the unit must not wire one.
//
// RED and build-tagged on purpose. The correction belongs in
// deploy/systemd/verify-archive-tier-a.service, which is outside this
// unit's file set — this test is the evidence, and the acceptance
// check for whoever owns that file. Drop the build tag when it is
// green:
//
//	go test -tags k023evidence ./test/controlwiring/ -run TestVerifyArchiveUnitHeaders -v
var noWatchdogClaimRE = regexp.MustCompile(`(?is)no\s+watchdog\s+wiring`)

func TestVerifyArchiveUnitHeaders_WatchdogClaimMatchesDirectives(t *testing.T) {
	t.Parallel()
	for _, rel := range verifyArchiveUnits {
		body := readRepoFile(t, rel)
		comments, directives := splitUnitFile(body)
		if !noWatchdogClaimRE.MatchString(comments) {
			continue
		}
		if strings.Contains(directives, "WatchdogSec=") {
			t.Errorf("%s: the header says the unit has no watchdog wiring, but it sets "+
				"WatchdogSec= (and Type=notify). An operator reading the header is told to add "+
				"wiring the file already has, or that a run has no liveness detection when it "+
				"does — and then keeps the wall-clock cap whose mid-walk expiry is the "+
				"2026-05-13 full-pass incident (RLT-265)", rel)
		}
	}
}

// splitUnitFile separates a unit file's comment prose from its
// directives, so a claim made in a comment can be checked against what
// the file actually sets.
func splitUnitFile(body string) (comments, directives string) {
	var c, d strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			c.WriteString(strings.TrimPrefix(trimmed, "#"))
			c.WriteString("\n")
			continue
		}
		d.WriteString(trimmed)
		d.WriteString("\n")
	}
	return c.String(), d.String()
}
