package clickhouse

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// supplyCovConn answers extendCap67SupplyCoverageOn's reads from a fixed
// coverage and a lake missing the ledgers in holes, and records every write.
type supplyCovConn struct {
	*stubConn
	writes [][]any
}

func (c *supplyCovConn) Exec(_ context.Context, _ string, args ...any) error {
	c.writes = append(c.writes, args)
	return nil
}

func newSupplyCovConn(cov Cap67Coverage, holes ...uint32) *supplyCovConn {
	c := &supplyCovConn{}
	c.stubConn = &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "stellar.cap67_movements_watermark"):
			return &stubRows{data: [][]any{{cov.Thru, cov.SupplyFrom, cov.SupplyThru}}}, nil
		case strings.Contains(q, "stellar.contract_events"):
			return &stubRows{data: [][]any{{uint64(0), uint64(0)}}}, nil
		case strings.Contains(q, "count(DISTINCT ledger_seq)"):
			args := c.args[len(c.args)-1]
			lo, hi := args[0].(uint32), args[1].(uint32)
			n := uint64(hi-lo) + 1
			for _, h := range holes {
				if h >= lo && h <= hi {
					n--
				}
			}
			return &stubRows{data: [][]any{{n}}}, nil
		}
		return nil, errors.New("unexpected query: " + q)
	}}
	return c
}

func TestExtendCap67SupplyCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cov    Cap67Coverage
		lo, hi uint32
		holes  []uint32
		want   [][]any
		err    error
	}{
		{
			name: "first window starts the range, from before thru",
			cov:  Cap67Coverage{Thru: 200}, lo: 101, hi: 200,
			want: [][]any{{cap67SupplyFromName, uint32(101)}, {cap67SupplyThruName, uint32(200)}},
		},
		{
			name: "forward window extends thru",
			cov:  Cap67Coverage{Thru: 210, SupplyFrom: 101, SupplyThru: 200}, lo: 201, hi: 210,
			want: [][]any{{cap67SupplyThruName, uint32(210)}},
		},
		{
			name: "backfill window lowers from",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 300}, lo: 51, hi: 100,
			want: [][]any{{cap67SupplyFromName, uint32(51)}},
		},
		{
			name: "a window past a transfer-only gap claims nothing",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 200}, lo: 250, hi: 300,
		},
		{
			name: "a window below a gap claims nothing",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 200}, lo: 10, hi: 50,
		},
		{
			name: "a re-derive inside the range claims nothing",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 200}, lo: 120, hi: 150,
		},
		{
			name: "an orphaned from row is an empty range the next window fills",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101}, lo: 101, hi: 150,
			want: [][]any{{cap67SupplyThruName, uint32(150)}},
		},
		{
			name: "a hole in the newly claimed ledgers refuses",
			cov:  Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 300}, lo: 51, hi: 100, holes: []uint32{70},
			err: ErrCap67MovementsHole,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newSupplyCovConn(tc.cov, tc.holes...)
			err := extendCap67SupplyCoverageOn(context.Background(), conn, tc.lo, tc.hi)
			if !errors.Is(err, tc.err) {
				t.Fatalf("extend [%d,%d] = %v, want %v", tc.lo, tc.hi, err, tc.err)
			}
			if !slices.EqualFunc(conn.writes, tc.want, slices.Equal) {
				t.Fatalf("extend [%d,%d] wrote %v, want %v", tc.lo, tc.hi, conn.writes, tc.want)
			}
		})
	}
}

// The served supply range never runs above the main watermark, and an
// orphaned from row (thru below it) is no range at all.
func TestCap67CoverageSupplyRange(t *testing.T) {
	for _, tc := range []struct {
		cov        Cap67Coverage
		from, thru uint32
		ok         bool
	}{
		{Cap67Coverage{Thru: 150, SupplyFrom: 101, SupplyThru: 200}, 101, 150, true},
		{Cap67Coverage{Thru: 300, SupplyFrom: 101, SupplyThru: 200}, 101, 200, true},
		{Cap67Coverage{Thru: 300, SupplyFrom: 101}, 0, 0, false},
		{Cap67Coverage{Thru: 300}, 0, 0, false},
	} {
		from, thru, ok := tc.cov.SupplyRange()
		if from != tc.from || thru != tc.thru || ok != tc.ok {
			t.Errorf("%+v.SupplyRange() = (%d, %d, %v), want (%d, %d, %v)", tc.cov, from, thru, ok, tc.from, tc.thru, tc.ok)
		}
	}
}
