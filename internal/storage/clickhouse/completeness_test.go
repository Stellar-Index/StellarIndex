package clickhouse

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// substrateProblemFakeRow implements driver.Row over a fixed uint64 slice.
type substrateProblemFakeRow struct{ vals []uint64 }

func (r substrateProblemFakeRow) Err() error { return nil }
func (r substrateProblemFakeRow) Scan(dest ...any) error {
	for i, d := range dest {
		*d.(*uint64) = r.vals[i]
	}
	return nil
}
func (r substrateProblemFakeRow) ScanStruct(any) error { return nil }

// substrateProblemFakeConn simulates a lake truncated below haveMin with the ledgers in missing removed,
// answering SubstrateProblem's three queries (endpoint presence, gap, hash-chain) truthfully without a live
// ClickHouse.
type substrateProblemFakeConn struct {
	driver.Conn
	to      uint32
	haveMin uint64
	missing map[uint64]bool
}

func (c substrateProblemFakeConn) present(seq uint64) bool {
	return seq >= c.haveMin && seq <= uint64(c.to) && !c.missing[seq]
}

func (c substrateProblemFakeConn) presentIn(lo, hi uint64) []uint64 {
	var out []uint64
	for s := lo; s <= hi; s++ {
		if c.present(s) {
			out = append(out, s)
		}
	}
	return out
}

func (c substrateProblemFakeConn) QueryRow(_ context.Context, query string, args ...any) driver.Row {
	switch {
	case strings.Contains(query, "uniqExact"):
		rows := c.presentIn(0, uint64(c.to))
		var lo, hi uint64
		if len(rows) > 0 {
			lo, hi = rows[0], rows[len(rows)-1]
		}
		return substrateProblemFakeRow{vals: []uint64{lo, hi, uint64(len(rows))}}
	case strings.Contains(query, "win_min"):
		rows := c.presentIn(args[0].(uint64), args[1].(uint64))
		if len(rows) == 0 {
			return substrateProblemFakeRow{vals: []uint64{0, 0}}
		}
		return substrateProblemFakeRow{vals: []uint64{rows[0], rows[len(rows)-1]}}
	case strings.Contains(query, "gap_start"):
		lo, hi := args[0].(uint64), args[1].(uint64)
		rows := c.presentIn(lo, hi)
		var gap uint64
		for i := 1; i < len(rows); i++ {
			if rows[i] > rows[i-1]+1 {
				gap = rows[i-1] + 1
				break
			}
		}
		return substrateProblemFakeRow{vals: []uint64{gap}}
	case strings.Contains(query, "prior_hash"):
		return substrateProblemFakeRow{vals: []uint64{0}} // no hash-chain break in this fixture
	default:
		panic("substrateProblemFakeConn: unexpected query: " + query)
	}
}

// TestSubstrateProblem_WalksInteriorDespiteHeadTruncation pins that the expected head-truncation problem does
// not short-circuit the contiguity walks. With ledgers [2,1000), head cut at 500 and a gap at 700, the walk
// of [500,1000] must still surface the gap at 700.
func TestSubstrateProblem_WalksInteriorDespiteHeadTruncation(t *testing.T) {
	const from, to = uint32(2), uint32(1000)
	conn := substrateProblemFakeConn{to: to, haveMin: 500, missing: map[uint64]bool{700: true}}

	problem, hasProblem, detail, err := substrateProblemOn(context.Background(), conn, "", from, to)
	if err != nil {
		t.Fatalf("substrateProblemOn: %v", err)
	}
	if !hasProblem {
		t.Fatalf("hasProblem = false, want true (both the head truncation and the interior gap are real problems)")
	}
	if problem != from {
		t.Fatalf("problem = %d, want %d — the earliest missing ledger is the head, not the later interior gap", problem, from)
	}
	if !strings.Contains(detail, "700") {
		t.Fatalf("detail = %q, want it to name the interior gap at 700 as well (the walk must still run)", detail)
	}
}

// TestSubstrateProblem_ReportsEarliestMissingLedger: the problem is exactly the first missing ledger in
// [from,to], for a missing head, a missing tail, interior gaps and their combinations. A later ledger would
// become a resume watermark covering ledgers the lake does not hold.
func TestSubstrateProblem_ReportsEarliestMissingLedger(t *testing.T) {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic fixture, not crypto
	for i := 0; i < 300; i++ {
		from := uint32(2 + rng.Intn(50))
		to := from + uint32(rng.Intn(400))
		conn := substrateProblemFakeConn{to: to, haveMin: uint64(from), missing: map[uint64]bool{}}
		if rng.Intn(3) == 0 {
			conn.haveMin = uint64(from) + uint64(rng.Intn(int(to-from)+1))
		}
		if rng.Intn(3) == 0 {
			conn.to = from + uint32(rng.Intn(int(to-from)+1))
		}
		for g := rng.Intn(4); g > 0; g-- {
			conn.missing[uint64(from)+uint64(rng.Intn(int(to-from)+1))] = true
		}
		var want uint32
		for s := uint64(from); s <= uint64(to); s++ {
			if !conn.present(s) {
				want = uint32(s)
				break
			}
		}
		got, has, detail, err := substrateProblemOn(context.Background(), conn, "", from, to)
		if err != nil {
			t.Fatalf("case %d [%d,%d] haveMin=%d lakeMax=%d missing=%v: %v", i, from, to, conn.haveMin, conn.to, conn.missing, err)
		}
		if has != (want != 0) || got != want {
			t.Fatalf("case %d [%d,%d] haveMin=%d lakeMax=%d missing=%v: got (%d,%v,%q), want first missing %d",
				i, from, to, conn.haveMin, conn.to, conn.missing, got, has, detail, want)
		}
	}
}

// TestSubstrateProblem_SeamHoleBehindShortTail: a hole covering both seam ledgers (8,9) of a 7-ledger window
// walk, with the lake tail also short. The answer is the hole's first ledger, never the tail's haveMax+1.
func TestSubstrateProblem_SeamHoleBehindShortTail(t *testing.T) {
	conn := substrateProblemFakeConn{to: 50, haveMin: 2, missing: map[uint64]bool{8: true, 9: true, 10: true}}
	got, has, detail, err := substrateProblemWindowed(context.Background(), conn, "", 2, 60, 7)
	if err != nil {
		t.Fatalf("substrateProblemWindowed: %v", err)
	}
	if !has || got != 8 {
		t.Fatalf("got (%d,%v,%q), want problem 8 — the first missing ledger, not the short tail", got, has, detail)
	}
}

// TestSubstrateProblem_EarliestMissingAcrossSeams is the property test with a small window so runs of missing
// ledgers straddle seams, combined with head and tail truncation. The walk alone must find every hole: the
// count-guard fallback needs a live lake and errors here.
func TestSubstrateProblem_EarliestMissingAcrossSeams(t *testing.T) {
	rng := rand.New(rand.NewSource(11)) //nolint:gosec // deterministic fixture, not crypto
	for i := 0; i < 2000; i++ {
		window := uint64(2 + rng.Intn(8))
		from := uint32(2 + rng.Intn(20))
		to := from + uint32(rng.Intn(120))
		conn := substrateProblemFakeConn{to: to, haveMin: uint64(from), missing: map[uint64]bool{}}
		if rng.Intn(4) == 0 {
			conn.haveMin = uint64(from) + uint64(rng.Intn(int(to-from)+1))
		}
		if rng.Intn(2) == 0 {
			conn.to = from + uint32(rng.Intn(int(to-from)+1))
		}
		for g := rng.Intn(3); g > 0; g-- {
			start := uint64(from) + uint64(rng.Intn(int(to-from)+1))
			for s := start; s < start+uint64(1+rng.Intn(int(window)+3)); s++ {
				conn.missing[s] = true
			}
		}
		var want uint32
		for s := uint64(from); s <= uint64(to); s++ {
			if !conn.present(s) {
				want = uint32(s)
				break
			}
		}
		got, has, detail, err := substrateProblemWindowed(context.Background(), conn, "", from, to, window)
		if err != nil {
			t.Fatalf("case %d window=%d [%d,%d] haveMin=%d lakeMax=%d missing=%v: %v", i, window, from, to, conn.haveMin, conn.to, conn.missing, err)
		}
		if has != (want != 0) || got != want {
			t.Fatalf("case %d window=%d [%d,%d] haveMin=%d lakeMax=%d missing=%v: got (%d,%v,%q), want first missing %d",
				i, window, from, to, conn.haveMin, conn.to, conn.missing, got, has, detail, want)
		}
	}
}

// TestSubstrateQueryLo: the per-window substrate query must start one ledger BELOW the window, including at
// `from`, so the seam hash-link prev_hash(from) == ledger_hash(from-1) is checked. It is guarded at genesis
// (ledger 1 has no predecessor) so it never underflows.
func TestSubstrateQueryLo(t *testing.T) {
	const window = uint64(substrateWindow)
	cases := []struct {
		name      string
		wlo, from uint64
		want      uint64
	}{
		// An incremental -from run's first window. Returning
		// `from` (63_300_000) would leave the seam link at `from`
		// unchecked; returning from-1 evaluates the link.
		{"lower-boundary seam is checked (the finding)", 63_300_000, 63_300_000, 63_299_999},
		// A full scan from the default floor still checks its seam link at
		// ledger 2 (prev_hash(2) == ledger_hash(1)).
		{"full-scan floor (from=2) checks the link at ledger 2", 2, 2, 1},
		// The chain genesis has no predecessor: no underflow, no phantom link.
		{"chain genesis (from=1) does not underflow", 1, 1, 1},
		// Inter-window seams are unchanged by the fix.
		{"inter-window seam still decrements", 2 + window, 2, 2 + window - 1},
		{"interior inter-window seam still decrements", 63_300_000 + window, 63_300_000, 63_300_000 + window - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := substrateQueryLo(tc.wlo, tc.from); got != tc.want {
				t.Fatalf("substrateQueryLo(wlo=%d, from=%d) = %d, want %d — a seam hash-link would go unchecked",
					tc.wlo, tc.from, got, tc.want)
			}
		})
	}

	// The load-bearing property, stated directly: for the FIRST window
	// (wlo == from) at any real scan floor (from > chain genesis), the query
	// must reach one ledger below `from` so the boundary link is in range.
	for _, from := range []uint64{2, 51_000_000, 63_300_000} {
		if got := substrateQueryLo(from, from); got != from-1 {
			t.Fatalf("first-window lo for from=%d = %d, want from-1 %d (the boundary seam must be covered)", from, got, from-1)
		}
	}
}

// TestWatermark pins the pure interpretation of the ContiguousWatermark query, including a hole at `from`.
// Inputs are shapes the SQL can really return (min(ledger_seq >= from)): a missing `from` yields firstGap==0
// and minPresent==from+1, not a synthetic gap at `from`, which would mask the silent-data-loss bug.
func TestWatermark(t *testing.T) {
	tests := []struct {
		name                         string
		from, chMax, gap, minPresent uint32
		want                         uint32
	}{
		// CH has not reached `from`: no ledger >= from exists, so minPresent==0.
		{"ch not yet reached from", 100, 90, 0, 0, 99}, // chMax<from → from-1 (idle)
		{"ch exactly at from-1", 100, 99, 0, 0, 99},    // same boundary
		// `from` present + contiguous to the tip: minPresent==from, firstGap==0.
		{"complete to tip, no gap", 100, 200, 0, 100, 200}, // firstGap==0 → chMax
		// `from` present, interior hole at 150: minPresent==from, firstGap==150.
		{"interior gap above from", 100, 200, 150, 100, 149}, // hole at 150 → 149
		// Hole exactly at `from` (100 absent, 101..200 present): the SQL gives firstGap==0 and minPresent==101.
		// Without the minPresent>from guard the watermark would return chMax (200) and the projector would skip
		// ledger 100, dropping its sep41 rows. The guard stalls at from-1 (99).
		{"HOLE AT from itself → stall at from-1", 100, 200, 0, 101, 99},
		// A hole at `from` and an interior gap coexist (100 and 150 absent): firstGap=150, minPresent=101.
		// The boundary hole wins with from-1 (99), so the projector stalls at the earliest defect, not at 149.
		{"hole at from wins over a later interior gap", 100, 200, 150, 101, 99},
		// `from` present, first ledger present then hole at 101: minPresent==from.
		{"gap one past from", 100, 200, 101, 100, 100}, // first ledger present, hole next
		{"single complete ledger at from", 100, 100, 0, 100, 100},
		// A source with no cursor yet asks from=0; the lake starts at 2, so
		// `from` is a boundary hole and from-1 must not wrap to MaxUint32.
		{"from=0 does not underflow", 0, 200, 0, 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watermark(tt.from, tt.chMax, tt.gap, tt.minPresent); got != tt.want {
				t.Errorf("watermark(from=%d, chMax=%d, gap=%d, minPresent=%d) = %d, want %d",
					tt.from, tt.chMax, tt.gap, tt.minPresent, got, tt.want)
			}
		})
	}
}

// TestSubstrateCountIntact pins that SubstrateProblem compares the exact uniqExact `present` count with the
// range size: a hole covering both overlap ledgers of a window seam escapes every local gap/chain scan, and
// an endpoint guard that only tests `present > 0` passes it.
func TestSubstrateCountIntact(t *testing.T) {
	const from, to = uint32(2), uint32(10_000_002)
	full := uint64(to) - uint64(from) + 1

	// The exact failure scenario: two missing ledgers straddling the window 1
	// / window 2 seam at 5_000_001..5_000_002. A SubstrateProblem that only
	// checks `present > 0` would pass here — a real hole would read as intact.
	if substrateCountIntact(from, to, full-2) {
		t.Fatalf("substrateCountIntact(present=%d, full=%d) = true, want false — a seam-straddling hole must not read as intact", full-2, full)
	}
	// The old check's own truth value stays true for this exact input — the
	// defect was believing THAT signal instead of the exact count.
	if !(full-2 > 0) {
		t.Fatalf("test setup: present>0 must hold for this scenario (that's exactly what let the hole through pre-fix)")
	}

	if !substrateCountIntact(from, to, full) {
		t.Fatalf("substrateCountIntact(present=%d, full=%d) = false, want true — a fully-present range must be intact", full, full)
	}
}

// TestSubstrateHeadProblem pins that an empty range or a missing head reports `from`, the first absent
// ledger; a covered head reports none.
func TestSubstrateHeadProblem(t *testing.T) {
	const from, to = uint32(2), uint32(63_000_000)
	cases := []struct {
		name        string
		present     bool
		haveMin     uint32
		wantProblem uint32
		wantHas     bool
	}{
		{"empty range returns from", false, 0, from, true},
		{"missing head returns from", true, 50_000_000, from, true},
		{"head present at from is covered", true, 2, 0, false},
		{"head present above from (from=2, haveMin=2) covered", true, from, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, has, _ := substrateHeadProblem(from, to, tc.present, tc.haveMin)
			if has != tc.wantHas || p != tc.wantProblem {
				t.Fatalf("substrateHeadProblem(present=%v, haveMin=%d) = (%d,%v), want (%d,%v)",
					tc.present, tc.haveMin, p, has, tc.wantProblem, tc.wantHas)
			}
		})
	}
	// An empty lake must not green a high-genesis source: its scan starts at its genesis, so the problem is
	// that genesis and `problem < genesis` is false.
	const soroswapGenesis = uint32(50_746_266)
	if p, has, _ := substrateHeadProblem(soroswapGenesis, to, false, 0); !has || p < soroswapGenesis {
		t.Fatalf("empty-lake problem ledger = %d (has=%v); must be ≥ the scanned source's genesis %d", p, has, soroswapGenesis)
	}
}

// queryRecordingConn records every query SubstrateProblem issues.
type queryRecordingConn struct {
	substrateProblemFakeConn
	queries *[]string
}

func (c queryRecordingConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	*c.queries = append(*c.queries, query)
	return c.substrateProblemFakeConn.QueryRow(ctx, query, args...)
}

// TestSubstrateChainQueryPairsHashesFromOneRow: ledger_hash and prev_hash
// must come from one tuple argMax; two independent argMax calls can pick
// different duplicate rows on an ingested_at tie and fake (or mask) a break.
func TestSubstrateChainQueryPairsHashesFromOneRow(t *testing.T) {
	var queries []string
	conn := queryRecordingConn{
		substrateProblemFakeConn: substrateProblemFakeConn{to: 100, haveMin: 2},
		queries:                  &queries,
	}
	if _, _, _, err := substrateProblemOn(context.Background(), conn, "", 2, 100); err != nil {
		t.Fatalf("substrateProblemOn: %v", err)
	}
	var chain string
	for _, q := range queries {
		if strings.Contains(q, "prior_hash") {
			chain = q
			break
		}
	}
	if chain == "" {
		t.Fatal("hash-chain query never issued")
	}
	if !strings.Contains(chain, "argMax((ledger_hash, prev_hash), ingested_at) AS hp") {
		t.Errorf("hash-chain query must take both hashes from one tuple argMax:\n%s", chain)
	}
	if strings.Contains(chain, "argMax(ledger_hash,") || strings.Contains(chain, "argMax(prev_hash,") {
		t.Errorf("hash-chain query still has an independent per-column argMax:\n%s", chain)
	}
}
