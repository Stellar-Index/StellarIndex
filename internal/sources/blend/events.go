// Package blend ingests events from the Blend Capital Soroban lending protocol.
//
// Blend is not a spot venue: auctions and position changes are NOT emitted as canonical.Trade; the
// sink writes them to per-protocol Blend storage. Auction stress-prices serve only as reference
// points on the asset detail surface and never enter VWAP.
//
// Verified against pool/src/events.rs and pool-factory/src/events.rs in blend-contracts.
package blend

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the stable source name used in metrics labels, registry keys and storage rows.
const SourceName = "blend"

// Event names: topic[0] of every Blend pool / pool-factory event, as a Soroban Symbol.
//
// Non-auction kinds are also the persisted `event_kind` in the blend_* tables, so they live in
// [domain.BlendEventSupply] and siblings (storage validates against them and cannot import this
// package); these alias those. The auction consts have no storage-side switch.
const (
	// Auction events (PRIMARY — directional price signals).
	EventNewAuction    = "new_auction"
	EventFillAuction   = "fill_auction"
	EventDeleteAuction = "delete_auction"

	// Money-market events (SECONDARY — supply/borrow tallies).
	EventSupply             = domain.BlendEventSupply
	EventWithdraw           = domain.BlendEventWithdraw
	EventSupplyCollateral   = domain.BlendEventSupplyCollateral
	EventWithdrawCollateral = domain.BlendEventWithdrawCollateral
	EventBorrow             = domain.BlendEventBorrow
	EventRepay              = domain.BlendEventRepay
	EventFlashLoan          = domain.BlendEventFlashLoan
	EventGulp               = domain.BlendEventGulp
	EventClaim              = domain.BlendEventClaim

	// Credit-risk + emissions events.
	EventBadDebt          = domain.BlendEventBadDebt
	EventDefaultedDebt    = domain.BlendEventDefaultedDebt
	EventReserveEmissions = domain.BlendEventReserveEmissions
	EventGulpEmissions    = domain.BlendEventGulpEmissions

	// Admin / status events.
	EventSetAdmin         = domain.BlendEventSetAdmin
	EventUpdatePool       = domain.BlendEventUpdatePool
	EventQueueSetReserve  = domain.BlendEventQueueSetReserve
	EventCancelSetReserve = domain.BlendEventCancelSetReserve
	EventSetReserve       = domain.BlendEventSetReserve
	EventSetStatus        = domain.BlendEventSetStatus

	// Pool-factory event — observed at the factory contract, used
	// for runtime pool enumeration.
	EventDeploy = domain.BlendEventDeploy

	// V1 pool-factory (CCZD6ESM…) events. The evidence trail is in the
	// domain.BlendEventUpdateEmissions / domain.BlendEventNewLiquidationAuction
	// docs and this package's README "Known gap".
	EventUpdateEmissions          = domain.BlendEventUpdateEmissions
	EventNewLiquidationAuction    = domain.BlendEventNewLiquidationAuction
	EventDeleteLiquidationAuction = domain.BlendEventDeleteLiquidationAuction
)

// Mainnet V2 contract addresses (see docs/protocols/blend.md).
const (
	// MainnetPoolFactory is the documented Pool Factory V2. Blend was
	// REDEPLOYED, so this is not the only factory — see MainnetPoolFactories.
	MainnetPoolFactory = "CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU"
	// MainnetPoolFactoryV1 is the earlier pool factory; it deployed pools a V2-only gate would
	// silently drop. Its first deploy is at ledger 51_499_915.
	MainnetPoolFactoryV1 = "CCZD6ESMOGMPWH2KRO4O7RGTAPGTUPFWFQBELQSS7ZUK63V3TZWETGAG"
	// MainnetBackstop is the Backstop V2 singleton, decoded by internal/sources/blend_backstop. Do
	// NOT add backstops to the pool gate registry: pool decode paths would mis-decode their bodies.
	MainnetBackstop = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	// MainnetBackstopV1 is the V1-era backstop, identified from the lake by its backstop event
	// profile (queue_withdrawal/gulp_emissions/...).
	MainnetBackstopV1 = "CAO3AGAMZVRMHITL36EJ2VZQWKYRPWMQAPDQD5YEOF3GIF7T44U4JAL3"
)

// MainnetPoolFactories is the complete set of Blend pool factories on mainnet (ADR-0035); gating `deploy` on one factory silently drops the other's pools, so re-derive from the lake if a third appears.
var MainnetPoolFactories = []string{
	MainnetPoolFactoryV1,
	MainnetPoolFactory,
}

// FactoryGenesisLedger is a lower bound on any Blend factory `deploy`; the pool-registry genesis seed, the ADR-0033 reconcile genesis and documented replays all start here.
const FactoryGenesisLedger uint32 = 51_499_546

// Pre-encoded base64 SCVal::Symbol blobs for byte-equality topic classification, which avoids
// re-decoding the topic on every event.
var (
	// Auction events.
	TopicSymbolNewAuction    = scval.MustEncodeSymbol(EventNewAuction)
	TopicSymbolFillAuction   = scval.MustEncodeSymbol(EventFillAuction)
	TopicSymbolDeleteAuction = scval.MustEncodeSymbol(EventDeleteAuction)

	// Money-market events.
	TopicSymbolSupply             = scval.MustEncodeSymbol(EventSupply)
	TopicSymbolWithdraw           = scval.MustEncodeSymbol(EventWithdraw)
	TopicSymbolSupplyCollateral   = scval.MustEncodeSymbol(EventSupplyCollateral)
	TopicSymbolWithdrawCollateral = scval.MustEncodeSymbol(EventWithdrawCollateral)
	TopicSymbolBorrow             = scval.MustEncodeSymbol(EventBorrow)
	TopicSymbolRepay              = scval.MustEncodeSymbol(EventRepay)
	TopicSymbolFlashLoan          = scval.MustEncodeSymbol(EventFlashLoan)
	TopicSymbolGulp               = scval.MustEncodeSymbol(EventGulp)
	TopicSymbolClaim              = scval.MustEncodeSymbol(EventClaim)

	// Credit-risk + emissions.
	TopicSymbolBadDebt          = scval.MustEncodeSymbol(EventBadDebt)
	TopicSymbolDefaultedDebt    = scval.MustEncodeSymbol(EventDefaultedDebt)
	TopicSymbolReserveEmissions = scval.MustEncodeSymbol(EventReserveEmissions)
	TopicSymbolGulpEmissions    = scval.MustEncodeSymbol(EventGulpEmissions)

	// Admin / status.
	TopicSymbolSetAdmin         = scval.MustEncodeSymbol(EventSetAdmin)
	TopicSymbolUpdatePool       = scval.MustEncodeSymbol(EventUpdatePool)
	TopicSymbolQueueSetReserve  = scval.MustEncodeSymbol(EventQueueSetReserve)
	TopicSymbolCancelSetReserve = scval.MustEncodeSymbol(EventCancelSetReserve)
	TopicSymbolSetReserve       = scval.MustEncodeSymbol(EventSetReserve)
	TopicSymbolSetStatus        = scval.MustEncodeSymbol(EventSetStatus)

	// Pool factory.
	TopicSymbolDeploy = scval.MustEncodeSymbol(EventDeploy)

	// V1 pool-factory events.
	TopicSymbolUpdateEmissions          = scval.MustEncodeSymbol(EventUpdateEmissions)
	TopicSymbolNewLiquidationAuction    = scval.MustEncodeSymbol(EventNewLiquidationAuction)
	TopicSymbolDeleteLiquidationAuction = scval.MustEncodeSymbol(EventDeleteLiquidationAuction)
)

// AuctionType discriminator — verified against
// pool/src/auctions/auction.rs constants. The contract emits
// `auction_type: u32` as topic[1] on every auction event.
const (
	AuctionTypeUserLiquidation uint32 = 0
	AuctionTypeBadDebt         uint32 = 1
	AuctionTypeInterest        uint32 = 2
)

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrNotBlendEvent: topic[0] is not a tracked name; the dispatcher skips rather than retries.
	ErrNotBlendEvent = errors.New("blend: not a tracked Blend event")

	// ErrMalformedPayload: topic arity / body shape / type tags differ from what the contract
	// emits. Fails loudly per event so decoder-vs-WASM drift surfaces.
	ErrMalformedPayload = errors.New("blend: malformed event payload")

	// ErrUnknownAuctionType: auction_type is outside {0,1,2}, i.e. a contract upgrade added an
	// auction kind we have not audited.
	ErrUnknownAuctionType = errors.New("blend: unknown auction type")
)
