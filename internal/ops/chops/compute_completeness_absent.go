package chops

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// parseAbsentByDesign reads -absent-by-design (comma-separated source names). Only an identity-free census source may be
// declared: for a contract-identity-gated source an empty lake read is exactly what a wrong or redeployed identity produces.
// Any other name fails the run, so a typo never quietly leaves a source red, excuses a different one, or excuses a gated one.
func parseAbsentByDesign(raw string, cat []reconSource) (map[string]bool, error) {
	byName := make(map[string]reconSource, len(cat))
	for _, src := range cat {
		byName[src.name] = src
	}
	out := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		src, ok := byName[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("-absent-by-design %q matches no reconciliation source for this config; declarable sources: %s", name, declarableSources(cat))
		case !src.census:
			return nil, fmt.Errorf("-absent-by-design %q cannot be declared: its expected side is gated on configured contract identity, so an empty lake "+
				"read is what a wrong or redeployed identity produces, not proof of absence; declarable sources: %s", name, declarableSources(cat))
		}
		out[name] = true
	}
	return out, nil
}

func declarableSources(cat []reconSource) string {
	var names []string
	for _, src := range cat {
		if src.census {
			names = append(names, src.name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// censusLakeTables are the raw tables the census's expected read (StreamSDEXOps) joins; a short one reads as zero trades.
var censusLakeTables = []string{"operations", "transactions", "operation_results"}

// absenceRead counts a source's expected rows (re-derived from the lake) and its served rows over [lo, hi].
type absenceRead func(ctx context.Context, lo, hi uint32) (expected, served int, blind completeness.BlindSpots, err error)

// rawTablesCheck names every raw table the expected read uses that holds fewer rows than the ledger headers declare.
type rawTablesCheck func(ctx context.Context, lo, hi uint32) (short []string, err error)

// projectionRead is what one reconcile read: summed expected and served rows over [lo, hi]. ran is false when it skipped the
// lake (an empty scope); uniform is true when every target was reconciled over all of [lo, hi].
type projectionRead struct {
	lo, hi           uint32
	expected, served int
	blind            completeness.BlindSpots
	ran, uniform     bool
}

// lakeAbsenceReader is the real [absenceRead]. It reuses this run's reconcile when that already read every target over the
// requested range, else reads the range itself: never the served-scoped window, which an empty served tier can shrink to nothing.
func lakeAbsenceReader(store *timescale.Store, streamer completeness.EventStreamer, chAddr string, src reconSource, done projectionRead) absenceRead {
	return func(ctx context.Context, lo, hi uint32) (int, int, completeness.BlindSpots, error) {
		if done.ran && done.uniform && done.lo == lo && done.hi == hi {
			return done.expected, done.served, done.blind, nil
		}
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

// rawCensusCheck is the real [rawTablesCheck]: clickhouse.RawTableCensus against stellar.ledgers' header counts, per
// 1M-ledger partition. operation_results is presence-only there (a partition owing rows holds none), never counted.
func rawCensusCheck(chAddr string, tables []string) rawTablesCheck {
	return func(ctx context.Context, lo, hi uint32) ([]string, error) {
		short, _, err := clickhouse.RawTableCensus(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, s := range short {
			if !slices.Contains(tables, s.Table) {
				continue
			}
			if s.Table == "operation_results" {
				out = append(out, fmt.Sprintf("stellar.operation_results partition %d holds no rows where operations are owed (presence-only check)", s.Partition))
				continue
			}
			out = append(out, fmt.Sprintf("stellar.%s partition %d holds %d of the %d rows stellar.ledgers declares", s.Table, s.Partition, s.Present, s.Expected))
		}
		return out, nil
	}
}

// declaredAbsence is the outcome of judging a source the host declared absent by design.
type declaredAbsence struct {
	applied bool // projection holds: the whole-range lake read found none of the source
	present bool // the network holds the source, so the declaration is wrong here and was ignored
	detail  string
}

// judgeDeclaredAbsence decides a declared source. verified is substrate and recognition proven over [genesis, tip] with no
// floor loss. A zero-row read proves absence only if the raw tables it joins are whole, so raw is checked before applying.
func judgeDeclaredAbsence(ctx context.Context, src reconSource, servedMins []servedFloor, verified bool, genesis, tip uint32, read absenceRead, raw rawTablesCheck) (declaredAbsence, error) {
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
	if expected > 0 || served > 0 {
		return declaredAbsence{present: true, detail: fmt.Sprintf(
			"absent-by-design declaration ignored: the lake holds %d expected %s row(s) in [%d,%d] (served %d), so this network has the source",
			expected, kind, genesis, tip, served)}, nil
	}
	short, err := raw(ctx, genesis, tip)
	if err != nil {
		return declaredAbsence{}, fmt.Errorf("absent-by-design raw-table census [%d,%d]: %w", genesis, tip, err)
	}
	switch {
	case len(short) > 0:
		return declaredAbsence{detail: fmt.Sprintf(
			"absent-by-design declaration not applied: the lake read found no row, but the raw tables it joins are short (%s), so zero proves nothing",
			strings.Join(short, "; "))}, nil
	case blind.Any():
		return declaredAbsence{detail: fmt.Sprintf(
			"absent-by-design declaration not applied: the lake read over [%d,%d] was blind (%s)", genesis, tip, blind.Detail())}, nil
	case !verified || len(src.targets) == 0:
		return declaredAbsence{detail: fmt.Sprintf(
			"absent-by-design declaration not applied: substrate and recognition are not both verified over [%d,%d]", genesis, tip)}, nil
	default:
		return declaredAbsence{applied: true, detail: fmt.Sprintf(
			"projection: absent by design on this network (declared); lake holds no %s in [%d,%d] and its raw tables are whole", kind, genesis, tip)}, nil
	}
}

// priorDeclaredPresence carries each declared source's stored absent_declared_but_present, for a run that never reaches the
// judge (projection deferred or not evaluated) and so has no fresh finding; an undeclared source carries nothing.
func priorDeclaredPresence(snaps []timescale.CompletenessSnapshot, declared map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for _, s := range snaps {
		if declared[s.Source] && s.AbsentDeclaredButPresent {
			out[s.Source] = true
		}
	}
	return out
}

// targetTables names the source's target tables for a verdict detail.
func (src reconSource) targetTables() string {
	names := make([]string, 0, len(src.targets))
	for _, t := range src.targets {
		names = append(names, t.table)
	}
	return strings.Join(names, "/")
}
