package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// LendingLiquidations is a pool's liquidation fills from [Client.LendingPoolLiquidations], each valued at the fill
// (ValuedAt "event_time"). Amounts and USD values are decimal strings; a nil value carries its Reason.
type LendingLiquidations struct {
	Pool               string               `json:"pool"`
	ValuedAt           string               `json:"valued_at"`
	PriceMaxAgeSeconds int                  `json:"price_max_age_seconds"`
	Liquidations       []LendingLiquidation `json:"liquidations"`
}

// LendingLiquidation is one user-liquidation or bad-debt fill: Lot is what the filler received, Bid what it repaid.
// FillerProfitUSD is set only when both sides are fully valued at prices within an hour of each other.
type LendingLiquidation struct {
	AuctionType      uint32                  `json:"auction_type"`
	AuctionTypeLabel string                  `json:"auction_type_label"`
	User             string                  `json:"user"`
	Filler           string                  `json:"filler"`
	FillPercent      string                  `json:"fill_percent"`
	Ledger           uint32                  `json:"ledger"`
	Time             time.Time               `json:"time"`
	TxHash           string                  `json:"tx_hash"`
	Block            *uint32                 `json:"block"`
	AmountsRecorded  bool                    `json:"amounts_recorded"`
	Lot              []LendingLiquidationLeg `json:"lot"`
	Bid              []LendingLiquidationLeg `json:"bid"`
	LotUSD           *string                 `json:"lot_usd"`
	LotUSDLowerBound bool                    `json:"lot_usd_lower_bound"`
	BidUSD           *string                 `json:"bid_usd"`
	BidUSDLowerBound bool                    `json:"bid_usd_lower_bound"`
	FillerProfitUSD  *string                 `json:"filler_profit_usd"`
	// FillerProfitReason is inputs_partial or prices_not_contemporaneous when FillerProfitUSD is null on a fill with amounts.
	FillerProfitReason *string `json:"filler_profit_reason"`
	USDReason          *string `json:"usd_reason"`
}

// LendingLiquidationLeg is one asset of a fill's side, valued at the fill.
type LendingLiquidationLeg struct {
	Asset      string     `json:"asset"`
	Kind       string     `json:"kind"`
	Amount     string     `json:"amount"`
	Underlying *string    `json:"underlying"`
	PriceUSD   *string    `json:"price_usd"`
	PriceAt    *time.Time `json:"price_at"`
	USD        *string    `json:"usd"`
	Reason     *string    `json:"reason"`
}

// LendingPoolLiquidations fetches a pool's liquidation fills newest first (GET /v1/lending/pools/{pool}/liquidations); pass a
// prior Pagination.Next as cursor, and a zero limit for the server default.
func (c *Client) LendingPoolLiquidations(ctx context.Context, pool, cursor string, limit int) (*Envelope[LendingLiquidations], error) {
	v := url.Values{}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var env Envelope[LendingLiquidations]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/lending/pools/"+url.PathEscape(pool)+"/liquidations", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
