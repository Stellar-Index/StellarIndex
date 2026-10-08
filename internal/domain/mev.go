package domain

import "time"

// MEVOracleRef is one oracle_updates row for the ordering-aware MEV detectors.
type MEVOracleRef struct {
	Source     string
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	Asset      string
	Quote      string
	Timestamp  time.Time
}

// MEVAuctionFill is one liquidation fill from blend_auctions for the cascade detector.
type MEVAuctionFill struct {
	Pool        string
	User        string
	Filler      string
	AuctionType int16 // 0=UserLiquidation, 1=BadDebt
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	Timestamp   time.Time
	// Assets are the distinct reserve assets in the fill's bid + lot:
	// the liquidated position's debt and collateral.
	Assets []string
}

// MEVStoredEvent is a detected MEV candidate as written to mev_events.
type MEVStoredEvent struct {
	Kind             string
	Ledger           uint32
	DetectedAtLedger uint32
	Timestamp        time.Time
	// AssetID / QuoteID are the event's primary asset and quote ("" →
	// stored NULL, for cross-asset kinds). Notional stays in DetailJSON:
	// it is trade size, not the profit estimate mev_events.profit_usd
	// holds, which no detector computes.
	AssetID    string
	QuoteID    string
	TxHashes   []string
	Accounts   []string
	DedupKey   string
	DetailJSON []byte
}
