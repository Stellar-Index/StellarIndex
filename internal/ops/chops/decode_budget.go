package chops

import (
	"flag"
	"fmt"
)

// registerDecodeBudget registers -max-decode-errors on a re-derive command
// that soft-skips undecodable records so one bad row cannot stall a window.
func registerDecodeBudget(fs *flag.FlagSet) *uint64 {
	return fs.Uint64("max-decode-errors", 0,
		"records a run may skip as undecodable before it exits non-zero (default 0: any skip fails the run AFTER every window completes, so an unattended re-derive that dropped rows is never reported as a success; raise it only once the skips are understood)")
}

// enforceDecodeBudget makes a run that soft-skipped more rows than the operator accepted exit non-zero; the skips are only logged per
// window.
func enforceDecodeBudget(cmd string, skipped, budget uint64) error {
	if skipped <= budget {
		return nil
	}
	return fmt.Errorf("%s: %d record(s) skipped as undecodable, above -max-decode-errors %d — every window ran, but those rows are MISSING from the target; see the per-window log, and re-run with a higher -max-decode-errors only if the skips are expected",
		cmd, skipped, budget)
}
