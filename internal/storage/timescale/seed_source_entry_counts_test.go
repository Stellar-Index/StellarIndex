package timescale

import (
	"regexp"
	"testing"
)

// TestSeedSourceEntryCountsFoldsEveryPerSourceHypertable keeps
// SeedSourceEntryCounts in lockstep with the per-source hypertables the
// gap detector already registers. The sink bumps a source's `entries`
// tally once per decoded event and the seed SET-resets it from the
// countable tables, so a bumped table the seed does not fold is a tally
// every re-seed silently truncates — aquarius kept only its swap count,
// blend_emitter / sorocredit / upshift were zeroed outright.
func TestSeedSourceEntryCountsFoldsEveryPerSourceHypertable(t *testing.T) {
	// Watched by the gap detector but never bumped: no seed fold is owed.
	notBumped := map[string]string{
		"soroban_events": "raw event lake; no sink bumps an entries tally for it",
	}
	for _, target := range DefaultGapDetectorTargets {
		if why, ok := notBumped[target.Table]; ok {
			t.Logf("skip %s: %s", target.Table, why)
			continue
		}
		folded := regexp.MustCompile(`\bFROM\s+` + regexp.QuoteMeta(target.Table) + `\b`)
		if !folded.MatchString(seedSourceEntryCountsSQL) {
			t.Errorf("SeedSourceEntryCounts does not fold %s (gap-detector source %q): a re-seed "+
				"overwrites that source's bumped entries tally with a count that omits this table",
				target.Table, target.Source)
		}
	}
}
