package clickhouse

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestRecentContracts_CensusPath pins the census-first directory read
// (inventory #26 item 2): with the day-keyed table usable and covering
// the window floor, the reader must sum day rows and NEVER touch
// contract_events (the 40s scan class).
func TestRecentContracts_CensusPath(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.contract_events"):
			t.Fatalf("census path must not scan contract_events: %s", q)
			return nil, nil
		case strings.Contains(q, "contracts_census_daily LIMIT 1"): // probe
			return &stubRows{data: [][]any{{base}}}, nil
		case strings.Contains(q, "FROM stellar.ledgers"): // day floor
			return &stubRows{data: [][]any{{base.Add(48 * time.Hour)}}}, nil
		case strings.Contains(q, "min(day)"): // coverage: min, max(head), today
			head := base.Add(72 * time.Hour)
			return &stubRows{data: [][]any{{base, head, head}}}, nil // fresh head
		default: // census sum
			if !strings.Contains(q, "GROUP BY contract_id") || !strings.Contains(q, "ORDER BY events DESC") {
				t.Fatalf("unexpected census query shape: %s", q)
			}
			return &stubRows{data: [][]any{
				{"CCONTRACTA", int64(500), uint32(63_000_100), base.Add(72 * time.Hour)},
				{"CCONTRACTB", int64(200), uint32(63_000_050), base.Add(71 * time.Hour)},
			}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	out, err := r.RecentContracts(context.Background(), 50, 62_999_000)
	if err != nil {
		t.Fatalf("RecentContracts: %v", err)
	}
	if len(out) != 2 || out[0].ContractID != "CCONTRACTA" || out[0].Events != 500 {
		t.Fatalf("rows = %+v, want census-ranked [A(500), B(200)]", out)
	}
}

// TestRecentContracts_CensusCoverageGapFallsBack: a window floor older
// than the census's earliest day must fall back to the exact scan —
// an in-progress backfill must never serve a silently-truncated
// ranking (the verification-blind-spots class).
func TestRecentContracts_CensusCoverageGapFallsBack(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sawLegacy := false
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.contract_events"):
			sawLegacy = true
			return &stubRows{}, nil
		case strings.Contains(q, "contracts_census_daily LIMIT 1"):
			return &stubRows{data: [][]any{{base}}}, nil
		case strings.Contains(q, "FROM stellar.ledgers"):
			return &stubRows{data: [][]any{{base.Add(-96 * time.Hour)}}}, nil // floor BEFORE coverage
		case strings.Contains(q, "min(day)"):
			return &stubRows{data: [][]any{{base, base, base}}}, nil // min, max(head), today
		default:
			t.Fatalf("unexpected query: %s", q)
			return nil, nil
		}
	}
	r := &ExplorerReader{conn: conn}
	if _, err := r.RecentContracts(context.Background(), 50, 1); err != nil {
		t.Fatalf("RecentContracts: %v", err)
	}
	if !sawLegacy {
		t.Fatal("coverage gap did not fall back to the exact contract_events scan")
	}
}

// TestRecentContracts_StaleCensusHeadFallsBack is the audit
// W1-explorer-perf-2 regression: the census reaches back to the floor
// (min(day) passes) but its HEAD is frozen days ago because the
// census-rollup timer stalled. The old gate checked min(day) only, so it
// served a ranking missing the last N days — stamped current. The reader
// must now detect the stale head (max(day) older than censusHeadMaxLag vs
// the server's today()) and fall through to the always-fresh exact scan.
func TestRecentContracts_StaleCensusHeadFallsBack(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	today := base.Add(30 * 24 * time.Hour) // "now" is 30 days past the floor day
	staleHead := today.Add(-5 * 24 * time.Hour)
	sawLegacy := false
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.contract_events"):
			sawLegacy = true
			return &stubRows{}, nil
		case strings.Contains(q, "contracts_census_daily LIMIT 1"):
			return &stubRows{data: [][]any{{base}}}, nil
		case strings.Contains(q, "FROM stellar.ledgers"): // floor is WELL within coverage
			return &stubRows{data: [][]any{{base.Add(24 * time.Hour)}}}, nil
		case strings.Contains(q, "min(day)"): // floor OK, but head is 5 days stale
			return &stubRows{data: [][]any{{base, staleHead, today}}}, nil
		default:
			t.Fatalf("stale head must not run the census sum: %s", q)
			return nil, nil
		}
	}
	r := &ExplorerReader{conn: conn}
	if _, err := r.RecentContracts(context.Background(), 50, 62_999_000); err != nil {
		t.Fatalf("RecentContracts: %v", err)
	}
	if !sawLegacy {
		t.Fatal("stale census head was served as authoritative instead of falling back to the exact scan")
	}
}

// TestRecentContracts_FreshHeadYesterdayServesCensus guards the lower
// bound of the freshness gate: right after UTC rollover, before the day's
// first rollup, the freshest census row is legitimately yesterday's. That
// one-day slack must NOT trip the gate — the census path must still serve.
func TestRecentContracts_FreshHeadYesterdayServesCensus(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	today := base.Add(30 * 24 * time.Hour)
	yesterday := today.Add(-24 * time.Hour) // exactly at the lag boundary
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.contract_events"):
			t.Fatalf("a one-day-old head must still serve from the census: %s", q)
			return nil, nil
		case strings.Contains(q, "contracts_census_daily LIMIT 1"):
			return &stubRows{data: [][]any{{base}}}, nil
		case strings.Contains(q, "FROM stellar.ledgers"):
			return &stubRows{data: [][]any{{base.Add(24 * time.Hour)}}}, nil
		case strings.Contains(q, "min(day)"):
			return &stubRows{data: [][]any{{base, yesterday, today}}}, nil
		default:
			return &stubRows{data: [][]any{
				{"CCONTRACTA", int64(500), uint32(63_000_100), yesterday},
			}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}
	out, err := r.RecentContracts(context.Background(), 50, 62_999_000)
	if err != nil {
		t.Fatalf("RecentContracts: %v", err)
	}
	if len(out) != 1 || out[0].ContractID != "CCONTRACTA" {
		t.Fatalf("rows = %+v, want the census-served [A]", out)
	}
}

// execRecorder is a censusExecConn that records every statement (under a
// mutex, so it is safe to share across the two contending goroutines).
type execRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (e *execRecorder) Exec(_ context.Context, query string, _ ...any) error {
	e.mu.Lock()
	e.queries = append(e.queries, query)
	e.mu.Unlock()
	return nil
}

var stagingTableRe = regexp.MustCompile(`contracts_census_daily_staging[0-9a-f_]*`)

// stagingTables returns the distinct staging-table names this run addressed.
func (e *execRecorder) stagingTables() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]bool{}
	for _, q := range e.queries {
		for _, m := range stagingTableRe.FindAllString(q, -1) {
			out[m] = true
		}
	}
	return out
}

// TestRunCensusDay_PrivateStagingPerRun is the W1-chrollup-4 regression:
// the 30-min census-rollup timer and a manual `ch-census-rollup -backfill`
// are two separate processes that both drive RunCensusDay against the
// current UTC day. Before the fix both DROP/INSERT/REPLACE against the ONE
// shared stellar.contracts_census_daily_staging table, so an interleaving
// (timer REPLACE landing after the backfill's staging clear but before its
// INSERT) swaps an EMPTY staging partition into the live table and serves
// zero census rows for `today`. The fix gives every run its OWN private
// staging table, so no two concurrent runs can ever touch the same staging
// table. This test drives two runs concurrently and asserts exactly that —
// and that neither run uses the old SHARED name, so it fails against the
// pre-fix code.
func TestRunCensusDay_PrivateStagingPerRun(t *testing.T) {
	day := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	const runs = 2
	recs := make([]*execRecorder, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		i := i
		recs[i] = &execRecorder{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runCensusDayConn(context.Background(), recs[i], day, func(string, ...any) {}); err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	seen := map[string]int{} // staging table -> which run(s) used it
	for i, rec := range recs {
		tables := rec.stagingTables()
		if len(tables) == 0 {
			t.Fatalf("run %d addressed no staging table at all", i)
		}
		if len(tables) != 1 {
			t.Fatalf("run %d used %d staging tables %v, want exactly one private table", i, len(tables), tables)
		}
		for tbl := range tables {
			// The old shared table name is exactly this, with no suffix — a run
			// that still uses it is not isolated.
			if tbl == "contracts_census_daily_staging" {
				t.Fatalf("run %d used the SHARED staging table %q — a concurrent backfill+timer can interleave DROP/INSERT/REPLACE on it", i, tbl)
			}
			if prev, ok := seen[tbl]; ok {
				t.Fatalf("runs %d and %d both used staging table %q — no isolation between concurrent census runs", prev, i, tbl)
			}
			seen[tbl] = i
		}
	}

	// Sanity: each run's staging table is also the one it CREATEs, INSERTs
	// into, REPLACEs FROM, and DROPs — i.e. the whole critical section is
	// scoped to the private table, so the live REPLACE never reads a
	// partition another run is mid-writing.
	for i, rec := range recs {
		var sawCreate, sawReplace, sawDrop bool
		var staging string
		for s := range rec.stagingTables() {
			staging = s
		}
		rec.mu.Lock()
		for _, q := range rec.queries {
			switch {
			case strings.HasPrefix(q, "CREATE TABLE stellar."+staging):
				sawCreate = true
			case strings.Contains(q, "REPLACE PARTITION") && strings.HasSuffix(q, "FROM stellar."+staging):
				sawReplace = true
			case strings.HasPrefix(q, "DROP TABLE IF EXISTS stellar."+staging):
				sawDrop = true
			}
		}
		rec.mu.Unlock()
		if !sawCreate || !sawReplace || !sawDrop {
			t.Fatalf("run %d critical section incomplete for %q: create=%v replace=%v drop=%v", i, staging, sawCreate, sawReplace, sawDrop)
		}
	}
}
