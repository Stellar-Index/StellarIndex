package clickhouse

import (
	"context"
	"strings"
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
		case strings.Contains(q, "min(day)"): // coverage
			return &stubRows{data: [][]any{{base}}}, nil
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
			return &stubRows{data: [][]any{{base}}}, nil
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
