package explorer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type statsReader struct {
	*capReader
	statsCalls atomic.Int32
	typesErr   error
}

func (r *statsReader) ContractStats(context.Context) (clickhouse.ContractStats, error) {
	r.statsCalls.Add(1)
	n := int64(9)
	return clickhouse.ContractStats{
		Deployments: []clickhouse.ContractDeployMonth{
			{Month: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), SAC: 2, Wasm: 3},
			{Month: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), SAC: 1, Wasm: 4},
		},
		Active90d: &n,
	}, nil
}

func (r *statsReader) ContractTypes(_ context.Context, ids []string) (map[string]bool, error) {
	if r.typesErr != nil {
		return nil, r.typesErr
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (r *statsReader) RecentContracts(context.Context, int, uint32) ([]clickhouse.ContractDirectoryRow, error) {
	return []clickhouse.ContractDirectoryRow{{ContractID: validTestContract, Events: 3}}, nil
}

func newStatsHandler(typesErr error) (*Handler, *statsReader) {
	h, swr := newSWRHandler()
	r := &statsReader{capReader: swr.capReader, typesErr: typesErr}
	h.Reader = r
	return h, r
}

func TestBuildContractStatsView_TotalsAndLowerBound(t *testing.T) {
	r := &statsReader{}
	stats, _ := r.ContractStats(context.Background())
	v := buildContractStatsView(stats)
	if v.TotalSAC != 3 || v.TotalWasm != 7 || v.TotalDeployed != 10 {
		t.Fatalf("totals = %d/%d/%d, want 3/7/10", v.TotalSAC, v.TotalWasm, v.TotalDeployed)
	}
	if v.Deployments[0].Month != "2024-02" {
		t.Fatalf("month = %q, want 2024-02", v.Deployments[0].Month)
	}
	if !v.LowerBound || v.HistoryComplete {
		t.Fatalf("lower_bound=%v history_complete=%v, want a lower bound without a watermark", v.LowerBound, v.HistoryComplete)
	}
	stats.HistoryComplete = true
	if v = buildContractStatsView(stats); v.LowerBound {
		t.Fatal("lower_bound set despite a complete history")
	}
}

func TestContractStats_StaleServedDegradedAndRefreshed(t *testing.T) {
	h, r := newStatsHandler(nil)
	staleAt := time.Now().Add(-2 * contractStatsTTL)
	h.contractStats.put(contractStatsEntry{view: ContractStatsView{TotalDeployed: 1}, cachedAt: staleAt})

	e, ok, fresh := h.contractStats.get()
	if !ok || fresh || e.view.TotalDeployed != 1 {
		t.Fatalf("seed: ok=%v fresh=%v", ok, fresh)
	}
	h.refreshContractStats()
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	if got := r.statsCalls.Load(); got != 1 {
		t.Fatalf("refresh ran %d times, want 1", got)
	}
	e, ok, fresh = h.contractStats.get()
	if !ok || !fresh || e.view.TotalDeployed != 10 {
		t.Fatalf("after refresh: ok=%v fresh=%v total=%d, want fresh total 10", ok, fresh, e.view.TotalDeployed)
	}
}

func TestPrewarmContractStats_FillsThenNoops(t *testing.T) {
	h, r := newStatsHandler(nil)
	h.PrewarmContractStats(context.Background())
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	h.PrewarmContractStats(context.Background())
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	if got := r.statsCalls.Load(); got != 1 {
		t.Fatalf("prewarm ran the aggregation %d times, want 1", got)
	}
}

func TestContractsDir_TypesAttached(t *testing.T) {
	h, _ := newStatsHandler(nil)
	h.refreshContractsDir(30)
	waitFlightIdle(t, &h.contractsDir.flight, "30")
	_, types, _, _, _, err := h.recentContractsCached(context.Background(), 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := contractTypeLabel(types, validTestContract); got != "sac" {
		t.Fatalf("type = %q, want sac", got)
	}
	if got := contractTypeLabel(types, "CUNKNOWN"); got != "" {
		t.Fatalf("unknown id type = %q, want empty", got)
	}
}

func TestContractsDir_TypeLookupFailureKeepsDirectory(t *testing.T) {
	h, _ := newStatsHandler(errors.New("boom"))
	h.refreshContractsDir(30)
	waitFlightIdle(t, &h.contractsDir.flight, "30")
	rows, types, _, _, _, err := h.recentContractsCached(context.Background(), 30, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v, want the directory served", len(rows), err)
	}
	if got := contractTypeLabel(types, validTestContract); got != "" {
		t.Fatalf("type = %q, want empty after a lookup failure", got)
	}
}
