package chops

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

type lendingSpan struct{ lo, hi uint32 }

func memoryLimitErr() error {
	return fmt.Errorf("insert: %w", &chdriver.Exception{Code: 241, Message: "Query memory limit exceeded"})
}

func TestDeriveLendingWindows_SplitsAMemoryLimitedWindowInOrder(t *testing.T) {
	var events []string
	var watermark uint32
	derive := func(lo, hi uint32) (uint64, error) {
		if lo == 54_400_000 && hi == 54_499_999 || lo == 54_400_000 && hi == 54_449_999 {
			events = append(events, fmt.Sprintf("241 [%d,%d]", lo, hi))
			return 0, memoryLimitErr()
		}
		events = append(events, fmt.Sprintf("derive [%d,%d]", lo, hi))
		return uint64(hi - lo + 1), nil
	}
	mark := func(lo, hi uint32, tx uint64) error {
		if lo != watermark+1 {
			t.Fatalf("watermark %d advanced over [%d,%d]: not contiguous", watermark, lo, hi)
		}
		if tx != uint64(hi-lo+1) {
			t.Fatalf("mark [%d,%d] got tx ledgers %d from another range", lo, hi, tx)
		}
		watermark = hi
		events = append(events, fmt.Sprintf("mark %d", hi))
		return nil
	}
	var done []lendingSpan
	watermark = 54_350_000 - 1
	var log strings.Builder
	err := deriveLendingWindows(54_350_000, 54_520_000, &log, derive, mark, func(lo, hi uint32) { done = append(done, lendingSpan{lo, hi}) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"derive [54350000,54399999]", "mark 54399999",
		"241 [54400000,54499999]",
		"241 [54400000,54449999]",
		"derive [54400000,54424999]", "mark 54424999",
		"derive [54425000,54449999]", "mark 54449999",
		"derive [54450000,54499999]", "mark 54499999",
		"derive [54500000,54520000]", "mark 54520000",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
	}
	if len(done) != 5 || watermark != 54_520_000 {
		t.Fatalf("done %v, watermark %d", done, watermark)
	}
	if got := strings.Count(log.String(), "exceeded the ClickHouse memory limit; splitting"); got != 2 {
		t.Fatalf("split log lines = %d, want 2:\n%s", got, log.String())
	}
}

func TestDeriveLendingWindows_FailsAtTheSplitFloor(t *testing.T) {
	var smallest uint32 = 1 << 31
	marked := 0
	derive := func(lo, hi uint32) (uint64, error) {
		smallest = min(smallest, hi-lo+1)
		return 0, memoryLimitErr()
	}
	err := deriveLendingWindows(54_400_000, 54_499_999, io.Discard, derive,
		func(uint32, uint32, uint64) error { marked++; return nil }, func(uint32, uint32) {})
	if err == nil || !strings.Contains(err.Error(), "split floor") || !strings.Contains(err.Error(), "[54400000,54403124]") {
		t.Fatalf("err = %v, want the floor range named", err)
	}
	if smallest != lendingSplitFloor || marked != 0 {
		t.Fatalf("smallest range %d (want %d), marked %d (want 0)", smallest, lendingSplitFloor, marked)
	}
}

func TestDeriveLendingWindows_DoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	boom := &chdriver.Exception{Code: 60, Message: "Unknown table"}
	derive := func(uint32, uint32) (uint64, error) { calls++; return 0, boom }
	err := deriveLendingWindows(54_400_000, 54_499_999, io.Discard, derive,
		func(uint32, uint32, uint64) error { t.Fatal("watermark advanced on a failed derive"); return nil }, func(uint32, uint32) {})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want the exception unchanged after 1", err, calls)
	}
}
