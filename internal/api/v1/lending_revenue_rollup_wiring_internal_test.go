package v1

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// pagingRollupLake records the position each change page is read after.
type pagingRollupLake struct {
	*fakeRollupLake
	after []clickhouse.LendingInterestDay
}

func (l *pagingRollupLake) LendingInterestChangedDays(ctx context.Context, after clickhouse.LendingInterestDay, limit int) ([]clickhouse.LendingInterestDay, error) {
	l.after = append(l.after, after)
	return l.fakeRollupLake.LendingInterestChangedDays(ctx, after, limit)
}

// A completed change cycle moves the revenue rollup's cursor to the last change it queued, and the next cycle pages on
// from there rather than re-reading the same page.
func TestLendingRevenueRollup_ChangeCursorAdvances(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 5})
	lake := &pagingRollupLake{fakeRollupLake: h.lake}
	h.r.lake = lake
	h.mustCycle()
	bootstrapped := h.r.cursor
	derived := h.clock.Add(time.Minute)
	h.lake.derived[reserveDay{"P", rollupAsset("P"), rollupDay(2)}] = derived
	h.mustCycle()
	if !h.r.cursor.DerivedAt.Equal(derived) || !h.r.cursor.Day.Equal(rollupDay(2)) {
		t.Fatalf("cursor %+v after the change cycle, was %+v; want the day-2 change at %v", h.r.cursor, bootstrapped, derived)
	}
	if d, ok := h.row("P", 2); !ok || !d.InterestDerivedAt.Equal(derived) {
		t.Fatalf("changed day stored %v derived %v", ok, d.InterestDerivedAt)
	}
	lake.after = nil
	h.mustCycle()
	if len(lake.after) != 2 || !lake.after[1].DerivedAt.Equal(derived) || !lake.after[1].Day.Equal(rollupDay(2)) {
		t.Fatalf("next cycle paged after %+v, want after the day-2 change", lake.after)
	}
}

// The revenue rollup's started and last-success gauges are the ones its stalled alert reads.
func TestLendingRevenueRollup_SetsStartedAndSuccessGauges(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 5})
	h.clock = h.clock.Add(3 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.r.Run(ctx)
	if got := testutil.ToFloat64(obs.LendingRevenueRollupStartedUnix); got != float64(h.clock.Unix()) {
		t.Fatalf("started gauge %v, want %v", got, h.clock.Unix())
	}
	h.clock = h.clock.Add(time.Hour)
	h.r.rollup().runCycle(context.Background())
	if got := testutil.ToFloat64(obs.LendingRevenueRollupLastSuccessUnix); got != float64(h.clock.Unix()) {
		t.Fatalf("last-success gauge %v, want %v", got, h.clock.Unix())
	}
}
