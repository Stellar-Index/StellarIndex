package client

import "time"

// VWAPResult is the data shape of [Client.VWAP]; Truncated means the window exceeded the server's 10000-trade cap, the OLDEST trades were dropped and Price is NOT the true window VWAP, and volumes are smallest-unit integers at a per-SOURCE scale (divide by 10^BaseVolumeDecimals).
type VWAPResult struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Price       string    `json:"price"`
	BaseVolume  string    `json:"base_volume"`
	QuoteVolume string    `json:"quote_volume"`
	// nil when a contributing trade's source is unrecognised: the sums
	// are then not convertible to asset units.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	OutliersFiltered    int  `json:"outliers_filtered"`
	Truncated           bool `json:"truncated"`
	// Clamped: see [OHLCBar.Clamped].
	Clamped bool `json:"clamped"`
	// Breakdown is set only for a `breakdown=source` request.
	Breakdown *VWAPBreakdown `json:"breakdown,omitempty"`
	// Substance is the measurement behind a Flags.ThinMarket result.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// VWAPBreakdown mirrors `internal/api/v1.VWAPBreakdown`.
type VWAPBreakdown struct {
	Interval  *string               `json:"interval"`
	Truncated bool                  `json:"truncated"`
	Buckets   []VWAPBreakdownBucket `json:"buckets"`
}

// VWAPBreakdownBucket is one time bucket of a [VWAPBreakdown].
type VWAPBreakdownBucket struct {
	Start       time.Time             `json:"start"`
	End         time.Time             `json:"end"`
	QuoteVolume string                `json:"quote_volume"`
	TradeCount  int                   `json:"trade_count"`
	Sources     []VWAPSourceBreakdown `json:"sources"`
}

// VWAPSourceBreakdown is one venue's share of a [VWAPBreakdownBucket].
type VWAPSourceBreakdown struct {
	Source           string  `json:"source"`
	Price            *string `json:"price"`
	BaseVolume       string  `json:"base_volume"`
	QuoteVolume      string  `json:"quote_volume"`
	TradeCount       int     `json:"trade_count"`
	Weight           string  `json:"weight"`
	OutliersExcluded int     `json:"outliers_excluded"`
}

// TWAPResult is the data shape of [Client.TWAP]; TradeCount counts trades that carried weight and OutliersFiltered those the default outlier filter removed.
type TWAPResult struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Price            string    `json:"price"`
	TradeCount       int       `json:"trade_count"`
	OutliersFiltered int       `json:"outliers_filtered"`
	Truncated        bool      `json:"truncated"`
	// Clamped: see [OHLCBar.Clamped].
	Clamped bool `json:"clamped"`
	// Substance is the measurement behind a Flags.ThinMarket result.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// Pool is one (source, base, quote) row from [Client.Pools], unlike [Market] which collapses across sources; LastPrice is per-source.
type Pool struct {
	Source        string    `json:"source"`
	Base          string    `json:"base"`
	Quote         string    `json:"quote"`
	LastTradeAt   time.Time `json:"last_trade_at"`
	TradeCount24h int64     `json:"trade_count_24h"`
	Volume24hUSD  *string   `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound is true when Volume24hUSD excludes unpriced trades.
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"`
	LastPrice        *string `json:"last_price,omitempty"`
}
