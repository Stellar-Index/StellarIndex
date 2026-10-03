package chops

import (
	"context"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A deployment whose watermark was advanced transfer-only must have mint,
// burn and clawback derived back down to the floor — and above the range
// first, where a supply extension failed — streaming only the supply topics,
// with the range recorded window by window.
func TestCap67SupplyFill_DrainsToTheFloor(t *testing.T) {
	cov := clickhouse.Cap67Coverage{Thru: 1000, SupplyFrom: 900, SupplyThru: 950}
	var windows [][2]uint32

	origRead, origExtend, origMin, origStream := readCap67Coverage, extendCap67SupplyCoverage, cap67LakeMinLedger, streamCap67TransferEvents
	t.Cleanup(func() {
		readCap67Coverage, extendCap67SupplyCoverage, cap67LakeMinLedger, streamCap67TransferEvents = origRead, origExtend, origMin, origStream
	})
	readCap67Coverage = func(context.Context, string) (clickhouse.Cap67Coverage, error) { return cov, nil }
	extendCap67SupplyCoverage = func(_ context.Context, _ string, lo, hi uint32) error {
		windows = append(windows, [2]uint32{lo, hi})
		cov.SupplyFrom, cov.SupplyThru = min(cov.SupplyFrom, lo), max(cov.SupplyThru, hi)
		return nil
	}
	cap67LakeMinLedger = func(context.Context, string) (uint32, error) { return 400, nil }
	streamCap67TransferEvents = func(_ context.Context, _ string, _, _ uint32, _, topic0Syms, _ []string, _, _, _ bool, _ func(events.Event) error) error {
		if !slices.Equal(topic0Syms, cap67SupplyTopics) {
			t.Errorf("fill streamed topics %v, want the supply topics %v only", topic0Syms, cap67SupplyTopics)
		}
		return nil
	}

	fill := &cap67SupplyFill{chAddr: "ch", window: 150, floorLedger: 500}
	if _, err := fill.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := [][2]uint32{{951, 1000}, {750, 899}, {600, 749}, {500, 599}}
	if !slices.Equal(windows, want) {
		t.Fatalf("fill windows = %v, want %v", windows, want)
	}
	if cov.SupplyFrom != 500 || cov.SupplyThru != 1000 {
		t.Fatalf("supply range after drain = [%d,%d], want [500,1000]", cov.SupplyFrom, cov.SupplyThru)
	}
}

func TestCap67SupplyFillWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cov    clickhouse.Cap67Coverage
		lo, hi uint32
		ok     bool
	}{
		{"no range yet: the forward derive starts it", clickhouse.Cap67Coverage{Thru: 1000}, 0, 0, false},
		{"range spans floor..watermark", clickhouse.Cap67Coverage{Thru: 1000, SupplyFrom: 500, SupplyThru: 1000}, 0, 0, false},
		{"orphaned from row fills up from it", clickhouse.Cap67Coverage{Thru: 1000, SupplyFrom: 900}, 900, 1000, true},
		{"last partial window stops at the floor", clickhouse.Cap67Coverage{Thru: 1000, SupplyFrom: 560, SupplyThru: 1000}, 500, 559, true},
	} {
		lo, hi, ok := cap67SupplyFillWindow(tc.cov, 500, 150)
		if lo != tc.lo || hi != tc.hi || ok != tc.ok {
			t.Errorf("%s: window = (%d, %d, %v), want (%d, %d, %v)", tc.name, lo, hi, ok, tc.lo, tc.hi, tc.ok)
		}
	}
}
