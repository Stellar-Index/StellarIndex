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
// deploy. Canonical home of internal/sources/blend.AdminEvent — see
// doc.go.
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

	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Timestamp  time.Time
}
