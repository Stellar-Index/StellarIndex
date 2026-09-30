package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// cap67AdvanceConn answers cap67AdvanceProven's three reads: the watermark,
// the ledger contiguity count and the window event census. The embedded
// driver.Conn panics on Exec, so a refused advance provably writes nothing.
func cap67AdvanceConn(wm uint32, ledgers, expected, present uint64) *stubConn {
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "stellar.cap67_movements_watermark"):
			return &stubRows{data: [][]any{{wm}}}, nil
		case strings.Contains(q, "stellar.contract_events"):
			return &stubRows{data: [][]any{{expected, present}}}, nil
		case strings.Contains(q, "count(DISTINCT ledger_seq)"):
			return &stubRows{data: [][]any{{ledgers}}}, nil
		}
		return nil, errors.New("unexpected query: " + q)
	}}
}

// TestCap67AdvanceProven_RefusesAnEventShortfall: a window whose ledgers are
// all present but whose contract_events fall short of Σ soroban_event_count
// (a dropped or unrestored partition) must not advance — the derive read it
// as a stretch with no movements, and the watermark never revisits it.
func TestCap67AdvanceProven_RefusesAnEventShortfall(t *testing.T) {
	const from, thru = uint32(101), uint32(110)
	conn := cap67AdvanceConn(100, 10, 42, 0)
	advance, err := cap67AdvanceProven(context.Background(), conn, from, thru)
	if !errors.Is(err, ErrCap67MovementsEventShortfall) {
		t.Fatalf("cap67AdvanceProven over a contiguous window missing its events = (%v, %v), want ErrCap67MovementsEventShortfall", advance, err)
	}
	if advance {
		t.Fatal("advance = true alongside a refusal")
	}
	censused := false
	for i, q := range conn.queries {
		if !strings.Contains(q, "stellar.contract_events") {
			continue
		}
		censused = true
		if got := conn.args[i]; len(got) != 4 || got[0] != from || got[1] != thru || got[2] != from || got[3] != thru {
			t.Fatalf("event census bound %v, want [from thru from thru] = [%d %d %d %d]", got, from, thru, from, thru)
		}
	}
	if !censused {
		t.Fatal("no event census query issued")
	}
}

// TestCap67AdvanceProven_EventCensusPassesCompleteAndDuplicated pins the
// non-vacuous side: complete events advance, and unmerged RMT duplicates
// (present above expected) are not read as a shortfall.
func TestCap67AdvanceProven_EventCensusPassesCompleteAndDuplicated(t *testing.T) {
	for _, present := range []uint64{42, 50} {
		advance, err := cap67AdvanceProven(context.Background(), cap67AdvanceConn(100, 10, 42, present), 101, 110)
		if err != nil || !advance {
			t.Fatalf("present=%d of 42 expected: (%v, %v), want (true, nil)", present, advance, err)
		}
	}
}

func TestCap67WindowEventCensusQuery_Shape(t *testing.T) {
	for _, want := range []string{
		"argMax(soroban_event_count, ingested_at)",
		"GROUP BY ledger_seq",
		"FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?",
		"FROM stellar.contract_events WHERE ledger_seq BETWEEN ? AND ?",
	} {
		if !strings.Contains(cap67WindowEventCensusQuery, want) {
			t.Errorf("cap67WindowEventCensusQuery missing %q", want)
		}
	}
}
