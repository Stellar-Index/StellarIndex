package clickhouse

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// walk drives the exact loop shape every windowed backfill in this package
// runs (BackfillTxHashIndex, BackfillContractActiveLedgers,
// BackfillContractInstanceChanges, BackfillOperationParticipants), collecting
// the [lo,hi] windows it would execute. It is a transcription of those loops,
// with the ClickHouse call removed — so a walk assertion here is an assertion
// about the production walk, not about a test helper.
//
// The `guard` cap exists so a non-terminating walk fails as a bounded test
// failure rather than hanging the package.
func walk(t *testing.T, from, to, window uint32) [][2]uint32 {
	t.Helper()
	const guard = 10_000
	var got [][2]uint32
	for lo := from; ; {
		hi := ledgerWindowHi(lo, to, window)
		got = append(got, [2]uint32{lo, hi})
		if len(got) > guard {
			t.Fatalf("walk(from=%d,to=%d,window=%d) did not terminate within %d windows; last=%v",
				from, to, window, guard, got[len(got)-1])
		}
		if hi >= to {
			return got
		}
		lo = hi + 1
	}
}

func fmtWindows(ws [][2]uint32) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = fmt.Sprintf("[%d,%d]", w[0], w[1])
	}
	return strings.Join(parts, " ")
}

// TestLedgerWindowHi_WalkCoversRangeExactlyOnce is the core contract: for any
// (from, to, window) the walk must tile [from,to] with contiguous,
// non-overlapping, non-empty windows — first window starts at `from`, last
// window ends at `to`, and each window starts exactly one past the previous
// one's end.
//
// The failure this pins is the last-partial-window class: a walk that computes
// `hi = lo + window - 1` unconditionally either reads PAST the tip (hi > to) or,
// with a naive `if hi > to { return }` bail, DROPS the remainder — silently
// leaving the tail of a backfill range unwritten while the job reports success.
func TestLedgerWindowHi_WalkCoversRangeExactlyOnce(t *testing.T) {
	cases := []struct {
		name             string
		from, to, window uint32
		want             [][2]uint32
	}{
		{
			// Exact multiple: two full windows, no remainder.
			name: "exact multiple", from: 1, to: 10, window: 5,
			want: [][2]uint32{{1, 5}, {6, 10}},
		},
		{
			// NOT a multiple — the remainder is a SHORT final window of one
			// ledger. Dropping it loses ledger 10 from the backfill.
			name: "remainder becomes a short final window", from: 1, to: 10, window: 3,
			want: [][2]uint32{{1, 3}, {4, 6}, {7, 9}, {10, 10}},
		},
		{
			// Window exactly equals the span: ONE window, not one full plus an
			// empty tail.
			name: "window equals span", from: 1, to: 10, window: 10,
			want: [][2]uint32{{1, 10}},
		},
		{
			// Window wider than the span: still exactly one window, clamped.
			name: "window wider than span", from: 100, to: 105, window: 1000,
			want: [][2]uint32{{100, 105}},
		},
		{
			// Single-ledger range.
			name: "single ledger", from: 42, to: 42, window: 100,
			want: [][2]uint32{{42, 42}},
		},
		{
			// Window of 1 — every ledger is its own window.
			name: "window of one", from: 7, to: 10, window: 1,
			want: [][2]uint32{{7, 7}, {8, 8}, {9, 9}, {10, 10}},
		},
		{
			// span = window+1: the boundary where the naive form is off by one.
			name: "span one more than window", from: 1, to: 11, window: 10,
			want: [][2]uint32{{1, 10}, {11, 11}},
		},
		{
			// span = window-1: a single short window.
			name: "span one less than window", from: 1, to: 9, window: 10,
			want: [][2]uint32{{1, 9}},
		},
		{
			// Realistic operator invocation: a 250k-ledger window over a
			// 600k-ledger range leaves a 100k remainder.
			name: "operator-shaped range", from: 60_000_000, to: 60_600_000, window: 250_000,
			want: [][2]uint32{
				{60_000_000, 60_249_999},
				{60_250_000, 60_499_999},
				{60_500_000, 60_600_000},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := walk(t, tc.from, tc.to, tc.window)
			if len(got) != len(tc.want) {
				t.Fatalf("walk(%d,%d,%d) = %s (%d windows), want %s (%d windows)",
					tc.from, tc.to, tc.window, fmtWindows(got), len(got), fmtWindows(tc.want), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("walk(%d,%d,%d) window %d = [%d,%d], want [%d,%d]\nfull: %s",
						tc.from, tc.to, tc.window, i, got[i][0], got[i][1], tc.want[i][0], tc.want[i][1], fmtWindows(got))
				}
			}
			// Structural invariants, asserted independently of the golden
			// list so a wrong golden can't certify a wrong walk.
			if got[0][0] != tc.from {
				t.Errorf("first window starts at %d, want from=%d", got[0][0], tc.from)
			}
			if last := got[len(got)-1][1]; last != tc.to {
				t.Errorf("last window ends at %d, want to=%d (the tail must not be dropped)", last, tc.to)
			}
			for i, w := range got {
				if w[1] < w[0] {
					t.Errorf("window %d = [%d,%d] is empty/inverted", i, w[0], w[1])
				}
				if span := uint64(w[1]) - uint64(w[0]) + 1; span > uint64(tc.window) {
					t.Errorf("window %d = [%d,%d] spans %d ledgers, exceeds window=%d",
						i, w[0], w[1], span, tc.window)
				}
				if i > 0 && w[0] != got[i-1][1]+1 {
					t.Errorf("window %d starts at %d, want %d (contiguous with previous [%d,%d])",
						i, w[0], got[i-1][1]+1, got[i-1][0], got[i-1][1])
				}
			}
		})
	}
}

// TestLedgerWindowHi_NoUint32Overflow pins the reason the helper tests the
// REMAINING span rather than computing lo+window-1 and clamping: near the top
// of the uint32 ledger-sequence space that addition WRAPS, and a wrapped hi is
// both less than `to` (so the loop never terminates) and a garbage window
// bound. `lo = hi + 1` then restarts the walk from near zero.
func TestLedgerWindowHi_NoUint32Overflow(t *testing.T) {
	const maxU32 = uint32(math.MaxUint32)

	// A window that runs off the end of the space must clamp to `to`, not wrap.
	if got := ledgerWindowHi(maxU32-5, maxU32, 1_000_000); got != maxU32 {
		t.Fatalf("ledgerWindowHi(%d, %d, 1000000) = %d, want %d (must clamp, not wrap)",
			maxU32-5, maxU32, got, maxU32)
	}
	// The naive spelling this guards against, for the record: it wraps.
	// (Held in variables so the wrap happens at run time — the same
	// expression written as constants is a compile error, which is itself a
	// reminder that only the runtime form can ship this bug.)
	lo, win := maxU32-5, uint32(1_000_000)
	if naive := lo + win - 1; naive >= lo {
		t.Fatalf("test premise broken: lo+window-1 = %d did not wrap below lo = %d", naive, lo)
	}

	// And the walk over a range that ends at the top of the space terminates.
	got := walk(t, maxU32-3, maxU32, 2)
	want := [][2]uint32{{maxU32 - 3, maxU32 - 2}, {maxU32 - 1, maxU32}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("walk at top of uint32 space = %s, want %s", fmtWindows(got), fmtWindows(want))
	}
}

// TestWindowedBackfills_RejectDegenerateArguments pins the shared usage
// contract every windowed backfill validates BEFORE dialing ClickHouse:
// 0 < from <= to and window > 0. window == 0 matters most — ledgerWindowHi's
// `rem >= window` is true for every rem, so a zero window would return
// lo+MaxUint32, i.e. a wrapped bound, on every iteration.
//
// These four all reject on the argument check, so they never reach openRead and
// need no ClickHouse; addr is deliberately unroutable to prove that.
func TestWindowedBackfills_RejectDegenerateArguments(t *testing.T) {
	const unreachable = "127.0.0.1:1" // never dialled: the arg check fires first
	noLog := func(string, ...any) {}

	bad := []struct {
		name             string
		from, to, window uint32
	}{
		{"from is zero", 0, 100, 10},
		{"to below from", 100, 99, 10},
		{"window is zero", 1, 100, 0},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if err := BackfillTxHashIndex(ctx, unreachable, tc.from, tc.to, tc.window, noLog); err == nil {
				t.Error("BackfillTxHashIndex accepted degenerate arguments")
			}
			if err := BackfillContractActiveLedgers(ctx, unreachable, tc.from, tc.to, tc.window, noLog); err == nil {
				t.Error("BackfillContractActiveLedgers accepted degenerate arguments")
			}
			if err := BackfillContractInstanceChanges(ctx, unreachable, tc.from, tc.to, tc.window, noLog); err == nil {
				t.Error("BackfillContractInstanceChanges accepted degenerate arguments")
			}
			if _, err := BackfillOperationParticipants(ctx, unreachable, tc.from, tc.to, tc.window, true, noLog); err == nil {
				t.Error("BackfillOperationParticipants accepted degenerate arguments")
			}
		})
	}
}
