package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// seedProofRows serves scripted rows to Scan's *string, *uint32 and *uint8 destinations.
type seedProofRows struct {
	driver.Rows
	rows [][]any
	i    int
}

func (r *seedProofRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *seedProofRows) Err() error { return nil }

func (r *seedProofRows) Close() error { return nil }

func (r *seedProofRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = row[i].(string)
		case *uint32:
			*d = row[i].(uint32)
		case *uint8:
			*d = row[i].(uint8)
		default:
			return fmt.Errorf("column %d: unexpected destination %T", i, d)
		}
	}
	return nil
}

// seedRange is one ranged read over the versions: its keys and [lo, hi].
type seedRange struct {
	keys   []string
	lo, hi uint32
}

// seedProofConn is a lake holding each seed as its key's version at the seed ledger, current at current[key], and
// the ranged reads' answer from after.
type seedProofConn struct {
	driver.Conn
	seeds   []lendingInterestSeedRow
	current map[string]uint32
	after   func(seedRange) ([][]any, error)
	ranges  []seedRange
}

func (c *seedProofConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "ledger_entries_current"):
		var rows [][]any
		for k, l := range c.current {
			rows = append(rows, []any{k, l})
		}
		return &seedProofRows{rows: rows}, nil
	case strings.Contains(q, "ledger_seq IN ?"):
		var rows [][]any
		for _, s := range c.seeds {
			if slices.Contains(args[2].([]string), s.Key) {
				rows = append(rows, []any{s.Key, s.Ledger, s.Intra, s.Entry, uint8(0)})
			}
		}
		return &seedProofRows{rows: rows}, nil
	}
	rg := seedRange{lo: args[1].(uint32), hi: args[2].(uint32)}
	for _, v := range args[0].(clickhouse.GroupSet).Value {
		rg.keys = append(rg.keys, v.(string))
	}
	slices.Sort(rg.keys)
	c.ranges = append(c.ranges, rg)
	if c.after == nil {
		return &seedProofRows{}, nil
	}
	rows, err := c.after(rg)
	return &seedProofRows{rows: rows}, err
}

// newSeedProofConn has five recently changed keys seeded just before the window and one untouched for 3M ledgers;
// every key changes again after the window, so each is read past its seed.
func newSeedProofConn() *seedProofConn {
	c := &seedProofConn{current: map[string]uint32{}}
	for i, l := range []uint32{10_000_090, 10_000_080, 10_000_070, 10_000_060, 9_990_000, 7_000_000} {
		k := fmt.Sprintf("k%d", i)
		c.seeds = append(c.seeds, lendingInterestSeedRow{Key: k, Ledger: l, Intra: 3, Entry: "img-" + k})
		c.current[k] = 10_000_500
	}
	return c
}

// A key untouched for 3M ledgers reads its own range; the recent keys read only from their own seeds on.
func TestProveLendingInterestSeeds_OldSeedDoesNotWidenTheOthers(t *testing.T) {
	c := newSeedProofConn()
	if err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100); err != nil {
		t.Fatal(err)
	}
	want := []seedRange{
		{keys: []string{"k0", "k1", "k2", "k3", "k4"}, lo: 9_990_001, hi: 10_000_099},
		{keys: []string{"k5"}, lo: 7_000_001, hi: 10_000_099},
	}
	if fmt.Sprint(c.ranges) != fmt.Sprint(want) {
		t.Fatalf("ranged reads %v, want %v", c.ranges, want)
	}
}

// However many distinct seed days, the ranged reads stay capped; the oldest seeds share the last.
func TestProveLendingInterestSeeds_BucketsAreCapped(t *testing.T) {
	c := &seedProofConn{current: map[string]uint32{}}
	for i := range 40 {
		k := fmt.Sprintf("k%02d", i)
		c.seeds = append(c.seeds, lendingInterestSeedRow{Key: k, Ledger: uint32(10_000_000 - i*100_000), Entry: k}) //nolint:gosec // small
		c.current[k] = 10_000_500
	}
	if err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100); err != nil {
		t.Fatal(err)
	}
	if len(c.ranges) != 16 || c.ranges[0].lo != 10_000_001 || len(c.ranges[0].keys) != 1 ||
		c.ranges[len(c.ranges)-1].lo != 10_000_000-39*100_000+1 {
		t.Fatalf("%d ranged reads, first %v, last lo %d", len(c.ranges), c.ranges[0], c.ranges[len(c.ranges)-1].lo)
	}
}

// A change after the seed and before the window refuses the seed, naming the key and the change.
func TestProveLendingInterestSeeds_LaterChangeUnproved(t *testing.T) {
	c := newSeedProofConn()
	c.after = func(rg seedRange) ([][]any, error) {
		if slices.Contains(rg.keys, "k5") {
			return [][]any{{"k5", uint32(8_000_000), uint32(1)}}, nil
		}
		return nil, nil
	}
	err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100)
	if !errors.Is(err, ErrLendingInterestSeedUnproved) || !strings.Contains(err.Error(), "key k5") ||
		!strings.Contains(err.Error(), "(8000000, 1)") {
		t.Fatalf("err %v", err)
	}
}

// A ranged read that times out names its keys and range, so an operator can act on the one slow key.
func TestProveLendingInterestSeeds_TimeoutNamesKeyAndRange(t *testing.T) {
	c := newSeedProofConn()
	c.after = func(rg seedRange) ([][]any, error) {
		if slices.Contains(rg.keys, "k5") {
			return nil, &clickhouse.Exception{Code: 159, Name: "TIMEOUT_EXCEEDED", Message: "Timeout exceeded"}
		}
		return nil, nil
	}
	err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100)
	if err == nil || errors.Is(err, ErrLendingInterestSeedUnproved) || !strings.Contains(err.Error(), "keys k5 over ledgers [7000001, 10000099]") ||
		!strings.Contains(err.Error(), "code: 159") {
		t.Fatalf("err %v", err)
	}
}
