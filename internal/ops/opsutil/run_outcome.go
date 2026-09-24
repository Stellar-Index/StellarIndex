// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import "fmt"

// RunOutcome is a write-path run's row accounting. Err is the one place a
// dropped row becomes a non-zero exit: a backfill writes with no
// dead-letter, so a row the writer rejected is lost unless the run fails
// loudly, and an idempotent command that exits 0 is never re-run.
type RunOutcome struct {
	Verb      string // subcommand name; prefixes the error
	Noun      string // what one row is, e.g. "trade", "oracle update"
	Attempted int    // rows handed to the writer
	Written   int    // rows the writer accepted
	Failed    int    // rows the writer rejected
}

// Err is non-nil when any row failed, or rows were attempted and none written.
func (o RunOutcome) Err() error {
	switch {
	case o.Failed > 0:
		return fmt.Errorf("%s: %d of %d %s(s) failed to insert (see per-row errors above) — rows were dropped with no dead-letter; refusing to exit 0",
			o.Verb, o.Failed, o.Attempted, o.Noun)
	case o.Attempted > 0 && o.Written == 0:
		return fmt.Errorf("%s: %d %s(s) attempted and none written; refusing to exit 0",
			o.Verb, o.Attempted, o.Noun)
	}
	return nil
}
