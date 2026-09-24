package chops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A LiveSink drop at 63,410,000 under a 63,420,000 raw tip: the lake is
// contiguous from any -from below the hole only through 63,409,999.
const (
	topHoleLedger      = uint32(63_410_000)
	topContiguousTip   = topHoleLedger - 1
	topRawLakeMax      = uint32(63_420_000)
	topLiveCaptureFrom = uint32(63_415_000)
)

// stubContiguousTip answers ContiguousWatermark for a lake holding every
// ledger from 2 except topHoleLedger, up to topRawLakeMax.
func stubContiguousTip(t *testing.T) {
	t.Helper()
	orig := contiguousLakeTip
	contiguousLakeTip = func(_ context.Context, _ string, from uint32) (uint32, error) {
		switch {
		case from < topHoleLedger:
			return topContiguousTip, nil
		case from == topHoleLedger:
			return from - 1, nil
		default:
			return topRawLakeMax, nil
		}
	}
	t.Cleanup(func() { contiguousLakeTip = orig })
}

// stubLakeContiguousThrough answers every ContiguousWatermark read with tip,
// for tests of what a backfill tool does once its bound has resolved.
func stubLakeContiguousThrough(t *testing.T, tip uint32) {
	t.Helper()
	orig := contiguousLakeTip
	contiguousLakeTip = func(context.Context, string, uint32) (uint32, error) { return tip, nil }
	t.Cleanup(func() { contiguousLakeTip = orig })
}

func stubParticipantFloor(t *testing.T, floor uint32, ok bool) {
	t.Helper()
	orig := minParticipantLedger
	minParticipantLedger = func(context.Context, string) (uint32, bool, error) { return floor, ok, nil }
	t.Cleanup(func() { minParticipantLedger = orig })
}

// Every ch-*-backfill tool resolves its upper bound through
// resolveBackfillTop; the raw lake max reads straight past a LiveSink hole.
func TestLakeBackfillToolsNeverBoundByRawLakeMax(t *testing.T) {
	files, err := filepath.Glob("ch_*backfill*.go")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if strings.Contains(string(src), "clickhouse.MaxLedger(") {
			t.Errorf("%s bounds a lake backfill by clickhouse.MaxLedger; use resolveBackfillTop (contiguous tip)", f)
		}
	}
	if checked < 4 {
		t.Fatalf("scanned %d backfill sources, want at least the 4 ch-*-backfill tools", checked)
	}
}

func TestResolveBackfillTop(t *testing.T) {
	stubContiguousTip(t)
	ctx := context.Background()

	got, err := resolveBackfillTop(ctx, "ch", 2, 0)
	if err != nil || got != topContiguousTip {
		t.Fatalf("-to 0 = (%d, %v), want the contiguous tip %d, not the raw max %d", got, err, topContiguousTip, topRawLakeMax)
	}
	if got, err := resolveBackfillTop(ctx, "ch", 2, 63_000_000); err != nil || got != 63_000_000 {
		t.Fatalf("-to below the hole = (%d, %v), want 63000000", got, err)
	}
	if got, err := resolveBackfillTop(ctx, "ch", 2, topContiguousTip); err != nil || got != topContiguousTip {
		t.Fatalf("-to at the contiguous tip = (%d, %v), want %d", got, err, topContiguousTip)
	}
	_, err = resolveBackfillTop(ctx, "ch", 2, topRawLakeMax)
	if err == nil || !strings.Contains(err.Error(), "missing ledger 63410000") {
		t.Fatalf("-to across the hole: err = %v, want a refusal naming ledger %d", err, topHoleLedger)
	}
	_, err = resolveBackfillTop(ctx, "ch", topHoleLedger, 0)
	if err == nil || !strings.Contains(err.Error(), "not contiguous at -from 63410000") {
		t.Fatalf("-from at the hole: err = %v, want a refusal", err)
	}
	if got, err := resolveBackfillTop(ctx, "ch", topHoleLedger+1, 0); err != nil || got != topRawLakeMax {
		t.Fatalf("-from above the hole = (%d, %v), want %d", got, err, topRawLakeMax)
	}
}

func TestResolveBackfillTop_PropagatesWatermarkError(t *testing.T) {
	orig := contiguousLakeTip
	contiguousLakeTip = func(context.Context, string, uint32) (uint32, error) { return 0, errors.New("dial refused") }
	t.Cleanup(func() { contiguousLakeTip = orig })
	if _, err := resolveBackfillTop(context.Background(), "ch", 2, 0); err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("err = %v, want the watermark read error", err)
	}
}

// operation_participants has no MV, so the auto target (live floor − 1) must
// not reach across a hole the live sink left below the floor.
func TestResolveParticipantTo_AutoTargetRefusesAcrossHole(t *testing.T) {
	stubContiguousTip(t)
	stubParticipantFloor(t, topLiveCaptureFrom, true)
	_, err := resolveParticipantTo(context.Background(), "ch", 2, 0)
	if err == nil || !strings.Contains(err.Error(), "missing ledger 63410000") {
		t.Fatalf("err = %v, want a refusal naming the hole below the live floor", err)
	}
}

func TestResolveParticipantTo_AutoTargetBelowHole(t *testing.T) {
	stubContiguousTip(t)
	stubParticipantFloor(t, 63_000_000, true)
	if got, err := resolveParticipantTo(context.Background(), "ch", 2, 0); err != nil || got != 62_999_999 {
		t.Fatalf("got (%d, %v), want the live floor − 1 = 62999999", got, err)
	}
}

func TestResolveParticipantTo_EmptyTableTargetsContiguousTip(t *testing.T) {
	stubContiguousTip(t)
	stubParticipantFloor(t, 0, false)
	if got, err := resolveParticipantTo(context.Background(), "ch", 2, 0); err != nil || got != topContiguousTip {
		t.Fatalf("got (%d, %v), want the contiguous tip %d", got, err, topContiguousTip)
	}
}

func TestResolveParticipantTo_ExplicitToAcrossHoleRefused(t *testing.T) {
	stubContiguousTip(t)
	stubParticipantFloor(t, 0, false)
	if _, err := resolveParticipantTo(context.Background(), "ch", 2, topRawLakeMax); err == nil {
		t.Fatal("explicit -to across the hole was accepted")
	}
}

// A resumed run reads its own first run's rows as the floor; the refusal must
// tell the operator to carry the first run's -to.
func TestResolveParticipantTo_ResumeNamesTheMissingTo(t *testing.T) {
	stubContiguousTip(t)
	stubParticipantFloor(t, 2, true)
	_, err := resolveParticipantTo(context.Background(), "ch", 500_001, 0)
	if err == nil || !strings.Contains(err.Error(), "pass the -to the first run printed") {
		t.Fatalf("err = %v, want the resume guidance", err)
	}
}
