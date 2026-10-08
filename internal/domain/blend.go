package domain

import (
	"math/big"
	"time"
)

// Blend event kinds: topic[0] of the pool event, stored in event_kind of
// blend_positions / blend_emissions / blend_admin. The auction kinds stay in
// internal/sources/blend, whose other types blend_auctions.go imports anyway.
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

	// V1 pool-factory (CCZD6ESM…) events: no auction_type or percent field
	// (internal/sources/blend/README.md "Known gap"). update_emissions is a
	// pool-wide total, unlike V2's per-reserve reserve_emission_update.
	BlendEventUpdateEmissions = "update_emissions"
	// V1 liquidation auctions go to blend_admin, not blend_auctions: without an
	// auction_type topic they cannot satisfy that table's CHECK, so we store
	// them as lifecycle events rather than guess a type.
	BlendEventNewLiquidationAuction    = "new_liquidation_auction"
	BlendEventDeleteLiquidationAuction = "delete_liquidation_auction"
)

// BlendPositionEvent is a money-market event that changes a (user, asset, pool)
// position (origin: internal/sources/blend.PositionEvent).
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

// BlendEmissionEvent is an emission or credit-risk event
// (origin: internal/sources/blend.EmissionEvent).
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

// BlendAdminEvent is a pool-config, admin or factory lifecycle event
// (origin: internal/sources/blend.AdminEvent).
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

	// queue_set_reserve.metadata as a map, stored as jsonb for round-trip parity
	// with the on-wire struct; nil for other kinds.
	ReserveConfig map[string]any
	// ReserveConfigMissing names the V2-only ReserveConfig fields
	// (supply_cap, enabled) absent from a V1 pool's event.
	ReserveConfigMissing []string

	// V1 new_liquidation_auction body; Target carries the user (topic[1]).
	// Zero for every other kind, including delete_liquidation_auction.
	AuctionBid   []BlendAssetAmount
	AuctionLot   []BlendAssetAmount
	AuctionBlock uint32

	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Timestamp  time.Time
}

// BlendAssetAmount is one (asset, amount) pair of a V1 auction's bid or lot;
// it mirrors blend.AssetAmount because domain cannot import sources.
type BlendAssetAmount struct {
	Asset  string
	Amount *big.Int // i128 per ADR-0003
}
