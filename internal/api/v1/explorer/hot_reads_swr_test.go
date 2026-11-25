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

// Pins stale-while-revalidate for lake-priced explorer reads (contracts directory, asset holders, op-type stats): a
// stale entry is served (real as_of) while one detached refresh runs; a cold key waits for it, bounded by
// the request deadline only; a failed refresh keeps the previous entry.

// swrReader overrides the three reads with countable, controllable stubs.
type swrReader struct {
	*capReader
	holdersCalls   atomic.Int32
	contractsCalls atomic.Int32
	statsCalls     atomic.Int32
	fail           atomic.Bool
	contractsBlock chan struct{} // when non-nil, RecentContracts waits for it
	holdersBlock   chan struct{} // when non-nil, AssetHolders waits for it
}

func (r *swrReader) AssetHolders(context.Context, string, int) ([]clickhouse.AssetHolder, int64, time.Time, error) {
	r.holdersCalls.Add(1)
	if r.holdersBlock != nil {
		<-r.holdersBlock
	}
	if r.fail.Load() {
		return nil, 0, time.Time{}, errors.New("boom")
	}
	return []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 7}}, 1, time.Time{}, nil
}

func (r *swrReader) RecentContracts(context.Context, int, uint32) ([]clickhouse.ContractDirectoryRow, error) {
	r.contractsCalls.Add(1)
	if r.contractsBlock != nil {
		<-r.contractsBlock
	}
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

func TestAssetHoldersCached_StaleServedAndRefreshed(t *testing.T) {
	h, reader := newSWRHandler()
	// Hold the detached refresh so the serve reads the stale entry, not one the refresh already replaced.
	reader.holdersBlock = make(chan struct{})
	// Past the TTL plus its one-refresh grace: stale, but not degraded while the refresh it just started is healthy.
	staleAt := time.Now().Add(-3 * assetHoldersTTL)
	h.assetHolders.put("native", assetHoldersEntry{
		holders: []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 1}},
		total:   1, cachedAt: staleAt,
	})

	holders, total, asOf, fr, err := h.assetHoldersCached(context.Background(), "native", 10)
	close(reader.holdersBlock)
	if err != nil || total != 1 || len(holders) != 1 {
		t.Fatalf("stale serve: holders=%d total=%d err=%v, want the stale entry", len(holders), total, err)
	}
	if !fr.stale || fr.degraded || !asOf.at.Equal(staleAt) {
		t.Errorf("stale serve: %+v asOf=%v, want stale, not degraded, with the entry's real time %v", fr, asOf, staleAt)
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

	_, total, _, fr, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || !fr.stale {
		t.Fatalf("stale serve under failing refresh: total=%d %+v err=%v", total, fr, err)
	}
	waitFlightIdle(t, &h.assetHolders.flight, "native")
	// The failed refresh must not blank the entry, and the next serve is carried forward over that failure.
	if e, ok, _ := h.assetHolders.get("native"); !ok || e.total != 1 {
		t.Errorf("failed refresh blanked the stale entry: ok=%v", ok)
	}
	reader.holdersBlock = make(chan struct{})
	_, _, _, fr, _ = h.assetHoldersCached(context.Background(), "native", 10)
	close(reader.holdersBlock)
	waitFlightIdle(t, &h.assetHolders.flight, "native")
	if !fr.degraded {
		t.Errorf("serve after a failed refresh: %+v, want degraded", fr)
	}
}

func TestAssetHoldersCached_ColdWaitsForDetachedCompute(t *testing.T) {
	h, reader := newSWRHandler()
	holders, total, _, fr, err := h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || len(holders) != 1 || fr.degraded {
		t.Fatalf("cold fill: holders=%d total=%d %+v err=%v", len(holders), total, fr, err)
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
	// Hold the detached refresh so the serve reads the stale entry, not one the refresh already replaced.
	reader.contractsBlock = make(chan struct{})
	staleAt := time.Now().Add(-2 * contractsDirTTL)
	h.contractsDir.put(30, contractsDirEntry{
		rows:  []clickhouse.ContractDirectoryRow{{ContractID: validTestContract, Events: 1}},
		since: 100, cachedAt: staleAt,
	})

	rows, _, since, asOf, degraded, err := h.recentContractsCached(context.Background(), 30, 10)
	close(reader.contractsBlock)
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

// TestPrewarmOpTypeStats_FillsColdCacheThenNoops: the prewarm loop builds the op-type panel before
// any request so a cold first /v1/operations load never renders without it; a fresh panel makes
// prewarm a no-op, and a reader-less or cancelled handler is safe.
func TestPrewarmOpTypeStats_FillsColdCacheThenNoops(t *testing.T) {
	h, reader := newSWRHandler()
	h.PrewarmOpTypeStats(context.Background())
	waitFlightIdle(t, &h.opTypeStats.flight, "stats")
	if got := reader.statsCalls.Load(); got != 1 {
		t.Fatalf("prewarm ran the aggregation %d times, want 1", got)
	}
	if stats, fresh, _ := h.opTypeStats.get(); !fresh || len(stats) != 1 {
		t.Fatalf("after prewarm: fresh=%v stats=%v, want a fresh 1-row panel", fresh, stats)
	}

	// Fresh panel → no-op.
	h.PrewarmOpTypeStats(context.Background())
	waitFlightIdle(t, &h.opTypeStats.flight, "stats")
	if got := reader.statsCalls.Load(); got != 1 {
		t.Errorf("prewarm on a fresh panel re-ran the aggregation (%d calls)", got)
	}

	// Nil reader / cancelled context: safe no-ops.
	(&Handler{}).PrewarmOpTypeStats(context.Background())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	h.PrewarmOpTypeStats(cancelled)
}

func TestResolveOpTypeStats_NeverComputesInline(t *testing.T) {
	h, reader := newSWRHandler()
	// Cold: returns nothing (panel omitted — op_type_stats is omitempty),
	// kicks the detached refresh.
	if got, stale := h.resolveOpTypeStats(); got != nil || !stale {
		t.Fatalf("cold resolve returned %v (stale=%v), want nil and stale (panel appears next request)", got, stale)
	}
	waitFlightIdle(t, &h.opTypeStats.flight, "stats")
	if got := reader.statsCalls.Load(); got != 1 {
		t.Fatalf("detached stats refresh ran %d times, want 1", got)
	}
	// Warm: served from cache, no further compute.
	stats, stale := h.resolveOpTypeStats()
	if stale {
		t.Error("warm resolve reported a stale panel")
	}
	if len(stats) != 1 || stats[0].Type != "payment" || stats[0].Count != 9 {
		t.Fatalf("warm resolve = %+v, want the cached normalized breakdown", stats)
	}
	if got := reader.statsCalls.Load(); got != 1 {
		t.Errorf("warm resolve recomputed (%d calls)", got)
	}
}

// Pins the shared SWR instrumentation on the asset_holders refresher (other caches use the same
// obs.ObserveExplorerSWRRefresh): a successful detached refresh records {cache, ok}, a failing one {cache, error}.
func TestSWRRefresh_ObservesMetrics(t *testing.T) {
	count := func(outcome string) uint64 {
		return obstest.HistogramSampleCount(t, obs.ExplorerSWRRefreshDurationSeconds, "outcome", outcome)
	}
	okBefore, errBefore := count("ok"), count("error")

	h, reader := newSWRHandler()
	fl := h.refreshAssetHolders("asset-metrics-ok", tryAdmit)
	<-fl.done
	if got := count("ok"); got != okBefore+1 {
		t.Errorf("ok observations = %d, want %d", got, okBefore+1)
	}

	reader.fail.Store(true)
	fl = h.refreshAssetHolders("asset-metrics-err", tryAdmit)
	<-fl.done
	if got := count("error"); got != errBefore+1 {
		t.Errorf("error observations = %d, want %d", got, errBefore+1)
	}
}

// ── shared detached-refresh gate ───────────────────

// The detached-refresh bound is global across cache keys: per-key single-flight alone lets fabricated addresses queue
// one unbounded lake scan each. On saturation the refresh is skipped (stale entries keep serving, cold keys miss
// retryably) and resumes when capacity frees.
func TestDetachedRefreshGate_SaturationSkipsNotQueues(t *testing.T) {
	h, reader := newSWRHandler()
	gate := h.detachedGate()
	held := 0
	for gate.TryAcquire() {
		held++
		if held > 1000 {
			t.Fatal("gate never saturates")
		}
	}
	if held == 0 {
		t.Fatal("gate refused its first slot")
	}

	// Cold key under saturation: skipped, not queued — no scan runs and
	// the waiter gets the retryable sentinel (mapped to 503 upstream).
	_, _, _, _, err := h.assetHoldersCached(context.Background(), "native", 10)
	if !errors.Is(err, errRefreshSaturated) {
		t.Fatalf("cold miss under saturation err = %v, want errRefreshSaturated", err)
	}
	if got := reader.holdersCalls.Load(); got != 0 {
		t.Fatalf("saturated gate still launched %d scans", got)
	}

	// Stale key under saturation: the old entry still serves, degraded even inside the grace because the refresh was
	// skipped, and the skip never blanks it.
	staleAt := time.Now().Add(-assetHoldersTTL - time.Second)
	h.assetHolders.put("USDC-"+validTestAccount, assetHoldersEntry{
		holders: []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 1}},
		total:   1, cachedAt: staleAt,
	})
	holders, total, _, fr, err := h.assetHoldersCached(context.Background(), "USDC-"+validTestAccount, 10)
	if err != nil || total != 1 || len(holders) != 1 || !fr.degraded || !fr.stale {
		t.Fatalf("stale serve under saturation: holders=%d total=%d %+v err=%v", len(holders), total, fr, err)
	}
	if got := reader.holdersCalls.Load(); got != 0 {
		t.Fatalf("stale-path refresh ran under saturation (%d scans)", got)
	}

	// Capacity freed → the next cold miss fills normally.
	for ; held > 0; held-- {
		gate.Release()
	}
	holders, total, _, fr, err = h.assetHoldersCached(context.Background(), "native", 10)
	if err != nil || total != 1 || len(holders) != 1 || fr.degraded {
		t.Fatalf("post-release cold fill: holders=%d total=%d %+v err=%v", len(holders), total, fr, err)
	}
	if got := reader.holdersCalls.Load(); got != 1 {
		t.Fatalf("post-release fill ran %d scans, want 1", got)
	}

	// The skipped key stays degraded until a refresh succeeds; the put clears the failure memory.
	usdc := "USDC-" + validTestAccount
	reader.holdersBlock = make(chan struct{}) // hold the refresh so the serve reads the skipped entry
	_, _, _, fr, _ = h.assetHoldersCached(context.Background(), usdc, 10)
	close(reader.holdersBlock)
	if !fr.degraded {
		t.Error("post-release serve of a key whose refresh was skipped came back clean before any refresh landed")
	}
	waitFlightIdle(t, &h.assetHolders.flight, usdc)
	if _, _, _, fr, _ := h.assetHoldersCached(context.Background(), usdc, 10); fr.degraded {
		t.Error("after a successful refresh the board still served degraded")
	}
}

// ── contract-detail SWR cache ────────────────

func TestContractDetailCached_ColdWaitsThenServes(t *testing.T) {
	h, _ := newSWRHandler()
	calls := 0
	compute := func(context.Context) (any, error) { calls++; return "payload-1", nil }

	v, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1", compute)
	if err != nil || v != "payload-1" || fr != (servedFreshness{}) {
		t.Fatalf("cold fill: v=%v %+v err=%v", v, fr, err)
	}
	// Warm hit: no recompute.
	v, _, fr, err = h.contractDetailCached(context.Background(), "ev:C1", compute)
	if err != nil || v != "payload-1" || fr != (servedFreshness{}) || calls != 1 {
		t.Fatalf("warm hit: v=%v %+v calls=%d err=%v", v, fr, calls, err)
	}
}

// ageContractDetail backdates key's entry to age.
func ageContractDetail(h *Handler, key string, age time.Duration) {
	h.contractDetail.mu.Lock()
	defer h.contractDetail.mu.Unlock()
	m := h.contractDetail.tableLocked(key)
	e := m[key]
	e.cachedAt = time.Now().Add(-age)
	m[key] = e
}

// An entry just past its TTL is refreshed but served unflagged: it is inside its staleness bound.
func TestContractDetailCached_ExpiredWithinBoundServedClean(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	ageContractDetail(h, "ev:C1", contractDetailTTL+time.Minute)

	v, asOf, fr, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return "new", nil })
	if err != nil || v != "old" || fr != (servedFreshness{}) {
		t.Fatalf("serve 1m past ttl: v=%v %+v err=%v, want old and unflagged", v, fr, err)
	}
	if time.Since(asOf) < contractDetailTTL {
		t.Fatalf("the serve must expose the entry's real age, got asOf=%v", asOf)
	}
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")
	if v, _, _, _ := h.contractDetailCached(context.Background(), "ev:C1", nil); v != "new" {
		t.Fatalf("expired serve did not refresh: v=%v", v)
	}
}

// Inside its bound an expired entry whose last refresh failed is flagged; a successful refresh clears the flag.
func TestContractDetailCached_FailedRefreshFlagsNextServe(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	ageContractDetail(h, "ev:C1", contractDetailTTL+time.Minute)
	h.contractDetailCached(context.Background(), "ev:C1", func(context.Context) (any, error) { return nil, errors.New("boom") })
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")

	release := make(chan struct{})
	v, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1", func(context.Context) (any, error) {
		<-release
		return "new", nil
	})
	if err != nil || v != "old" || !fr.degraded || !fr.stale {
		t.Fatalf("serve after a failed refresh, inside the bound: v=%v %+v err=%v, want old/stale+degraded", v, fr, err)
	}
	close(release)
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")
	ageContractDetail(h, "ev:C1", contractDetailTTL+time.Minute)
	hold := make(chan struct{}) // held until the serve returns: the verdict must come from the entry alone
	defer close(hold)
	if v, _, fr, _ := h.contractDetailCached(context.Background(), "ev:C1", func(context.Context) (any, error) {
		<-hold
		return nil, errors.New("released")
	}); v != "new" || fr.degraded {
		t.Fatalf("expired after a successful refresh: v=%v %+v, want new/not degraded", v, fr)
	}
}

// Positions are balances held now: their unflagged window past the TTL is half a TTL, not a whole one.
func TestContractDetailEntry_PositionsGraceIsHalfTTL(t *testing.T) {
	key := positionsCacheKey + "G1"
	if got, want := detailStaleAfterForKey(key), accountPositionsTTL+accountPositionsTTL/2; got != want {
		t.Errorf("detailStaleAfterForKey(pos) = %v, want %v", got, want)
	}
	e := contractDetailEntry{v: "p", cachedAt: time.Now().Add(-(accountPositionsTTL + 45*time.Second))}
	if !e.servedStale(key) {
		t.Error("positions 45s past their TTL were served unflagged")
	}
}

// Age past the bound is stale, not degraded: nothing has failed, and the refresh the read just started is healthy.
func TestContractDetailCached_PastBoundHealthyRefreshIsStaleNotDegraded(t *testing.T) {
	for _, key := range []string{"ev:C1", "ch:C1", positionsCacheKey + "G1", accountActivityCacheKey + "G1"} {
		h, _ := newSWRHandler()
		h.contractDetail.put(key, "old")
		ageContractDetail(h, key, detailStaleAfterForKey(key)+time.Minute)

		// Hold the detached refresh so the serve reads the stale entry, not one the refresh already replaced.
		hold := make(chan struct{})
		v, asOf, fr, err := h.contractDetailCached(context.Background(), key,
			func(context.Context) (any, error) { <-hold; return "new", nil })
		close(hold)
		if err != nil || v != "old" || !fr.stale || fr.degraded {
			t.Fatalf("%s past its bound: v=%v %+v err=%v, want old, stale, not degraded", key, v, fr, err)
		}
		if time.Since(asOf) < detailStaleAfterForKey(key) {
			t.Fatalf("%s: stale serve must expose the entry's REAL age, got asOf=%v", key, asOf)
		}
		waitFlightIdle(t, &h.contractDetail.flight, key)
		if v, _, fr, _ := h.contractDetailCached(context.Background(), key,
			func(context.Context) (any, error) { return "unused", nil }); v != "new" || fr != (servedFreshness{}) {
			t.Fatalf("%s after refresh: v=%v %+v, want new and unflagged", key, v, fr)
		}
	}
}

func TestContractDetailCached_FailedRefreshKeepsStaleEntry(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	ageContractDetail(h, "ev:C1", detailStaleAfterForKey("ev:C1")+time.Minute)

	v, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { return nil, errors.New("boom") })
	if err != nil || v != "old" || !fr.stale {
		t.Fatalf("stale serve during failing refresh: v=%v %+v err=%v", v, fr, err)
	}
	waitFlightIdle(t, &h.contractDetail.flight, "ev:C1")
	hold := make(chan struct{})
	defer close(hold)
	if v, _, fr, _ := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { <-hold; return nil, errors.New("boom") }); v != "old" || !fr.degraded || !fr.stale {
		t.Fatalf("serve after the failed refresh: v=%v %+v, want the previous entry, stale+degraded", v, fr)
	}
}

// A refresh refused a slot by the saturated gate is a known failure: that serve is degraded even inside the bound.
func TestContractDetailCached_RefusedRefreshIsDegraded(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	ageContractDetail(h, "ev:C1", contractDetailTTL+time.Minute)
	gate, class := h.detachedGate(), detachedClassForKey("ev:C1")
	held := 0
	for gate.TryAcquireClass(class) {
		held++
		if held > 1000 {
			t.Fatal("gate never saturates")
		}
	}
	defer func() {
		for ; held > 0; held-- {
			gate.ReleaseClass(class)
		}
	}()
	v, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { t.Error("a refused refresh ran"); return "new", nil })
	if err != nil || v != "old" || !fr.degraded || !fr.stale {
		t.Fatalf("serve with its refresh refused: v=%v %+v err=%v, want old, stale+degraded", v, fr, err)
	}
}

type provisionalTestPayload struct{}

func (provisionalTestPayload) provisional() bool { return true }

// A provisional payload is degraded on its expired serve, as before.
func TestContractDetailCached_ProvisionalIsDegraded(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", provisionalTestPayload{})
	hold := make(chan struct{})
	defer close(hold)
	_, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { <-hold; return "new", nil })
	if err != nil || !fr.degraded || !fr.stale {
		t.Fatalf("provisional serve: %+v err=%v, want stale+degraded", fr, err)
	}
}

// A refresh still running past its budget is an overrun: the entry it should have replaced is served degraded.
func TestContractDetailCached_OverrunRefreshIsDegraded(t *testing.T) {
	h, _ := newSWRHandler()
	h.contractDetail.put("ev:C1", "old")
	ageContractDetail(h, "ev:C1", detailStaleAfterForKey("ev:C1")+time.Hour)
	fl, _ := h.contractDetail.flight.begin("ev:C1")
	fl.started = time.Now().Add(-contractDetailRefreshTimeout - time.Second)
	defer h.contractDetail.flight.end("ev:C1", fl, nil)
	_, _, fr, err := h.contractDetailCached(context.Background(), "ev:C1",
		func(context.Context) (any, error) { t.Error("a second refresh started"); return "new", nil })
	if err != nil || !fr.degraded || !fr.stale {
		t.Fatalf("serve under an overrun refresh: %+v err=%v, want stale+degraded", fr, err)
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

// TestDetachedClassForKey_PanelsDoNotShareOneBudget: the contract page fans out to several panels
// at once, which must not compete for one refresh-gate class, or the per-class cap (half the
// global limit) refuses the losers and cold pages serve 503 panels.
func TestDetachedClassForKey_PanelsDoNotShareOneBudget(t *testing.T) {
	// The four kinds a contract page + account activity actually use.
	keys := map[string]string{
		"events":       "ev:CTEST",
		"interactions": "ix:CTEST:30",
		"code-history": "ch:CTEST",
		"activity":     "act:GTEST",
	}
	seen := map[string]string{}
	for panel, key := range keys {
		class := detachedClassForKey(key)
		if other, dup := seen[class]; dup {
			t.Errorf("panels %q and %q share refresh-gate class %q — a single page "+
				"would starve itself for that class's budget", other, panel, class)
		}
		seen[class] = panel
	}
	// A key with no kind prefix must still land somewhere valid rather
	// than producing an empty class name.
	if got := detachedClassForKey("nokindprefix"); got != "contract_detail" {
		t.Errorf("detachedClassForKey(unprefixed) = %q, want the shared default", got)
	}
}

// TestDefaultDetachedRefreshLimit_FitsOnePage — the gate's global bound
// must be at least as wide as one page's fan-out, or a single visitor
// cannot fill a cold page no matter how the classes are split.
func TestDefaultDetachedRefreshLimit_FitsOnePage(t *testing.T) {
	const contractPagePanels = 5
	if clickhouse.DefaultDetachedRefreshLimit < contractPagePanels {
		t.Fatalf("DefaultDetachedRefreshLimit = %d, but one contract page fans out to %d "+
			"concurrent reads — a cold page cannot fill itself",
			clickhouse.DefaultDetachedRefreshLimit, contractPagePanels)
	}
}
