package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"
)

// A hypertable with 1-day chunks accrues thousands of them, and every
// ON CONFLICT insert walks all of them for the unique check: trades hit
// 3445 chunks and max_locks_per_transaction 4096 on r1 before 0062
// widened it. 0062 fixed five tables and missed nine; 0171 fixed those.
// The migration set is what a fresh node applies, so it is the thing
// held to the floor here.
const chunkIntervalFloor = 7 * 24 * time.Hour

var (
	// One create_hypertable / set_chunk_time_interval call, arguments
	// captured up to the statement's closing `);`.
	createHypertableCallRe = regexp.MustCompile(`(?s)create_hypertable\s*\((.*?)\)\s*;`)
	setChunkIntervalCallRe = regexp.MustCompile(`(?s)set_chunk_time_interval\s*\((.*?)\)\s*;`)
	// Positional interval of set_chunk_time_interval (its second argument).
	positionalIntervalArgRe = regexp.MustCompile(`^\s*'[a-zA-Z0-9_.]+'\s*,\s*INTERVAL\s*'([^']+)'`)
	chunkIntervalArgRe      = regexp.MustCompile(`chunk_time_interval\s*=>\s*INTERVAL\s*'([^']+)'`)
	sqlIntervalRe           = regexp.MustCompile(`^(\d+)\s*(hours?|days?|weeks?)$`)
)

// parseSQLInterval understands the spellings this tree uses for chunk
// widths. Anything else fails the caller rather than being skipped.
func parseSQLInterval(s string) (time.Duration, bool) {
	m := sqlIntervalRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[m[2][0]]
	return time.Duration(n) * unit, true
}

// applyChunkIntervalCalls folds one migration's create_hypertable and
// set_chunk_time_interval calls into ledger.
func applyChunkIntervalCalls(t *testing.T, ledger map[string]time.Duration, sql, file string) {
	t.Helper()
	for _, m := range createHypertableCallRe.FindAllStringSubmatch(sql, -1) {
		rel := positionalRelationArgRe.FindStringSubmatch(m[1])
		if rel == nil {
			t.Fatalf("%s: cannot read the relation of create_hypertable(%s)", file, m[1])
		}
		width := chunkIntervalFloor // TimescaleDB's default when none is given
		if iv := chunkIntervalArgRe.FindStringSubmatch(m[1]); iv != nil {
			width = mustParseSQLInterval(t, iv[1], file)
		}
		ledger[normalizeRelationName(rel[1])] = width
	}
	for _, m := range setChunkIntervalCallRe.FindAllStringSubmatch(sql, -1) {
		rel := positionalRelationArgRe.FindStringSubmatch(m[1])
		iv := positionalIntervalArgRe.FindStringSubmatch(m[1])
		if rel == nil || iv == nil {
			t.Fatalf("%s: cannot read set_chunk_time_interval(%s)", file, m[1])
		}
		ledger[normalizeRelationName(rel[1])] = mustParseSQLInterval(t, iv[1], file)
	}
}

func mustParseSQLInterval(t *testing.T, s, file string) time.Duration {
	t.Helper()
	d, ok := parseSQLInterval(s)
	if !ok {
		t.Fatalf("%s: unparseable chunk interval %q — extend parseSQLInterval, do not skip it", file, s)
	}
	return d
}

// chunkIntervalLedger replays every up-migration in numeric order and
// returns each surviving hypertable's chunk interval.
func chunkIntervalLedger(t *testing.T) map[string]time.Duration {
	t.Helper()
	ups, err := filepath.Glob(filepath.Join(findRepoRoot(t), "migrations", "[0-9]*_*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(ups)
	ledger := map[string]time.Duration{}
	for _, path := range ups {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sql := stripSQLComments(string(b))
		for _, m := range dropTableRe.FindAllStringSubmatch(sql, -1) {
			delete(ledger, normalizeRelationName(m[1]))
		}
		applyChunkIntervalCalls(t, ledger, sql, filepath.Base(path))
	}
	// 45 hypertables at 0171; a count this low means the patterns stopped matching.
	if len(ledger) < 40 {
		t.Fatalf("chunk-interval ledger holds %d hypertables — the patterns no longer match this tree. "+
			"Fix them, do not delete the test.", len(ledger))
	}
	return ledger
}

func TestHypertableChunkIntervals_NoneBelowSevenDays(t *testing.T) {
	ledger := chunkIntervalLedger(t)
	var narrow []string
	for rel, width := range ledger {
		if width < chunkIntervalFloor {
			narrow = append(narrow, rel+" ("+width.String()+")")
		}
	}
	sort.Strings(narrow)
	for _, n := range narrow {
		t.Errorf("hypertable %s ends the migration set narrower than 7 days — widen it with "+
			"set_chunk_time_interval in a new migration, as 0062 and 0171 do", n)
	}
}

func TestHypertableChunkIntervals_LedgerReadsKnownWidths(t *testing.T) {
	ledger := chunkIntervalLedger(t)
	for rel, want := range map[string]time.Duration{
		"trades":          chunkIntervalFloor,  // 1 day in 0001, widened by 0062
		"freeze_events":   30 * 24 * time.Hour, // 0018
		"fx_quotes":       30 * 24 * time.Hour, // 0028
		"rozo_events":     chunkIntervalFloor,  // 0039
		"prices_1m":       0,                   // a CAGG, never a create_hypertable call
		"sep41_transfers": chunkIntervalFloor,  // 1 day in 0047, widened by 0171
	} {
		if got := ledger[rel]; got != want {
			t.Errorf("ledger[%s] = %v, want %v", rel, got, want)
		}
	}
	if _, ok := ledger["classic_movements"]; ok {
		t.Error("classic_movements is still in the ledger although 0113 drops it")
	}
}
