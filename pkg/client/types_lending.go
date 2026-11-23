package client

import "time"

// LendingPool is one Blend or BLNT pool (Protocol says which) from [Client.LendingPools]; the shape is designed to grow with new fields.
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

// LendingTopAccountsQuery is the input for [Client.LendingPoolTopAccounts]; Side is "supply" or "borrow", Asset (optional) a
// reserve token C-strkey that switches the ranking to amount within that reserve, Limit 1..100 (0 = server default 20).
type LendingTopAccountsQuery struct {
	Side  string
	Asset string
	Limit int
}

// LendingTopAccounts is one pool's largest positions on one side; LowerBound means Rows and Accounts omit what Excluded and
// Unattributed name.
type LendingTopAccounts struct {
	Pool         string                           `json:"pool"`
	Protocol     string                           `json:"protocol"`
	Side         string                           `json:"side"`
	Asset        *string                          `json:"asset"`
	Ranking      string                           `json:"ranking"` // value_usd | amount
	Accounts     int                              `json:"accounts"`
	LowerBound   bool                             `json:"lower_bound"`
	Excluded     []LendingTopAccountsExcluded     `json:"excluded"`
	Unattributed []LendingTopAccountsUnattributed `json:"unattributed"`
	Rows         []LendingTopAccount              `json:"rows"`
	AsOfLedger   uint32                           `json:"as_of_ledger"`
	Note         string                           `json:"note"`
}

// LendingTopAccountsUnattributed is the part of a reserve's side total no read position holds; decimal strings, share 6 dp.
type LendingTopAccountsUnattributed struct {
	Asset          string `json:"asset"`
	Side           string `json:"side"`
	Tokens         string `json:"tokens"`
	Amount         string `json:"amount"`
	ShareOfReserve string `json:"share_of_reserve"`
}

// LendingTopAccountsExcluded names positions left out of a ranking and why.
type LendingTopAccountsExcluded struct {
	Kind   string  `json:"kind"`
	Pool   *string `json:"pool"`
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
}

// LendingTopAccount is one account's position in one reserve; Tokens, Amount, ValueUSD and ShareOfReserve are decimal strings.
type LendingTopAccount struct {
	Rank               int     `json:"rank"`
	Account            string  `json:"account"`
	Asset              string  `json:"asset"`
	Decimals           *int    `json:"decimals"`
	Tokens             string  `json:"tokens"`
	Amount             string  `json:"amount"`
	ValueUSD           *string `json:"value_usd"`
	UsdReason          *string `json:"usd_reason"`
	ShareOfReserve     *string `json:"share_of_reserve"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
	RankLowerBound     bool    `json:"rank_lower_bound,omitempty"` // an unread position could rank above this row
}
