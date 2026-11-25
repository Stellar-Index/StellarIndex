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

// checkSeedRanges fails unless the ranged reads tile (lowest seed, before) in order, each spanning at most a chunk
// and asking for exactly the keys whose seed lies below its hi, and number at most span/chunk + 2.
func checkSeedRanges(t *testing.T, ranges []seedRange, seeds []lendingInterestSeedRow, before uint32) {
	t.Helper()
	lowest := slices.MinFunc(seeds, func(a, b lendingInterestSeedRow) int { return int(a.Ledger) - int(b.Ledger) }).Ledger
	if limit := int(before-lowest)/lendingInterestSeedChunkLedgers + 2; len(ranges) > limit {
		t.Fatalf("%d ranged reads over %d ledgers, want at most %d", len(ranges), before-lowest, limit)
	}
	next := lowest + 1
	for _, rg := range ranges {
		var want []string
		for _, s := range seeds {
			if s.Ledger < rg.hi {
				want = append(want, s.Key)
			}
		}
		slices.Sort(want)
		if rg.lo != next || rg.hi < rg.lo || rg.hi-rg.lo >= lendingInterestSeedChunkLedgers || !slices.Equal(rg.keys, want) {
			t.Fatalf("ranged read [%d, %d] of %d keys, want %d keys from %d spanning under %d", rg.lo, rg.hi, len(rg.keys), len(want),
				next, lendingInterestSeedChunkLedgers)
		}
		next = rg.hi + 1
	}
	if next != before {
		t.Fatalf("ranged reads end at %d, want %d", next-1, before-1)
	}
}

// A key untouched for 3M ledgers is read over its whole range in bounded chunks; the recent keys only from their own
// seeds on.
func TestProveLendingInterestSeeds_OldSeedDoesNotWidenTheOthers(t *testing.T) {
	c := newSeedProofConn()
	if err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100); err != nil {
		t.Fatal(err)
	}
	checkSeedRanges(t, c.ranges, c.seeds, 10_000_100)
	if last := c.ranges[len(c.ranges)-1]; last.lo != 10_000_000 || len(c.ranges) != 13 {
		t.Fatalf("%d ranged reads, last %v", len(c.ranges), last)
	}
}

// However many old seeds over however long a range, every read spans at most a chunk and the reads are bounded by
// the span, not by the number of keys.
func TestProveLendingInterestSeeds_ReadsAreBounded(t *testing.T) {
	c := &seedProofConn{current: map[string]uint32{}}
	for i := range 1000 {
		k := fmt.Sprintf("k%04d", i)
		c.seeds = append(c.seeds, lendingInterestSeedRow{Key: k, Ledger: uint32(60_000_000 - i*37_123), Entry: k}) //nolint:gosec // small
		c.current[k] = 60_000_500
	}
	if err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 60_000_100); err != nil {
		t.Fatal(err)
	}
	checkSeedRanges(t, c.ranges, c.seeds, 60_000_100)
}

// A change after an old seed, chunks below the window, refuses the seed, naming the key and the change.
func TestProveLendingInterestSeeds_LaterChangeUnproved(t *testing.T) {
	c := newSeedProofConn()
	c.after = func(rg seedRange) ([][]any, error) {
		if slices.Contains(rg.keys, "k5") && rg.lo <= 8_000_000 && 8_000_000 <= rg.hi {
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

// The same old seed with no later change proves.
func TestProveLendingInterestSeeds_OldSeedWithoutLaterChangeProves(t *testing.T) {
	c := newSeedProofConn()
	c.after = func(seedRange) ([][]any, error) { return nil, nil }
	if err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100); err != nil {
		t.Fatal(err)
	}
}

// A ranged read the server times out is ErrLendingInterestSeedProofTimeout, naming its keys and range so an operator
// can act on the one slow key; any other failure is neither sentinel.
func TestProveLendingInterestSeeds_TimeoutNamesKeyAndRange(t *testing.T) {
	for _, code := range []int32{159, 160} {
		c := newSeedProofConn()
		c.after = func(rg seedRange) ([][]any, error) {
			if slices.Contains(rg.keys, "k5") {
				return nil, &clickhouse.Exception{Code: code, Message: "Timeout exceeded"}
			}
			return nil, nil
		}
		err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100)
		if !errors.Is(err, ErrLendingInterestSeedProofTimeout) || errors.Is(err, ErrLendingInterestSeedUnproved) ||
			!strings.Contains(err.Error(), "keys k5 over ledgers [7000001, 7249999]") || !strings.Contains(err.Error(), fmt.Sprintf("code: %d", code)) {
			t.Fatalf("code %d: err %v", code, err)
		}
	}
	c := newSeedProofConn()
	c.after = func(seedRange) ([][]any, error) {
		return nil, &clickhouse.Exception{Code: 241, Message: "Memory limit"}
	}
	err := proveLendingInterestSeeds(context.Background(), c, c.seeds, 10_000_100)
	if err == nil || errors.Is(err, ErrLendingInterestSeedProofTimeout) || errors.Is(err, ErrLendingInterestSeedUnproved) {
		t.Fatalf("memory limit: err %v", err)
	}
}
