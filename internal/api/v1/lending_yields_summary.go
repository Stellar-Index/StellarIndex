package v1

import (
	"cmp"
	"context"
	"math/big"
	"net/http"
	"slices"
)

// LendingYieldsSummaryView is the GET /v1/lending/yields/summary body: every /v1/lending/rates row's yield block, per side,
// split into the rows that may be ranked (in the server's order) and those that may not, each with its reasons.
type LendingYieldsSummaryView struct {
	YieldWindow         string               `json:"yield_window"`
	APYConvention       string               `json:"apy_convention"`
	YieldHistoryThrough *WireTime            `json:"yield_history_through"`
	OmittedPools        []LendingOmittedPool `json:"omitted_pools"`
	Partial             bool                 `json:"partial"`
	// Order names how ranked is ordered: the exact net yield highest first, then pool id and asset.
	Order  string                   `json:"order"`
	Supply LendingYieldsSummarySide `json:"supply"`
	Borrow LendingYieldsSummarySide `json:"borrow"`
}

// LendingYieldsSummarySide is one side's table. Unrankable rows are never ranked last as if they were low: a yield that
// cannot be compared has no place in the order.
type LendingYieldsSummarySide struct {
	Ranked     []LendingYieldsSummaryRow `json:"ranked"`
	Unrankable []LendingYieldsSummaryRow `json:"unrankable"`
	Counts     LendingYieldsSummaryCount `json:"counts"`
}

// LendingYieldsSummaryCount counts a side's rows; ByReason counts the unrankable rows naming each reason.
type LendingYieldsSummaryCount struct {
	Ranked     int            `json:"ranked"`
	Unrankable int            `json:"unrankable"`
	ByReason   map[string]int `json:"by_reason"`
}

// LendingYieldsSummaryRow is one reserve side, copied from its /v1/lending/rates row. Rank is 1-based and null on an
// unrankable row.
type LendingYieldsSummaryRow struct {
	Rank        *int    `json:"rank"`
	Protocol    string  `json:"protocol"`
	PoolVersion *string `json:"pool_version"`
	Pool        string  `json:"pool"`
	Asset       string  `json:"asset"`
	AssetID     *string `json:"asset_id"`
	AssetCode   *string `json:"asset_code"`
	AssetIssuer *string `json:"asset_issuer"`
	// Exclusions are the /rates row's: why a new position may not earn the rate.
	Exclusions          []string  `json:"exclusions"`
	RealisedFrom        *WireTime `json:"realised_from"`
	RealisedTo          *WireTime `json:"realised_to"`
	RealisedSpanSeconds *int64    `json:"realised_span_seconds"`
	LendingSideYield
}

const lendingYieldsSummaryOrder = "net_yield_desc"

// handleLendingYieldsSummary serves GET /v1/lending/yields/summary from buildLendingRates, the path /v1/lending/rates
// serves its yield block from, under the same caches.
func (s *Server) handleLendingYieldsSummary(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	q, ok := parseLendingRateFilter(w, r)
	if !ok {
		return
	}
	rates := LendingRatesView{Rates: []LendingRate{}, OmittedPools: []LendingOmittedPool{}}
	var flags Flags
	var vintage dataVintage
	if s.Lending != nil {
		ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
		defer cancel()
		var err error
		if flags, err = s.buildLendingRates(ctx, q, &rates, &vintage); err != nil {
			s.writeListBlendPoolsError(ctx, w, r, err)
			return
		}
	}
	view := lendingYieldsSummary(rates, q.windowName)
	writeEnvelope(w, Envelope{Data: view, AsOf: vintage.asOf(), Flags: flags})
}

// lendingYieldsSummary splits the rate rows' yield blocks into each side's table.
func lendingYieldsSummary(rates LendingRatesView, window string) LendingYieldsSummaryView {
	out := LendingYieldsSummaryView{
		YieldWindow: window, APYConvention: apyConventionRealised, YieldHistoryThrough: rates.YieldHistoryThrough,
		OmittedPools: rates.OmittedPools, Partial: rates.Partial, Order: lendingYieldsSummaryOrder,
	}
	var supply, borrow []LendingYieldsSummaryRow
	for _, row := range rates.Rates {
		supply = append(supply, summaryRow(row, row.Yield.Supply))
		borrow = append(borrow, summaryRow(row, row.Yield.Borrow))
	}
	out.Supply, out.Borrow = summarySide(supply), summarySide(borrow)
	return out
}

func summaryRow(row LendingRate, side LendingSideYield) LendingYieldsSummaryRow {
	return LendingYieldsSummaryRow{
		Protocol: row.Protocol, PoolVersion: row.PoolVersion, Pool: row.Pool, Asset: row.Asset, AssetID: row.AssetID,
		AssetCode: row.AssetCode, AssetIssuer: row.AssetIssuer, Exclusions: row.Exclusions, RealisedFrom: row.Yield.RealisedFrom,
		RealisedTo: row.Yield.RealisedTo, RealisedSpanSeconds: row.Yield.RealisedSpanSeconds, LendingSideYield: side,
	}
}

// summarySide ranks the rankable rows by their exact net yield before rounding, ties by pool id then asset (each table is
// one side), and lists the rest in pool and asset order. A row without an exact net is listed unrankable, never ranked.
func summarySide(rows []LendingYieldsSummaryRow) LendingYieldsSummarySide {
	out := LendingYieldsSummarySide{
		Ranked: []LendingYieldsSummaryRow{}, Unrankable: []LendingYieldsSummaryRow{}, Counts: LendingYieldsSummaryCount{ByReason: map[string]int{}},
	}
	nets := map[int]*big.Rat{}
	var ranked []int
	for i, r := range rows {
		if n, ok := summaryNet(r); ok {
			nets[i] = n
			ranked = append(ranked, i)
			continue
		}
		out.Unrankable = append(out.Unrankable, r)
		for _, reason := range r.RankableReasons {
			out.Counts.ByReason[reason]++
		}
	}
	slices.SortStableFunc(ranked, func(a, b int) int {
		return cmp.Or(nets[b].Cmp(nets[a]), cmp.Compare(rows[a].Pool, rows[b].Pool), cmp.Compare(rows[a].Asset, rows[b].Asset))
	})
	for i, idx := range ranked {
		r, rank := rows[idx], i+1
		r.Rank = &rank
		out.Ranked = append(out.Ranked, r)
	}
	slices.SortStableFunc(out.Unrankable, func(a, b LendingYieldsSummaryRow) int {
		return cmp.Or(cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Asset, b.Asset))
	})
	out.Counts.Ranked, out.Counts.Unrankable = len(out.Ranked), len(out.Unrankable)
	return out
}

// summaryNet is a row's exact, unrounded net yield when the row may be ranked.
func summaryNet(r LendingYieldsSummaryRow) (*big.Rat, bool) {
	if !r.Rankable || r.netExact == nil {
		return nil, false
	}
	return r.netExact, true
}
