package v1_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// gatedExplorer parks every reserves build until gate closes and counts builds; fail makes them error.
type gatedExplorer struct {
	*stubExplorerReader
	gate   chan struct{}
	fail   atomic.Bool
	mu     sync.Mutex
	builds int
}

func (g *gatedExplorer) BlendPoolReserves(ctx context.Context, _ string, _ blend.PoolVersion, _ []string, _ map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	g.mu.Lock()
	g.builds++
	g.mu.Unlock()
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if g.fail.Load() {
		return nil, errors.New("lake down")
	}
	return nil, nil
}

func (g *gatedExplorer) buildCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.builds
}

type failingConfigsReader struct{ stubLendingReader }

func (r *failingConfigsReader) BlendReserveConfigs(context.Context, string) (map[string]blend.ReserveConfig, error) {
	return nil, errors.New("configs unavailable")
}

type poolsList struct {
	cacheControl string
	rows         []v1.LendingPool
	flags        v1.Flags
}

func getPoolsList(t *testing.T, base string) poolsList {
	t.Helper()
	resp := mustGet(t, base+"/v1/lending/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data  []v1.LendingPool `json:"data"`
		Flags v1.Flags         `json:"flags"`
	}
	mustDecode(t, resp, &body)
	return poolsList{cacheControl: resp.Header.Get("Cache-Control"), rows: body.Data, flags: body.Flags}
}

// pollPoolsList re-reads the list until done holds; fills run detached, so the test waits on the wire.
func pollPoolsList(t *testing.T, base string, done func(poolsList) bool) poolsList {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := getPoolsList(t, base)
		if done(l) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("list never settled: %+v", l)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func noPending(l poolsList) bool {
	for _, r := range l.rows {
		if r.SummaryPending {
			return false
		}
	}
	return true
}

func blendPools(t *testing.T, n int) []timescale.BlendPoolSummary {
	t.Helper()
	out := make([]timescale.BlendPoolSummary, n)
	for i := range out {
		out[i] = timescale.BlendPoolSummary{Pool: mkCStrkey(t, byte(40+i))}
	}
	return out
}

// The list never waits on a reserves build: cold pools come back pending and uncacheable, then summarised once the fills land.
func TestLendingPools_ColdSummariesDoNotBlock(t *testing.T) {
	explorer := &gatedExplorer{stubExplorerReader: &stubExplorerReader{}, gate: make(chan struct{})}
	srv := v1.New(v1.Options{Explorer: explorer, Lending: &stubLendingReader{pools: blendPools(t, 16)}})
	base := httpTestServer(t, srv).URL

	start := time.Now()
	cold := getPoolsList(t, base)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("list took %v with every build parked", d)
	}
	if cold.cacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store while a row is pending", cold.cacheControl)
	}
	for _, r := range cold.rows {
		if !r.SummaryPending || r.Summary != nil || r.SummaryError {
			t.Fatalf("row %s = pending %v summary %v error %v, want pending only", r.Pool, r.SummaryPending, r.Summary, r.SummaryError)
		}
	}
	getPoolsList(t, base)
	close(explorer.gate)
	warm := pollPoolsList(t, base, noPending)
	for _, r := range warm.rows {
		if r.Summary == nil || r.Summary.ReserveCount != 0 {
			t.Fatalf("row %s summary = %+v, want an empty pool's summary", r.Pool, r.Summary)
		}
	}
	if warm.cacheControl == "no-store" {
		t.Error("a fully summarised list must be cacheable")
	}
	if n := explorer.buildCount(); n != 16 {
		t.Errorf("builds = %d, want 16 (one per pool across repeated lists)", n)
	}
}

// Without a reserves reader there is nothing to build: rows carry no summary and are not pending.
func TestLendingPools_NoExplorerNoSummary(t *testing.T) {
	srv := v1.New(v1.Options{Lending: &stubLendingReader{pools: blendPools(t, 2)}})
	l := getPoolsList(t, httpTestServer(t, srv).URL)
	for _, r := range l.rows {
		if r.Summary != nil || r.SummaryPending || r.SummaryError {
			t.Fatalf("row = %+v, want no summary state", r)
		}
	}
	if l.cacheControl == "no-store" {
		t.Error("list without summaries must stay cacheable")
	}
}

// A failed first build marks the row errored, not pending, and the list neither rebuilds it nor turns uncacheable.
func TestLendingPools_FailedBuildIsHeldOff(t *testing.T) {
	explorer := &gatedExplorer{stubExplorerReader: &stubExplorerReader{}}
	explorer.fail.Store(true)
	srv := v1.New(v1.Options{Explorer: explorer, Lending: &stubLendingReader{pools: blendPools(t, 1)}})
	base := httpTestServer(t, srv).URL

	l := pollPoolsList(t, base, func(l poolsList) bool { return l.rows[0].SummaryError })
	if l.rows[0].SummaryPending || l.rows[0].Summary != nil {
		t.Errorf("row = %+v, want error only", l.rows[0])
	}
	if l.cacheControl == "no-store" {
		t.Error("an errored row must not make the list uncacheable")
	}
	getPoolsList(t, base)
	if n := explorer.buildCount(); n != 1 {
		t.Errorf("builds = %d, want 1 inside the retry window", n)
	}
}

// Each used entry's own disclosure reaches the list: a stale lake watermark, and failed rate configs (Degraded, sent as no-store).
func TestLendingPools_SummaryFlagsReachList(t *testing.T) {
	srv := v1.New(v1.Options{
		Explorer:      &gatedExplorer{stubExplorerReader: &stubExplorerReader{}},
		Lending:       &failingConfigsReader{stubLendingReader{pools: blendPools(t, 1)}},
		LakeWatermark: &wmStub{ledger: 63_500_000, closedAt: time.Now().Add(-10 * time.Minute)},
	})
	l := pollPoolsList(t, httpTestServer(t, srv).URL, noPending)
	if !l.flags.Stale {
		t.Error("flags.stale should carry the entry's stale lake watermark")
	}
	if l.cacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for an entry degraded by failed rate configs", l.cacheControl)
	}
}
