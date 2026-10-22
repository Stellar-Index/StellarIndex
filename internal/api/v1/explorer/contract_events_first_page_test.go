package explorer

import (
	"context"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type eventsLimitReader struct {
	*capReader
	mu     sync.Mutex
	limits []int
}

func (r *eventsLimitReader) ContractEventsRecent(_ context.Context, _ string, limit int, _ clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	r.mu.Lock()
	r.limits = append(r.limits, limit)
	r.mu.Unlock()
	rows := make([]clickhouse.ContractActivityRow, limit)
	for i := range rows {
		rows[i].Seq = uint32(limit - i)
	}
	return rows, nil
}

// The first page is computed at the smallest bucket covering the request, not always at 500: a walk of 600 active ledgers read
// ~600MB per explorer page on r1 to serve 50 rows.
func TestContractEventsFirstPage_ComputesAtSmallestCoveringSize(t *testing.T) {
	for _, tc := range []struct{ limit, want int }{{50, 100}, {100, 100}, {101, 500}, {500, 500}} {
		h, _ := newSWRHandler()
		reader := &eventsLimitReader{capReader: &capReader{probe: &deadlineProbe{}}}
		h.Reader = reader
		rows, _, _, err := h.contractEventsFirstPageCached(context.Background(), "C1", tc.limit)
		if err != nil {
			t.Fatalf("limit %d: %v", tc.limit, err)
		}
		if len(rows) != tc.limit {
			t.Fatalf("limit %d: got %d rows", tc.limit, len(rows))
		}
		if len(reader.limits) != 1 || reader.limits[0] != tc.want {
			t.Fatalf("limit %d: computed at %v, want [%d]", tc.limit, reader.limits, tc.want)
		}
	}
}

// A 100-bucket entry must never answer a 500-row request (it would end pagination early).
func TestContractEventsFirstPage_BucketsDoNotShareEntries(t *testing.T) {
	h, _ := newSWRHandler()
	reader := &eventsLimitReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h.Reader = reader
	if _, _, _, err := h.contractEventsFirstPageCached(context.Background(), "C1", 50); err != nil {
		t.Fatal(err)
	}
	rows, _, _, err := h.contractEventsFirstPageCached(context.Background(), "C1", 500)
	if err != nil || len(rows) != 500 {
		t.Fatalf("500 after 50: rows=%d err=%v", len(rows), err)
	}
}
