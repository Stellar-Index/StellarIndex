package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestContractActivityBounds_MemoisesTheSettledBase: the lifetime count below the split is read once; later calls read only the tail
// at or above the same split, and the two disjoint counts add.
func TestContractActivityBounds_MemoisesTheSettledBase(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	var base, tail, settled int
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.ledgers"):
			settled++
			return &stubRows{data: [][]any{{uint32(200000)}}}, nil
		case strings.HasSuffix(q, "< ?"):
			base++
			return &stubRows{data: [][]any{{t0, t0.Add(time.Hour), uint64(40)}}}, nil
		case strings.HasSuffix(q, ">= ?"):
			tail++
			return &stubRows{data: [][]any{{t0.Add(2 * time.Hour), t0.Add(3 * time.Hour), uint64(2)}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	r := &ExplorerReader{conn: conn}
	for range 2 {
		got, err := r.contractActivityBounds(context.Background(), "CTEST")
		if err != nil {
			t.Fatal(err)
		}
		want := contractActivitySpan{first: t0, last: t0.Add(3 * time.Hour), total: 42}
		if got != want {
			t.Fatalf("span = %+v, want %+v", got, want)
		}
	}
	if settled != 1 || base != 1 || tail != 2 {
		t.Fatalf("reads settled=%d base=%d tail=%d, want 1/1/2", settled, base, tail)
	}
	for i, args := range conn.args {
		if strings.HasSuffix(conn.queries[i], "?") && args[len(args)-1] != uint32(200000) {
			t.Errorf("query %d split = %v, want 200000", i, args[len(args)-1])
		}
	}
}

func TestContractActivitySpan_PlusKeepsTheNonEmptySide(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	a := contractActivitySpan{first: t0, last: t0.Add(time.Hour), total: 3}
	var empty contractActivitySpan
	if got := a.plus(empty); got != a {
		t.Fatalf("a+empty = %+v, want %+v", got, a)
	}
	if got := empty.plus(a); got != a {
		t.Fatalf("empty+a = %+v, want %+v", got, a)
	}
}
