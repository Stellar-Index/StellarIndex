package chops

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// contiguousLakeTip is clickhouse.ContiguousWatermark, a var so the bound
// resolution is testable without a live lake.
var contiguousLakeTip = clickhouse.ContiguousWatermark

// resolveBackfillTop resolves a windowed lake backfill's inclusive upper
// ledger against the lake's CONTIGUOUS tip from `from`, never its raw max:
// the LiveSink drops whole ledgers under buffer pressure, and a pass read
// across such a hole reports done while the missing ledger contributed
// nothing — permanently so for a target with no MV to re-index it once
// ch-live-catchup heals the lake. to == 0 targets the contiguous tip; an
// explicit to above it is refused rather than clamped, so a short pass is
// never reported as the range the operator asked for.
func resolveBackfillTop(ctx context.Context, chAddr string, from, to uint32) (uint32, error) {
	tip, err := contiguousLakeTip(ctx, chAddr, from)
	if err != nil {
		return 0, fmt.Errorf("resolve contiguous lake tip from ledger %d: %w", from, err)
	}
	if tip < from {
		return 0, fmt.Errorf("the lake is not contiguous at -from %d: stellar.ledgers is missing ledger %d (an unhealed hole, or ingest has not reached it); heal it with ch-live-catchup and re-run",
			from, from)
	}
	if to == 0 {
		return tip, nil
	}
	if to > tip {
		return 0, fmt.Errorf("-to %d is above the contiguous lake tip %d: stellar.ledgers is missing ledger %d (an unhealed hole, or ingest has not reached it); heal it with ch-live-catchup and re-run, or pass -to %d to fill only the contiguous prefix",
			to, tip, tip+1, tip)
	}
	return to, nil
}
