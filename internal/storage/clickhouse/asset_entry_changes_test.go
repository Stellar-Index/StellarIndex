package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type aecFixtureRow struct {
	key      assetEntryChangeKey
	ingested time.Time
}

// aecLake answers assetEntryChangesSQL against an in-memory table the way
// ClickHouse would: filter, sort-key order, optional LIMIT 1 BY newest
// ingested_at, LIMIT. floors records the floor of every exact read.
type aecLake struct {
	rows   []aecFixtureRow
	floors []uint32
}

func (l *aecLake) eval(q string, args []any) [][]any {
	maxLedger := args[1].(uint32)
	i := 2
	var cur *assetEntryChangeKey
	if strings.Contains(q, "(ledger, tx_hash, op_index, change_index, role) <") {
		cur = &assetEntryChangeKey{args[i+1].(uint32), args[i+2].(string), args[i+3].(int32), args[i+4].(uint32), args[i+5].(string)}
		i += 6
	}
	exact := strings.Contains(q, "LIMIT 1 BY")
	floor := uint32(0)
	if exact {
		floor = args[i].(uint32)
		l.floors = append(l.floors, floor)
		i++
	}
	limit := args[i].(int)

	var sel []aecFixtureRow
	for _, r := range l.rows {
		if r.key.ledger > maxLedger || r.key.ledger < floor || (cur != nil && !aecKeyLess(r.key, *cur)) {
			continue
		}
		sel = append(sel, r)
	}
	sort.SliceStable(sel, func(a, b int) bool {
		if sel[a].key != sel[b].key {
			return aecKeyLess(sel[b].key, sel[a].key)
		}
		return exact && sel[a].ingested.After(sel[b].ingested)
	})
	var out [][]any
	for j, r := range sel {
		if exact && j > 0 && sel[j-1].key == r.key {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, []any{
			r.key.ledger, time.Unix(1700000000, 0).UTC(), r.key.txHash, r.key.opIndex, r.key.changeIndex, r.key.role, uint32(0),
			"trustline", "updated",
			[]string{"balance"},
			"GHOLDER", big.NewInt(1), r.ingested.Format(time.RFC3339), r.ingested,
		})
	}
	return out
}

func aecKeyLess(a, b assetEntryChangeKey) bool {
	if a.ledger != b.ledger {
		return a.ledger < b.ledger
	}
	if a.txHash != b.txHash {
		return a.txHash < b.txHash
	}
	if a.opIndex != b.opIndex {
		return a.opIndex < b.opIndex
	}
	if a.changeIndex != b.changeIndex {
		return a.changeIndex < b.changeIndex
	}
	return a.role < b.role
}

// TestAssetEntryChanges_FlooredExactFallbackMatchesUnbounded pages a
// duplicate-heavy asset (every key re-derived often enough that no window
// proves a page) across dense and sparse stretches, and checks every page
// equals the unbounded LIMIT 1 BY answer while each exact read carries a floor.
func TestAssetEntryChanges_FlooredExactFallbackMatchesUnbounded(t *testing.T) {
	const (
		limit     = 4
		versions  = 24 // > windowRows(limit, windowFactorKeys): every window fails to prove the page
		maxLedger = uint32(3_000_000)
	)
	base := time.Unix(1_000, 0).UTC()
	lake := &aecLake{}
	// Dense near the top, gaps wider than several spans lower down, one key at ledger 1.
	ledgers := []uint32{
		maxLedger + 5, maxLedger, maxLedger - 1, maxLedger - 100, maxLedger - 9_000, maxLedger - 9_000,
		maxLedger - 70_000, 2_000_000, 1_400_000, 900_000, 40_000, 1,
	}
	for n, lg := range ledgers {
		for v := 0; v < versions; v++ {
			lake.rows = append(lake.rows, aecFixtureRow{
				key:      assetEntryChangeKey{lg, fmt.Sprintf("tx%02d", n), int32(n % 3), 0, "holder"},
				ingested: base.Add(time.Duration((v*7)%versions) * time.Minute),
			})
		}
	}
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		return &stubRows{data: lake.eval(q, conn.args[len(conn.args)-1])}, nil
	}
	r := &ExplorerReader{conn: conn}

	var cur AssetEntryChangeCursor
	var pages int
	for ; pages < 20; pages++ {
		got, err := r.AssetEntryChanges(context.Background(), "native", limit, cur, maxLedger)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		args := []any{"native", maxLedger}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.ChangeIndex, cur.Role)
		}
		want := lake.eval(assetEntryChangesSQL(cur.IsSet(), true), append(args, uint32(0), limit))
		lake.floors = lake.floors[:len(lake.floors)-1]
		if len(got) != len(want) {
			t.Fatalf("page %d: %d rows, unbounded gives %d", pages, len(got), len(want))
		}
		for i, w := range want {
			g := got[i]
			if g.Ledger != w[0].(uint32) || g.TxHash != w[2].(string) || g.Fields != w[12].(string) {
				t.Fatalf("page %d row %d = %d/%s/%s, unbounded gives %v/%v/%v", pages, i, g.Ledger, g.TxHash, g.Fields, w[0], w[2], w[12])
			}
			if g.Fields != base.Add(time.Duration(versions-1)*time.Minute).Format(time.RFC3339) {
				t.Fatalf("page %d row %d kept version %s, want the newest", pages, i, g.Fields)
			}
		}
		if len(got) < limit {
			break
		}
		last := got[len(got)-1]
		cur = AssetEntryChangeCursor{last.Ledger, last.TxHash, last.OpIndex, last.ChangeIndex, last.Role}
	}
	if pages < 2 {
		t.Fatalf("fixture paged %d times; want several pages", pages)
	}
	if len(lake.floors) == 0 || lake.floors[0] != maxLedger-assetExactDedupSpan {
		t.Fatalf("first exact read floor = %v, want %d (ceiling minus the first span)", lake.floors, maxLedger-assetExactDedupSpan)
	}
	var widened, unbounded bool
	for i := 1; i < len(lake.floors); i++ {
		widened = widened || (lake.floors[i] < lake.floors[i-1] && lake.floors[i] > 0)
		unbounded = unbounded || lake.floors[i] == 0
	}
	if !widened || !unbounded {
		t.Fatalf("floors %v: want a geometric widening and a final unbounded read on the short last page", lake.floors)
	}
}
