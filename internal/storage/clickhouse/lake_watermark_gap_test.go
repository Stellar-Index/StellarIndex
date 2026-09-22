package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestLakeWatermark_ClampsToContiguousTip is the RLT-151 proof: LakeWatermark
// must not surface `as_of_ledger` past a live-sink hole. LiveSink drops whole
// ledgers under buffer pressure (ADR-0041), so raw max(ledger_seq) can sit
// past a gap the lake has not actually captured. This pins that a hole
// INSIDE the gap window (from raw tip - lakeWatermarkGapWindow) clamps the
// returned ledger + close time to the true contiguous tip (firstGap-1), not
// the raw max and its close time.
func TestLakeWatermark_ClampsToContiguousTip(t *testing.T) {
	const (
		rawTip        = uint32(100_020)
		firstGap      = uint64(100_005) // ledger 100_005 is missing
		wantWatermark = uint32(100_004) // firstGap - 1
	)
	from := rawTip - 10_000 // 90_020: rawTip - lakeWatermarkGapWindow, the query's own `from`

	rawCloseTime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)   // stamped on the (wrong) raw tip
	wantCloseTime := time.Date(2026, 9, 21, 11, 30, 0, 0, time.UTC) // stamped on the true watermark

	isContiguityQuery := func(q string) bool { return strings.Contains(q, "leadInFrame") }
	// Pre-fix shape: ONE query selecting both max(ledger_seq) and
	// max(close_time), no WHERE, no window function.
	isCombinedTipQuery := func(q string) bool {
		return strings.Contains(q, "max(ledger_seq)") && strings.Contains(q, "max(close_time)") && !isContiguityQuery(q)
	}
	// Fixed shape, query 1: raw tip alone.
	isRawTipOnlyQuery := func(q string) bool {
		return strings.Contains(q, "max(ledger_seq)") && !strings.Contains(q, "max(close_time)") && !isContiguityQuery(q)
	}
	// Fixed shape, query 3: close time AT the resolved watermark ledger.
	isCloseTimeAtLedgerQuery := func(q string) bool {
		return strings.Contains(q, "max(close_time)") && strings.Contains(q, "WHERE ledger_seq = ?")
	}

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isContiguityQuery(q):
			return &stubRows{data: [][]any{{uint64(rawTip), firstGap, uint64(from)}}}, nil
		case isCloseTimeAtLedgerQuery(q):
			return &stubRows{data: [][]any{{wantCloseTime}}}, nil
		case isCombinedTipQuery(q):
			return &stubRows{data: [][]any{{rawTip, rawCloseTime}}}, nil
		case isRawTipOnlyQuery(q):
			return &stubRows{data: [][]any{{rawTip}}}, nil
		default:
			t.Fatalf("unexpected query shape: %s", q)
			return nil, nil
		}
	}

	r := &ExplorerReader{conn: conn}
	ledger, closedAt, err := r.LakeWatermark(context.Background())
	if err != nil {
		t.Fatalf("LakeWatermark: %v", err)
	}
	if ledger != wantWatermark {
		t.Fatalf("LakeWatermark ledger = %d, want %d (the contiguous tip below the hole at %d) — "+
			"as_of_ledger must never point past an uncaptured ledger", ledger, wantWatermark, firstGap)
	}
	if !closedAt.Equal(wantCloseTime) {
		t.Fatalf("LakeWatermark close time = %s, want %s (the watermark ledger's own close time, "+
			"not the raw tip's)", closedAt, wantCloseTime)
	}
}
