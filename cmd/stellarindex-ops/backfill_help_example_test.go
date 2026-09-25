package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

var backfillExampleSourceRe = regexp.MustCompile(`stellarindex-ops backfill \\[^$]*?-source ([a-z0-9_,-]+)`)

// TestBackfillHelpExampleIsAdmissible pins that the `backfill` example in
// --help names only sources backfill accepts: not projector-owned
// (invariant [7]) and BackfillSafe. An example the command refuses teaches
// the operator the wrong tool.
func TestBackfillHelpExampleIsAdmissible(t *testing.T) {
	m := backfillExampleSourceRe.FindStringSubmatch(usageBody)
	if m == nil {
		t.Fatal("no `stellarindex-ops backfill ... -source` example found in usageBody; the detector is broken, not the help text")
	}
	for _, s := range strings.Split(m[1], ",") {
		if projector.IsProjectedSource(s, config.OracleConfig{}, nil) {
			t.Errorf("backfill example names projector-owned source %q; backfill refuses it", s)
		}
		if !external.BackfillSafe(s) {
			t.Errorf("backfill example names %q, which is not BackfillSafe; backfill refuses it", s)
		}
	}
}
