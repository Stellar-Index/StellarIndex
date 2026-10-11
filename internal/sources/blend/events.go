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

// SourceName constant — appears in metrics labels, registry keys,
// and storage rows. Stable.
const SourceName = "blend"

// Event names — topic[0] of every Blend pool / pool-factory event,
// as a Soroban Symbol on the wire (blend-contracts-v2 c19abee5b9be4f49e0cda9057e87d343e5dcc095).
//
// All kinds except the three auction ones are also the persisted `event_kind` value in
// blend_positions / blend_emissions / blend_admin, so they are defined in [domain.BlendEventSupply]
// and siblings (storage validates against them and cannot import this package); the names below
// alias those. The auction consts have no storage-side validation switch, so they live only here.
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

// Mainnet V2 contract addresses — verified via stellar.expert and
// cross-referenced against docs/protocols/blend.md and the
// Blend Capital blend-contracts-v2 deploy manifest.
const (
	// MainnetPoolFactory is the documented Pool Factory V2. Blend was
	// REDEPLOYED, so this is not the only factory — see MainnetPoolFactories.
	MainnetPoolFactory = "CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU"
	// MainnetPoolFactoryV1 is the earlier pool factory. It emits `deploy` events with an
	// ScVal::Address body and deployed 4 of the 9 known auction-emitting pools, which a V2-only
	// gate would silently drop. Its first deploy is at ledger 51_499_915.
	MainnetPoolFactoryV1 = "CCZD6ESMOGMPWH2KRO4O7RGTAPGTUPFWFQBELQSS7ZUK63V3TZWETGAG"
	// MainnetBackstop is the Backstop V2 singleton (V1 is below; it was redeployed). Backstop
	// events are a different surface decoded by internal/sources/blend_backstop. Do NOT add
	// backstops to the pool gate registry: pool decode paths would mis-decode their bodies.
	MainnetBackstop = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	// MainnetBackstopV1 is the V1-era backstop, identified from the lake by its backstop event
	// profile (queue_withdrawal/gulp_emissions/...).
	MainnetBackstopV1 = "CAO3AGAMZVRMHITL36EJ2VZQWKYRPWMQAPDQD5YEOF3GIF7T44U4JAL3"
)

// MainnetPoolFactories is the COMPLETE, empirically-verified set of Blend
// pool factories on mainnet (ADR-0035). Blend was redeployed, so gating `deploy` on a single
// factory silently drops the other's pools. Derived by decoding every lake `deploy` event whose
// body is an ScVal::Address and confirming the children cover all 9 known pools; re-run if a third
// factory appears. Order is irrelevant (set membership).
var MainnetPoolFactories = []string{
	MainnetPoolFactoryV1,
	MainnetPoolFactory,
}

// FactoryGenesisLedger is a lower bound on any Blend pool factory `deploy`:
// the ledger of Blend's first mainnet contract (the backstop Comet pool), 369 ledgers before the V1
// factory's first deploy. The pool-registry genesis seed (ADR-0035) walks factory deploys from
// here, the ADR-0033 reconcile uses it as the blend source genesis, and documented replays start
// here (doc_parity_test).
const FactoryGenesisLedger uint32 = 51_499_546

// Pre-encoded base64 SCVal::Symbol blobs, computed at init via
// scval.MustEncodeSymbol. Used for byte-equality classification
// against incoming event topics (cheaper than re-decoding the
// topic on every event).
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
	// ErrNotBlendEvent — topic[0] doesn't match any of the names we
	// track. Returned by classify(); the dispatcher uses this to
	// skip cheaply rather than retry.
	ErrNotBlendEvent = errors.New("blend: not a tracked Blend event")

	// ErrMalformedPayload — topic arity / body shape / type tags
	// don't match what blend-contracts-v2 emits. Per-event-fail-loud
	// rather than silent skip; surfaces decoder vs WASM drift.
	ErrMalformedPayload = errors.New("blend: malformed event payload")

	// ErrUnknownAuctionType — auction_type in topic[1] is outside
	// {0=UserLiquidation, 1=BadDebt, 2=Interest}. Indicates a
	// contract upgrade introducing a new auction kind we haven't
	// audited.
	ErrUnknownAuctionType = errors.New("blend: unknown auction type")
)
