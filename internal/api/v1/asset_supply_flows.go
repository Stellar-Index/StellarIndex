package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"
)

// AssetSupplyFlows is the wire response for GET /v1/assets/{asset_id}/supply/flows:
// the token's mint / burn / clawback log bucketed by UTC day, from its first
// recorded flow. Amounts are base-unit decimal strings (ADR-0003).
type AssetSupplyFlows struct {
	AssetID    string               `json:"asset_id"`
	ContractID string               `json:"contract_id"`
	Days       []AssetSupplyFlowDay `json:"days"`
	// HistoryIncomplete is set when the running Σmint − Σ(burn+clawback)
	// dips below zero on some day: earlier mints are missing from the lake,
	// so the series must not be cumulated into a supply level.
	HistoryIncomplete bool   `json:"history_incomplete"`
	AsOfLedger        uint32 `json:"as_of_ledger,omitempty"`
}

// AssetSupplyFlowDay is one UTC day's flows. A day with no flows has no row.
type AssetSupplyFlowDay struct {
	Day      string `json:"day"` // YYYY-MM-DD, UTC
	Mint     string `json:"mint"`
	Burn     string `json:"burn"`
	Clawback string `json:"clawback"`
	Net      string `json:"net"` // mint − burn − clawback; signed
	Flows    uint64 `json:"flows"`
}

// handleAssetSupplyFlows serves GET /v1/assets/{asset_id}/supply/flows from
// the same supply_flows lake as /supply, through the daily-flows seam.
func (s *Server) handleAssetSupplyFlows(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tokenSupply.(rwaSupplyFlowHistoryReader)
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/supply-unavailable",
			"Supply unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the ClickHouse supply reader yet.")
		return
	}
	assetID := r.PathValue("asset_id")
	if isNativeSupplyAlias(assetID) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-not-mapped",
			"Supply flows not available", http.StatusNotFound,
			"XLM has no mint/burn log; its supply is the ledger header's total_coins, served at /v1/assets/native/supply.")
		return
	}
	contractID, ok := s.resolveSupplyContractID(assetID)
	if !ok {
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-not-mapped",
			"Supply flows not available", http.StatusNotFound,
			"Supply flows are keyed by contract: Soroban tokens (C…) resolve directly and classic assets (CODE-ISSUER) derive their Stellar-Asset-Contract; this id has no contract.")
		return
	}

	// Same 8s ceiling as /supply: one PK-prefix-scoped GROUP BY on one contract.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	days, err := reader.DailySupplyFlowsForContracts(ctx, []string{contractID})
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Warn("supply flows: daily read", "contract_id", contractID, "err", err)
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
			"Supply read failed", http.StatusBadGateway, "Could not read token supply flows.")
		return
	}

	out := AssetSupplyFlows{AssetID: assetID, ContractID: contractID, Days: make([]AssetSupplyFlowDay, 0, len(days))}
	level := new(big.Int)
	for _, d := range days {
		level.Add(level, d.Net)
		if level.Sign() < 0 {
			out.HistoryIncomplete = true
		}
		out.Days = append(out.Days, AssetSupplyFlowDay{
			Day:      d.Day.UTC().Format(time.DateOnly),
			Mint:     d.Mint.String(),
			Burn:     d.Burn.String(),
			Clawback: d.Clawback.String(),
			Net:      d.Net.String(),
			Flows:    d.Flows,
		})
	}
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	out.AsOfLedger = wmLedger
	writeJSON(w, out, Flags{Stale: stale})
}
