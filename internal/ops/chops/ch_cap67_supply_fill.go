package chops

import (
	"context"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// cap67SupplyFill derives mint, burn and clawback over ledgers the main
// watermark covers but the supply range does not: first the stretch above
// the range up to the watermark (a window whose supply extension failed),
// then the stretch below it down to the derive floor (everything derived
// before these kinds joined the derive). Transfers there are already in the
// archive, so only the supply topics are streamed. The API states the supply
// range it reads back, so the claim follows this fill rather than preceding it.
type cap67SupplyFill struct {
	chAddr      string
	window      uint32
	floorLedger uint32
	lakeFloor   uint32 // max(floorLedger, the lake's first ledger), resolved on first need
	paused      int
	progress    func(rows int64, cursor uint32)
}

// Store calls, vars so the fill is testable without a live ClickHouse.
var (
	readCap67Coverage         = clickhouse.Cap67MovementsCoverage
	extendCap67SupplyCoverage = clickhouse.ExtendCap67SupplyCoverage
	cap67LakeMinLedger        = clickhouse.LakeMinLedger
)

// cap67SupplyFillPause is how many follow ticks a failed fill sits out, so a
// lasting refusal (a lake hole below the range) logs about once a minute.
const cap67SupplyFillPause = 60

// cap67SupplyFillWindow picks the next window to fill; ok=false when the
// range already spans [floor, watermark] or has not started (the forward
// derive starts it).
func cap67SupplyFillWindow(cov clickhouse.Cap67Coverage, floor, window uint32) (lo, hi uint32, ok bool) {
	if cov.Thru == 0 || cov.SupplyFrom == 0 {
		return 0, 0, false
	}
	thru := max(cov.SupplyThru, cov.SupplyFrom-1)
	switch {
	case thru < cov.Thru:
		return thru + 1, thru + min(window, cov.Thru-thru), true
	case cov.SupplyFrom > floor:
		return cov.SupplyFrom - min(window, cov.SupplyFrom-floor), cov.SupplyFrom - 1, true
	}
	return 0, 0, false
}

// step derives and records one fill window; worked=false when none is due.
func (f *cap67SupplyFill) step(ctx context.Context) (skipped uint64, worked bool, err error) {
	cov, err := readCap67Coverage(ctx, f.chAddr)
	if err != nil {
		return 0, false, fmt.Errorf("read coverage: %w", err)
	}
	floor := f.floorLedger
	if cov.SupplyFrom > floor {
		if f.lakeFloor == 0 {
			lakeMin, err := cap67LakeMinLedger(ctx, f.chAddr)
			if err != nil {
				return 0, false, fmt.Errorf("read lake min ledger: %w", err)
			}
			f.lakeFloor = max(f.floorLedger, lakeMin)
		}
		floor = f.lakeFloor
	}
	lo, hi, ok := cap67SupplyFillWindow(cov, floor, f.window)
	if !ok {
		return 0, false, nil
	}
	rows, skipped, err := deriveCap67Window(ctx, f.chAddr, lo, hi, cap67SupplyTopics, false)
	if err != nil {
		return skipped, false, fmt.Errorf("supply window [%d,%d]: %w", lo, hi, err)
	}
	if err := extendCap67SupplyCoverage(ctx, f.chAddr, lo, hi); err != nil {
		return skipped, false, fmt.Errorf("extend supply coverage over [%d,%d]: %w", lo, hi, err)
	}
	if f.progress != nil {
		f.progress(rows, cov.Thru)
	}
	fmt.Fprintf(os.Stderr, "ch-cap67-movements: supply window [%d,%d] done — %d movement rows\n", lo, hi, rows)
	return skipped, true, nil
}

// drain runs fill windows until none is due (the one-shot catch-up),
// returning the events skipped as undecodable.
func (f *cap67SupplyFill) drain(ctx context.Context) (skipped uint64, err error) {
	for {
		s, worked, err := f.step(ctx)
		skipped += s
		if err != nil || !worked {
			return skipped, err
		}
	}
}

// tick runs at most one fill window per follow tick, so the forward derive
// keeps its real-time cadence. A failure is logged and the fill sits out
// cap67SupplyFillPause ticks; it never fails the forward derive.
func (f *cap67SupplyFill) tick(ctx context.Context) bool {
	if f.paused > 0 {
		f.paused--
		return false
	}
	_, worked, err := f.step(ctx)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "ch-cap67-movements: supply fill error (range holds, retrying in %d ticks): %v\n", cap67SupplyFillPause, err)
		}
		f.paused = cap67SupplyFillPause
		return false
	}
	return worked
}
