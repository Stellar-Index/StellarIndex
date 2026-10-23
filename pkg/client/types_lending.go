package client

import "time"

// LendingPool is one Blend pool from [Client.LendingPools] observed in the trailing 7d auction stream; the shape is designed to grow with new fields.
type LendingPool struct {
	Protocol       string    `json:"protocol"`
	Pool           string    `json:"pool"`
	Auctions24h    int64     `json:"auctions_24h"`
	AuctionsTotal  int64     `json:"auctions_total"`
	UniqueUsers30d int64     `json:"unique_users_30d"`
	LastSeen       time.Time `json:"last_seen"`
	// NetSupplied30d / NetBorrowed30d are 30-day NET-FLOW proxies in
	// token base-units (decimal strings) — not all-time TVL or
	// current reserve balances. nil when the window's flows span more
	// than one reserve asset, whose base units do not add.
	NetSupplied30d *string `json:"net_supplied_30d"`
	NetBorrowed30d *string `json:"net_borrowed_30d"`
	// Utilization30dPct is the single-asset borrow/supply window
	// ratio; nil when either flow is nil or net supply ≤ 0.
	Utilization30dPct *float64 `json:"utilization_30d_pct,omitempty"`
	// Summary totals the pool's current reserves view; nil while that view is still being built (SummaryPending) or its last build
	// failed (SummaryError).
	Summary        *LendingPoolSummary `json:"summary"`
	SummaryPending bool                `json:"summary_pending"`
	SummaryError   bool                `json:"summary_error"`
}

// LendingPoolSummary is one pool's current-state totals; USD values are decimal strings, and utilization and APRs cover priced reserves only.
type LendingPoolSummary struct {
	TVLUSD             *string  `json:"tvl_usd"`
	LowerBound         bool     `json:"lower_bound"`
	BorrowedUSD        *string  `json:"borrowed_usd"`
	BorrowedLowerBound bool     `json:"borrowed_lower_bound"`
	UtilizationPct     *float64 `json:"utilization_pct"`
	//floatmoney:ok supplied-weighted yield rate, not an amount
	SupplyAPR          *float64 `json:"supply_apr"`
	BorrowAPR          *float64 `json:"borrow_apr"`
	PricedReserveCount int      `json:"priced_reserve_count"`
	ReserveCount       int      `json:"reserve_count"`
}
