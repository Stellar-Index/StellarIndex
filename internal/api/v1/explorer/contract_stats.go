package explorer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// contractStatsTTL is long: the deployment histogram is a full-table aggregate (seconds of
// scan) and a month-bucketed series gains nothing from fresher data.
const contractStatsTTL = time.Hour

const contractStatsRefreshTimeout = 3 * time.Minute

const contractStatsKey = "stats"

// ContractDeploymentMonth is one month of the deployments series.
type ContractDeploymentMonth struct {
	Month string `json:"month"`
	SAC   int64  `json:"sac"`
	Wasm  int64  `json:"wasm"`
}

// ContractStatsView is the wire response for GET /v1/contracts/stats.
type ContractStatsView struct {
	Deployments     []ContractDeploymentMonth `json:"deployments"`
	TotalDeployed   int64                     `json:"total_deployed"`
	TotalSAC        int64                     `json:"total_sac"`
	TotalWasm       int64                     `json:"total_wasm"`
	Active90d       *int64                    `json:"active_90d"`
	HistoryComplete bool                      `json:"history_complete"`
	// LowerBound is set while the instance history is not genesis-complete: the totals
	// then exclude every contract deployed before the backfilled range.
	LowerBound bool `json:"lower_bound"`
}

type contractStatsEntry struct {
	view     ContractStatsView
	cachedAt time.Time
}

// contractStatsCache is the single-entry cache behind GET /v1/contracts/stats. Zero value ready.
type contractStatsCache struct {
	mu     sync.Mutex
	entry  *contractStatsEntry
	flight perKeyFlight
}

func (c *contractStatsCache) get() (e contractStatsEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry == nil {
		return contractStatsEntry{}, false, false
	}
	return *c.entry, true, time.Since(c.entry.cachedAt) <= contractStatsTTL
}

func (c *contractStatsCache) put(e contractStatsEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = &e
}

func buildContractStatsView(s clickhouse.ContractStats) ContractStatsView {
	v := ContractStatsView{
		Deployments:     make([]ContractDeploymentMonth, len(s.Deployments)),
		Active90d:       s.Active90d,
		HistoryComplete: s.HistoryComplete,
		LowerBound:      !s.HistoryComplete,
	}
	for i, d := range s.Deployments {
		v.Deployments[i] = ContractDeploymentMonth{Month: d.Month.UTC().Format("2006-01"), SAC: d.SAC, Wasm: d.Wasm}
		v.TotalSAC += d.SAC
		v.TotalWasm += d.Wasm
	}
	v.TotalDeployed = v.TotalSAC + v.TotalWasm
	return v
}

// refreshContractStats kicks ONE detached stats aggregation (no-op when one is already
// running) and returns its flight; same detach rationale as refreshContractsDir.
func (h *Handler) refreshContractStats() *keyFlight {
	fl, owner := h.contractStats.flight.begin(contractStatsKey)
	if !owner {
		return fl
	}
	gate := h.detachedGate()
	if !gate.TryAcquireClass("contract_stats") {
		h.contractStats.flight.end(contractStatsKey, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("contract_stats")
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-contract-stats-refresh", rec)
				err = errRefreshPanicked
			}
			h.contractStats.flight.end(contractStatsKey, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), contractStatsRefreshTimeout)
		defer cancel()
		stats, err := h.Reader.ContractStats(rctx)
		obs.ObserveExplorerSWRRefresh("contract_stats", start, err)
		if err != nil {
			h.Logger.Warn("contract stats detached refresh failed", "err", err)
			return
		}
		h.contractStats.put(contractStatsEntry{view: buildContractStatsView(stats), cachedAt: time.Now()})
	}()
	return fl
}

// PrewarmContractStats keeps the stats snapshot warm; called from the API prewarm loop.
func (h *Handler) PrewarmContractStats(ctx context.Context) {
	if h.Reader == nil || ctx.Err() != nil {
		return
	}
	if _, _, fresh := h.contractStats.get(); fresh {
		return
	}
	h.refreshContractStats() //nolint:contextcheck // intentional detach — prewarm kicks the same background compute
}

// ContractStats serves GET /v1/contracts/stats: deployments per month, totals, and the
// 90-day active count. Snapshot-served; a stale entry is returned degraded while a
// detached refresh runs.
func (h *Handler) ContractStats(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	if e, ok, fresh := h.contractStats.get(); ok {
		if !fresh {
			h.refreshContractStats() //nolint:contextcheck // intentional detach — see refreshContractStats
		}
		h.writeJSONAt(w, e.view, !fresh, !fresh, e.cachedAt)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	fl := h.refreshContractStats() //nolint:contextcheck // intentional detach — see refreshContractStats
	var err error
	select {
	case <-fl.done:
		if e, ok, _ := h.contractStats.get(); ok {
			h.writeJSONAt(w, e.view, false, false, e.cachedAt)
			return
		}
		if err = fl.err; err == nil {
			err = errRefreshFailed
		}
	case <-ctx.Done():
		err = ctx.Err()
	}
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/contracts-timeout",
			"Contract stats timed out")
		return
	}
	h.Logger.Error("explorer ContractStats failed", "err", err)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
}
