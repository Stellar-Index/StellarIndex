package domain

import (
	"math/big"
	"time"
)

// Blend money-market / credit-risk / admin event-kind values —
// topic[0] of the corresponding Blend pool event, as persisted in
// the `event_kind` column of blend_positions / blend_emissions /
// blend_admin. Canonical home of the matching
// internal/sources/blend.EventXxx constants — see doc.go. (The three
// AUCTION event kinds — new_auction / fill_auction / delete_auction —
// stay defined only in internal/sources/blend: blend_auctions.go
// also needs blend.ParseReserveConfigMetadata and blend.ReserveConfig
// (the latter shared with internal/api/v1's lending surface, methods
// and all), so that storage file keeps its blend import regardless —
// moving just the auction consts would not shrink its baseline entry.)
const (
	// Money-market events.
	BlendEventSupply             = "supply"
	BlendEventWithdraw           = "withdraw"
	BlendEventSupplyCollateral   = "supply_collateral"
	BlendEventWithdrawCollateral = "withdraw_collateral"
	BlendEventBorrow             = "borrow"
	BlendEventRepay              = "repay"
	BlendEventFlashLoan          = "flash_loan"
	BlendEventGulp               = "gulp"
	BlendEventClaim              = "claim"

	// Credit-risk + emissions events.
	BlendEventBadDebt          = "bad_debt"
	BlendEventDefaultedDebt    = "defaulted_debt"
	BlendEventReserveEmissions = "reserve_emission_update"
	BlendEventGulpEmissions    = "gulp_emissions"

	// Admin / status events.
	BlendEventSetAdmin         = "set_admin"
	BlendEventUpdatePool       = "update_pool"
	BlendEventQueueSetReserve  = "queue_set_reserve"
	BlendEventCancelSetReserve = "cancel_set_reserve"
	BlendEventSetReserve       = "set_reserve"
	BlendEventSetStatus        = "set_status"

	// Pool-factory event.
	BlendEventDeploy = "deploy"

	// V1 pool-factory (CCZD6ESM…) events — ROADMAP #89 residual
	// (2026-07-10). The V1 factory's pools speak a simpler,
	// different vocabulary than the V2 events above (no auction_type
	// discriminator, no percent field); real-lake-bytes verified at
	// ledgers 51,524,668 / 51,611,821 / 54,890,906. See
	// internal/sources/blend/README.md "Known gap" for the full
	// evidence trail.
	//
	// BlendEventUpdateEmissions lands in blend_emissions — a
	// pool-wide emissions total (bare i128 body), a different concept
	// from V2's per-reserve reserve_emission_update.
	BlendEventUpdateEmissions = "update_emissions"
	// BlendEventNewLiquidationAuction / BlendEventDeleteLiquidationAuction
	// land in blend_admin (not blend_auctions) — the V1 body carries the
	// same AuctionData {bid, lot, block} Map shape as V2's AuctionData,
	// but WITHOUT an auction_type topic to classify it against the V2
	// UserLiquidation/BadDebt/Interest taxonomy, so it is stored as an
	// admin/lifecycle event (Target=user, attributes={bid,lot,block})
	// rather than guessing an auction_type for the blend_auctions CHECK.
	BlendEventNewLiquidationAuction    = "new_liquidation_auction"
	BlendEventDeleteLiquidationAuction = "delete_liquidation_auction"
)

// BlendPositionEvent is the decoded shape of every money-market event
// that changes a (user, asset, pool) position: supply / withdraw /
// supply_collateral / withdraw_collateral / borrow / repay /
// flash_loan. Canonical home of
// internal/sources/blend.PositionEvent — see doc.go.
type BlendPositionEvent struct {
	Pool string // emitting pool contract C-strkey
	Kind string // one of the seven money-market event-kind constants

	Asset        string // topic[1] asset Address (G or C)
	User         string // topic[2] from / user Address (G or C)
	Counterparty string // flash_loan only: topic[3] borrowing contract; "" otherwise

	TokenAmount *big.Int // body[0]: tokens_in / tokens_out i128
	BOrDAmount  *big.Int // body[1]: b_or_d_tokens minted / burnt i128

	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Timestamp  time.Time
}

// BlendEmissionEvent is the decoded shape of the emission /
// credit-risk events (gulp / claim / reserve_emission_update /
// gulp_emissions / bad_debt / defaulted_debt). Canonical home of
// internal/sources/blend.EmissionEvent — see doc.go.
type BlendEmissionEvent struct {
	Pool string
	Kind string

	Asset string
	User  string

	Amount *big.Int // primary i128 amount (per-kind mapping — see blend.EmissionEvent)

	// reserve_emission_update extras (zero for everything else).
	ResTokenID      uint32
	EmissionsPerSec uint64
	Expiration      uint64

	// claim extras (nil for everything else).
	ReserveTokenIDs []uint32

	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Timestamp  time.Time
}

// BlendAdminEvent is the decoded shape of every pool-config / admin /
// pool-factory lifecycle event: set_admin, update_pool,
// queue_set_reserve, cancel_set_reserve, set_reserve, set_status,
// deploy, new_liquidation_auction, delete_liquidation_auction.
// Canonical home of internal/sources/blend.AdminEvent — see doc.go.
type BlendAdminEvent struct {
	ContractID string
	Kind       string

	Admin  string
	Asset  string
	Target string

	// update_pool body fields.
	BackstopTakeRate uint32
	MaxPositions     uint32
	MinCollateral    *big.Int // i128 per ADR-0003

	// set_reserve body field; queue_set_reserve.metadata.index.
	ReserveIndex uint32

	// set_status body field.
	NewStatus uint32
	ByAdmin   bool

	// queue_set_reserve.metadata — full ReserveConfig, kept as a map
	// for round-trip parity with the on-wire struct (the storage
	// layer marshals it to jsonb). Nil when the event kind doesn't
	// carry a ReserveConfig.
	ReserveConfig map[string]any

	// new_liquidation_auction body fields (V1 pool-factory only — see
	// BlendEventNewLiquidationAuction doc). Same {bid, lot, block}
	// shape as V2's AuctionData; Target carries the user (topic[1]).
	// Nil/zero for every other event kind, including
	// delete_liquidation_auction (empty body on the wire).
	AuctionBid   []BlendAssetAmount
	AuctionLot   []BlendAssetAmount
	AuctionBlock uint32

	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Timestamp  time.Time
}

// BlendAssetAmount is one (asset, amount) pair from a V1
// new_liquidation_auction's bid/lot map. Domain-level mirror of
// internal/sources/blend.AssetAmount — declared separately here
// (rather than shared) because domain sits BENEATH internal/sources/
// blend in the import graph and cannot import it (see BlendAdminEvent
// M0-1 doc / PositionEvent doc for the same pattern).
type BlendAssetAmount struct {
	Asset  string
	Amount *big.Int // i128 per ADR-0003
}
