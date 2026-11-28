package chops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// parseAbsentByDesign reads -absent-by-design (comma-separated source names). A name that is not a source of this
// catalogue fails the run: a typo must never quietly leave a declared source red, nor excuse a different one.
func parseAbsentByDesign(raw string, cat []reconSource) (map[string]bool, error) {
	known := make(map[string]bool, len(cat))
	for _, src := range cat {
		known[src.name] = true
	}
	out := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("-absent-by-design %q matches no reconciliation source for this config; known sources: %s", name, strings.Join(names, ", "))
		}
		out[name] = true
	}
	return out, nil
}

// absenceRead counts a source's expected rows (re-derived from the lake) and its served rows over [lo, hi].
type absenceRead func(ctx context.Context, lo, hi uint32) (expected, served int, blind completeness.BlindSpots, err error)

// lakeAbsenceReader is the real [absenceRead]. It reads the range it is given, never the served-scoped window: an empty
// served tier can shrink that window to nothing, and a skipped read returns 0 without the lake having been asked.
func lakeAbsenceReader(store *timescale.Store, streamer completeness.EventStreamer, chAddr string, src reconSource) absenceRead {
	return func(ctx context.Context, lo, hi uint32) (int, int, completeness.BlindSpots, error) {
		expectedFor, blind, err := expectedProjection(ctx, streamer, chAddr, src, lo, hi)
		if err != nil {
			return 0, 0, completeness.BlindSpots{}, err
		}
		sc := projectionScope{From: lo, To: hi}
		var expected, served int
		for _, tgt := range src.targets {
			for _, n := range clipCounts(expectedFor(tgt), sc) {
				expected += n
			}
			actual, cerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.countFilter(), lo, hi)
			if cerr != nil {
				return 0, 0, completeness.BlindSpots{}, fmt.Errorf("%s served count: %w", tgt.table, cerr)
			}
			for _, n := range actual {
				served += n
			}
		}
		return expected, served, blind, nil
	}
}

// declaredAbsence is the outcome of judging a source the host declared absent by design.
type declaredAbsence struct {
	applied bool // projection holds: the whole-range lake read found none of the source
	present bool // the network holds the source, so the declaration is wrong here and was ignored
	detail  string
}

// judgeDeclaredAbsence decides a declared source. verified is substrate and recognition proven over [genesis, tip] with no
// floor loss; the read always runs (unless served rows already prove presence) so a wrong declaration is seen either way.
func judgeDeclaredAbsence(ctx context.Context, src reconSource, servedMins []servedFloor, verified bool, genesis, tip uint32, read absenceRead) (declaredAbsence, error) {
	kind := src.targetTables()
	for _, m := range servedMins {
		if m.present {
			return declaredAbsence{present: true, detail: fmt.Sprintf(
				"absent-by-design declaration ignored: the served tier holds %s rows in [%d,%d], so this network has the source", kind, genesis, tip)}, nil
		}
	}
	expected, served, blind, err := read(ctx, genesis, tip)
	if err != nil {
		return declaredAbsence{}, fmt.Errorf("absent-by-design lake read [%d,%d]: %w", genesis, tip, err)
	}
	switch {
	case expected > 0 || served > 0:
		return declaredAbsence{present: true, detail: fmt.Sprintf(
			"absent-by-design declaration ignored: the lake holds %d expected %s row(s) in [%d,%d] (served %d), so this network has the source",
			expected, kind, genesis, tip, served)}, nil
	case blind.Any():
		return declaredAbsence{detail: fmt.Sprintf(
			"absent-by-design declaration not applied: the lake read over [%d,%d] was blind (%s)", genesis, tip, blind.Detail())}, nil
	case !verified || len(src.targets) == 0:
		return declaredAbsence{detail: fmt.Sprintf(
			"absent-by-design declaration not applied: substrate and recognition are not both verified over [%d,%d]", genesis, tip)}, nil
	default:
		return declaredAbsence{applied: true, detail: fmt.Sprintf(
			"projection: absent by design on this network (declared); lake holds no %s in [%d,%d]", kind, genesis, tip)}, nil
	}
}

// targetTables names the source's target tables for a verdict detail.
func (src reconSource) targetTables() string {
	names := make([]string, 0, len(src.targets))
	for _, t := range src.targets {
		names = append(names, t.table)
	}
	return strings.Join(names, "/")
}
