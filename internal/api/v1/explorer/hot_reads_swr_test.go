package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin the stale-while-revalidate contract added in route-sweep
// 2026-07-29 for the three lake-priced explorer reads (contracts directory,
// asset holders, op-type stats): a stale entry is SERVED (degraded=true,
// with its real as_of) while exactly one detached refresh runs; a cold key
// waits for the detached compute bounded by the request deadline only; a
// failed refresh keeps the previous entry.

// swrReader overrides the three reads with countable, controllable stubs.
type swrReader struct {
	*capReader
	holdersCalls   atomic.Int32
	contractsCalls atomic.Int32
	statsCalls     atomic.Int32
	fail           atomic.Bool
}

func (r *swrReader) AssetHolders(context.Context, string, int) ([]clickhouse.AssetHolder, int64, error) {
	r.holdersCalls.Add(1)
	if r.fail.Load() {
		return nil, 0, errors.New("boom")
	}
	return []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 7}}, 1, nil
}

func (r *swrReader) RecentContracts(context.Context, int, uint32) ([]clickhouse.ContractDirectoryRow, error) {
	r.contractsCalls.Add(1)
	if r.fail.Load() {
		return nil, errors.New("boom")
	}
	return []clickhouse.ContractDirectoryRow{{ContractID: validTestContract, Events: 3}}, nil
}

func (r *swrReader) OperationTypeStats(context.Context, uint32) ([]clickhouse.OpTypeCount, error) {
	r.statsCalls.Add(1)
	if r.fail.Load() {
		return nil, errors.New("boom")
	}
	return []clickhouse.OpTypeCount{{OpType: "OperationTypePayment", Count: 9}}, nil
}

func newSWRHandler() (*Handler, *swrReader) {
	reader := &swrReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := &Handler{
		Reader: reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h, reader
}

// waitFlightIdle waits for a kicked detached refresh to finish (its flight
// entry disappears once end() runs).
func waitFlightIdle(t *testing.T, f *perKeyFlight, key string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		_, up := f.inGoing[key]
		f.mu.Unlock()
		if !up {
			return
		}
		select {
		case <-deadline:
			t.Fatal("detached refresh never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestAssetHoldersCached_StaleServedDegradedAndRefreshed(t *testing.T) {
	h, reader := newSWRHandler()
	staleAt := time.Now().Add(-2 * assetHoldersTTL)
	h.assetHolders.put("native", assetHoldersEntry{
		holders: []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 1}},
		total:   1, cachedAt: staleAt,
	})

	holders, total, asOf, degraded, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || len(holders) != 1 {
		t.Fatalf("stale serve: holders=%d total=%d err=%v, want the stale entry", len(holders), total, err)
	}
	if !degraded || !asOf.Equal(staleAt) {
		t.Errorf("stale serve: degraded=%v asOf=%v, want degraded=true with the entry's real time %v", degraded, asOf, staleAt)
	}

	waitFlightIdle(t, &h.assetHolders.flight, "native")
	if got := reader.holdersCalls.Load(); got != 1 {
		t.Errorf("detached refresh ran %d times, want exactly 1", got)
	}
	if _, ok, fresh := h.assetHolders.get("native"); !ok || !fresh {
		t.Errorf("after detached refresh: ok=%v fresh=%v, want a fresh entry", ok, fresh)
	}
}

func TestAssetHoldersCached_FailedRefreshKeepsStaleEntry(t *testing.T) {
	h, reader := newSWRHandler()
	reader.fail.Store(true)
	staleAt := time.Now().Add(-2 * assetHoldersTTL)
	h.assetHolders.put("native", assetHoldersEntry{
		holders: []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 1}},
		total:   1, cachedAt: staleAt,
	})

	_, total, _, degraded, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || !degraded {
		t.Fatalf("stale serve under failing refresh: total=%d degraded=%v err=%v", total, degraded, err)
	}
	waitFlightIdle(t, &h.assetHolders.flight, "native")
	// The failed refresh must not blank the entry.
	if e, ok, _ := h.assetHolders.get("native"); !ok || e.total != 1 {
		t.Errorf("failed refresh blanked the stale entry: ok=%v", ok)
	}
}

func TestAssetHoldersCached_ColdWaitsForDetachedCompute(t *testing.T) {
	h, reader := newSWRHandler()
	holders, total, _, degraded, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || len(holders) != 1 || degraded {
		t.Fatalf("cold fill: holders=%d total=%d degraded=%v err=%v", len(holders), total, degraded, err)
	}
	if got := reader.holdersCalls.Load(); got != 1 {
		t.Errorf("cold fill ran %d scans, want 1", got)
	}
}

func TestAssetHoldersCached_ColdFailurePropagatesRefreshError(t *testing.T) {
	h, reader := newSWRHandler()
	reader.fail.Store(true)
	_, _, _, _, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("cold failure err=%v, want the refresh's real error (C-F1: a reader deadline must map to 503)", err)
	}
}

func TestRecentContractsCached_StaleServedDegradedAndRefreshed(t *testing.T) {
	h, reader := newSWRHandler()
	staleAt := time.Now().Add(-2 * contractsDirTTL)
	h.contractsDir.put(30, contractsDirEntry{
		rows:  []clickhouse.ContractDirectoryRow{{ContractID: validTestContract, Events: 1}},
		since: 100, cachedAt: staleAt,
	})

	rows, since, asOf, degraded, err := h.recentContractsCached(context.Background(), 30, 10)
	if err != nil || len(rows) != 1 || since != 100 {
		t.Fatalf("stale serve: rows=%d since=%d err=%v", len(rows), since, err)
	}
	if !degraded || !asOf.Equal(staleAt) {
		t.Errorf("stale serve: degraded=%v asOf=%v, want degraded=true with %v", degraded, asOf, staleAt)
	}
	waitFlightIdle(t, &h.contractsDir.flight, "30")
	if got := reader.contractsCalls.Load(); got != 1 {
		t.Errorf("detached refresh ran %d aggregations, want exactly 1", got)
	}
	if _, ok, fresh := h.contractsDir.get(30); !ok || !fresh {
		t.Errorf("after detached refresh: ok=%v fresh=%v, want a fresh entry", ok, fresh)
	}
}

func TestPrewarmContractsDirectory_WarmsDefaultRung(t *testing.T) {
	h, reader := newSWRHandler()
	h.PrewarmContractsDirectory(context.Background())
	waitFlightIdle(t, &h.contractsDir.flight, "30")
	if got := reader.contractsCalls.Load(); got != 1 {
		t.Fatalf("prewarm ran %d aggregations, want 1", got)
	}
	if _, ok, fresh := h.contractsDir.get(contractsWindow(30)); !ok || !fresh {
		t.Fatalf("prewarm did not fill the default rung")
	}
	// Warm rung → prewarm is a no-op.
	h.PrewarmContractsDirectory(context.Background())
	waitFlightIdle(t, &h.contractsDir.flight, "30")
	if got := reader.contractsCalls.Load(); got != 1 {
		t.Errorf("prewarm on a warm rung re-ran the aggregation (%d calls)", got)
	}
}

func TestResolveOpTypeStats_NeverComputesInline(t *testing.T) {
	h, reader := newSWRHandler()
	// Cold: returns nothing (panel omitted — op_type_stats is omitempty),
	// kicks the detached refresh.
	if got := h.resolveOpTypeStats(context.Background(), nil); got != nil {
		t.Fatalf("cold resolve returned %v, want nil (panel appears next request)", got)
	}
	waitFlightIdle(t, &h.opTypeStats.flight, "stats")
	if got := reader.statsCalls.Load(); got != 1 {
		t.Fatalf("detached stats refresh ran %d times, want 1", got)
	}
	// Warm: served from cache, no further compute.
	stats := h.resolveOpTypeStats(context.Background(), nil)
	if len(stats) != 1 || stats[0].Type != "payment" || stats[0].Count != 9 {
		t.Fatalf("warm resolve = %+v, want the cached normalized breakdown", stats)
	}
	if got := reader.statsCalls.Load(); got != 1 {
		t.Errorf("warm resolve recomputed (%d calls)", got)
	}
}

// TestSWRRefresh_ObservesMetrics pins the shared SWR instrumentation on the
// asset_holders refresher (the other four caches route through the same
// obs.ObserveExplorerSWRRefresh helper): a successful detached refresh
// records {cache, ok}, a failing one records {cache, error}.
func TestSWRRefresh_ObservesMetrics(t *testing.T) {
	count := func(outcome string) uint64 {
		return obstest.HistogramSampleCount(t, obs.ExplorerSWRRefreshDurationSeconds, "outcome", outcome)
	}
	okBefore, errBefore := count("ok"), count("error")

	h, reader := newSWRHandler()
	fl := h.refreshAssetHolders("asset-metrics-ok")
	<-fl.done
	if got := count("ok"); got != okBefore+1 {
		t.Errorf("ok observations = %d, want %d", got, okBefore+1)
	}

	reader.fail.Store(true)
	fl = h.refreshAssetHolders("asset-metrics-err")
	<-fl.done
	if got := count("error"); got != errBefore+1 {
		t.Errorf("error observations = %d, want %d", got, errBefore+1)
	}
}

// ── contract-detail SWR cache (route-sweep 2026-07-30) ────────────────

func TestContractDetailCached_ColdWaitsThenServes(t *testing.T) {
	h, _ := newSWRHandler()
	calls := 0
	compute := func(context.Context) (any, error) { calls++; return "payload-1", nil }

	v, _, degraded, err := h.contractDetailCached(context.Background(), "ev:C1", compute)
	if err != nil || v != "payload-1" || degraded {
		t.Fatalf("cold fill: v=%v degraded=%v err=%v", v, degraded, err)
	}
	// Warm hit: no recompute.
	v, _, degraded, err = h.contractDetailCached(context.Background(), "ev:C1", compute)
	if err != nil || v != "payload-1" || degraded || calls != 1 {
		t.Fatalf("warm hit: v=%v degraded=%v calls=%d err=%v", v, degraded, calls, err)
	}
}

func TestContractDetailCached_StaleServedDegradedAndRefreshed(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	// Age the entry past the TTL.
	h.contractDetail.mu.Lock()
	e := h.contractDetail.entries["ev:C1"]
	e.cachedAt = time.Now().Add(-contractDetailTTL - time.Minute)
	h.contractDetail.entries["ev:C1"] = e
	h.contractDetail.mu.Unlock()

	v, asOf, degraded, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return "new", nil })
	if err != nil || v != "old" || !degraded {
		t.Fatalf("stale serve: v=%v degraded=%v err=%v", v, degraded, err)
	}
	if time.Since(asOf) < contractDetailTTL {
		t.Fatalf("stale serve must expose the entry's REAL age, got asOf=%v", asOf)
	}
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")
	if v, _, degraded, _ := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return "unused", nil }); v != "new" || degraded {
		t.Fatalf("after refresh: v=%v degraded=%v, want new/false", v, degraded)
	}
}

func TestContractDetailCached_FailedRefreshKeepsStaleEntry(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	h.contractDetail.mu.Lock()
	e := h.contractDetail.entries["ev:C1"]
	e.cachedAt = time.Now().Add(-contractDetailTTL - time.Minute)
	h.contractDetail.entries["ev:C1"] = e
	h.contractDetail.mu.Unlock()

	v, _, degraded, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return nil, errors.New("boom") })
	if err != nil || v != "old" || !degraded {
		t.Fatalf("stale serve during failing refresh: v=%v degraded=%v err=%v", v, degraded, err)
	}
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")
	if v, _, _, _ := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return nil, errors.New("boom") }); v != "old" {
		t.Fatalf("failed refresh must keep the previous entry, got %v", v)
	}
}

func TestContractDetailCached_ColdFailurePropagates(t *testing.T) {
	h, _ := newSWRHandler()
	_, _, _, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return nil, errors.New("boom") })
	if err == nil {
		t.Fatal("cold-path compute failure must propagate")
	}
}
