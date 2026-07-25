package v1_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// C3-002 + C3-009 (audit-2026-07-23) — the two unauthenticated explorer
// reads whose cost is set by the lake rather than the request.
//
// GET /v1/assets/{asset_id}/holders ran two ledger_entries_current FINAL
// scans per request; the GET /v1/contracts directory ran a GROUP BY over
// up to a year of contract_events per request, across 365 distinct
// accepted window sizes. Both on the shared 8-connection explorer pool,
// with no credential required.

// countingExplorerReader wraps the shared stub and counts the two heavy
// reads (plus the tip read each window floor needs), so these tests can
// assert how many times a burst actually reaches ClickHouse. Embedding
// keeps the other ~30 interface methods on the shared stub.
type countingExplorerReader struct {
	*stubExplorerReader

	mu            sync.Mutex
	holderCalls   int
	contractCalls int
	lastSince     uint32
}

func (c *countingExplorerReader) AssetHolders(ctx context.Context, asset string, limit int) ([]clickhouse.AssetHolder, int64, error) {
	c.mu.Lock()
	c.holderCalls++
	c.mu.Unlock()
	return c.stubExplorerReader.AssetHolders(ctx, asset, limit)
}

func (c *countingExplorerReader) RecentContracts(ctx context.Context, limit int, since uint32) ([]clickhouse.ContractDirectoryRow, error) {
	c.mu.Lock()
	c.contractCalls++
	c.lastSince = since
	c.mu.Unlock()
	return c.stubExplorerReader.RecentContracts(ctx, limit, since)
}

func (c *countingExplorerReader) counts() (holders, contracts int, since uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holderCalls, c.contractCalls, c.lastSince
}

// TestExplorer_AssetHolders_CachedAcrossRequests is the C3-002
// regression: repeat traffic for the same asset must not re-run the two
// FINAL scans, and the cached response must be byte-identical to the
// live one.
func TestExplorer_AssetHolders_CachedAcrossRequests(t *testing.T) {
	reader := &countingExplorerReader{stubExplorerReader: &stubExplorerReader{
		holders: []clickhouse.AssetHolder{
			{AccountID: testG, Balance: 900},
			{AccountID: "GB" + testG[2:], Balance: 100},
		},
		holderCount: 2,
	}}
	base := explorerTestServer(t, reader)

	for i := range 3 {
		resp := mustGet(t, base+"/v1/assets/USDC-"+testG+"/holders")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		var body struct {
			Data v1.AssetHoldersView `json:"data"`
		}
		mustDecode(t, resp, &body)
		if body.Data.HolderCount != 2 {
			t.Errorf("request %d holder_count = %d, want 2", i, body.Data.HolderCount)
		}
		if len(body.Data.Holders) != 2 {
			t.Fatalf("request %d holders = %d, want 2", i, len(body.Data.Holders))
		}
		if body.Data.Holders[0].AccountID != testG || body.Data.Holders[0].Balance != "900" {
			t.Errorf("request %d top holder = %+v, want (%s, \"900\")", i, body.Data.Holders[0], testG)
		}
	}

	holders, _, _ := reader.counts()
	if holders != 1 {
		t.Errorf("AssetHolders reader calls = %d, want 1 — three identical unauthenticated requests must "+
			"cost ONE pair of FINAL scans, not three", holders)
	}
}

// TestExplorer_AssetHolders_LimitIsNotACacheKey pins the slicing
// contract: one warm entry holds the maximum page and every accepted
// ?limit= slices from it, so varying the limit cannot multiply the scan
// count — and each response is still cut to exactly the requested size.
func TestExplorer_AssetHolders_LimitIsNotACacheKey(t *testing.T) {
	seeded := make([]clickhouse.AssetHolder, 0, 5)
	for i := range 5 {
		seeded = append(seeded, clickhouse.AssetHolder{AccountID: testG, Balance: int64(500 - i)})
	}
	reader := &countingExplorerReader{stubExplorerReader: &stubExplorerReader{
		holders:     seeded,
		holderCount: 5,
	}}
	base := explorerTestServer(t, reader)

	for _, tc := range []struct{ limit, want int }{{2, 2}, {5, 5}, {4, 4}} {
		resp := mustGet(t, base+"/v1/assets/native/holders?limit="+itoa(tc.limit))
		var body struct {
			Data v1.AssetHoldersView `json:"data"`
		}
		mustDecode(t, resp, &body)
		if len(body.Data.Holders) != tc.want {
			t.Errorf("limit=%d returned %d holders, want %d", tc.limit, len(body.Data.Holders), tc.want)
		}
		if body.Data.HolderCount != 5 {
			t.Errorf("limit=%d holder_count = %d, want 5 (the total is limit-independent)",
				tc.limit, body.Data.HolderCount)
		}
	}
	holders, _, _ := reader.counts()
	if holders != 1 {
		t.Errorf("AssetHolders reader calls = %d, want 1 across three different ?limit= values", holders)
	}
}

// TestExplorer_ContractsDirectory_WindowQuantised is the C3-009
// regression: an arbitrary ?days= is rounded UP onto the supported
// ladder before it reaches the lake, and the response reports the window
// actually aggregated rather than the one asked for.
func TestExplorer_ContractsDirectory_WindowQuantised(t *testing.T) {
	now := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	const tip = 10_000_000
	cases := []struct {
		days       int
		wantWindow int
	}{
		{1, 1}, {2, 7}, {7, 7}, {30, 30}, {31, 90}, {45, 90}, {90, 90}, {91, 365}, {365, 365},
	}
	for _, tc := range cases {
		reader := &countingExplorerReader{stubExplorerReader: &stubExplorerReader{
			ledgers:   []clickhouse.LedgerHeader{{Seq: tip, CloseTime: now}},
			directory: []clickhouse.ContractDirectoryRow{{ContractID: "CBLEND", Events: 1, LastLedger: tip - 1, LastSeen: now}},
		}}
		base := explorerTestServer(t, reader)

		resp := mustGet(t, base+"/v1/contracts?days="+itoa(tc.days))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("days=%d status = %d, want 200", tc.days, resp.StatusCode)
		}
		var body struct {
			Data v1.ContractsDirectoryView `json:"data"`
		}
		mustDecode(t, resp, &body)
		if body.Data.WindowDays != tc.wantWindow {
			t.Errorf("days=%d → window_days = %d, want %d", tc.days, body.Data.WindowDays, tc.wantWindow)
		}
		wantSince := uint32(tip - tc.wantWindow*17_280)
		if body.Data.SinceLedger != wantSince {
			t.Errorf("days=%d → since_ledger = %d, want %d", tc.days, body.Data.SinceLedger, wantSince)
		}
		if _, _, gotSince := reader.counts(); gotSince != wantSince {
			t.Errorf("days=%d → reader queried since=%d, want %d (the quantised floor must reach the lake, "+
				"not the raw request value)", tc.days, gotSince, wantSince)
		}
	}
}

// TestExplorer_ContractsDirectory_SharesBucketAcrossRequests pins the
// bound that the quantisation exists for: a caller walking ?days= across
// a bucket cannot multiply the number of multi-day GROUP BYs.
func TestExplorer_ContractsDirectory_SharesBucketAcrossRequests(t *testing.T) {
	now := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	reader := &countingExplorerReader{stubExplorerReader: &stubExplorerReader{
		ledgers:   []clickhouse.LedgerHeader{{Seq: 10_000_000, CloseTime: now}},
		directory: []clickhouse.ContractDirectoryRow{{ContractID: "CBLEND", Events: 1, LastLedger: 9_999_999, LastSeen: now}},
	}}
	base := explorerTestServer(t, reader)

	// Every one of these lands in the 90-day rung.
	for _, days := range []int{31, 45, 60, 89, 90} {
		resp := mustGet(t, base+"/v1/contracts?days="+itoa(days))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("days=%d status = %d, want 200", days, resp.StatusCode)
		}
		var body struct {
			Data v1.ContractsDirectoryView `json:"data"`
		}
		mustDecode(t, resp, &body)
		if body.Data.WindowDays != 90 {
			t.Errorf("days=%d → window_days = %d, want 90", days, body.Data.WindowDays)
		}
	}
	_, contracts, _ := reader.counts()
	if contracts != 1 {
		t.Errorf("RecentContracts reader calls = %d, want 1 — five requests inside one window bucket must "+
			"cost ONE contract_events aggregation", contracts)
	}
}

// itoa keeps the URL building above readable without dragging strconv
// into every call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
