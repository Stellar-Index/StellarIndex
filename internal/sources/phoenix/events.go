// Package phoenix ingests trade events from the Phoenix Soroban DEX.
//
// Design reference: README.md here and docs/protocols/phoenix.md. Read the README's Q1-Q5 quirks
// before modifying the decoder, especially the 8-events-per-swap correlation (Q1).
package phoenix

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName — stable identifier.
const SourceName = "phoenix"

// Phoenix emits a constant-product swap as 8 distinct events, each carrying a single field value.
// These constants name the fields exactly as the contract does. The spelling MATTERS: "actual
// received amount" has embedded spaces (Q2), so it cannot be an ScvSymbol and every swap topic
// slot is an ScvString.
const (
	FieldSender         = "sender"
	FieldSellToken      = "sell_token"
	FieldOfferAmount    = "offer_amount"
	FieldActualReceived = "actual received amount" // note the spaces (Q2)
	FieldBuyToken       = "buy_token"
	FieldReturnAmount   = "return_amount"
	FieldSpreadAmount   = "spread_amount"
	FieldReferralFee    = "referral_fee_amount"
)

// SwapFieldCount is the number of distinct events per swap (Q1); a trade is ready only when all
// 8 RawSwap slots are populated.
const SwapFieldCount = 8

// EventActionSwap is topic[0] of every swap-field event; topic[1] carries the field name.
const EventActionSwap = "swap"

// ─── Liquidity actions ──────────────────────────────────────────
//
// Pool contracts emit the same N-event-per-action shape as `swap`:
//
//	provide_liquidity (5 events): sender, token_a, token_a-amount, token_b, token_b-amount
//	withdraw_liquidity (4 events): sender, shares_amount, return_amount_a, return_amount_b
//
// withdraw may also emit a 5th ("withdraw_liquidity", "auto unbonded") event with a tuple body;
// it is classified but not required for the correlation to complete. The stake contract emits 3
// events per bond / unbond: user, token, amount. Field strings are literal contract source
// (including the `-amount` hyphens) and all topics are ScVal::String, not Symbol.

const (
	EventActionProvideLiquidity  = "provide_liquidity"
	EventActionWithdrawLiquidity = "withdraw_liquidity"
	EventActionBond              = "bond"
	EventActionUnbond            = "unbond"
	// EventActionAdmin is topic[0] of every XYK pool admin-rotation event (topic[1] phrases:
	// "Admin replacement requested by old admin: ", "Replace with new admin: ",
	// "Undo admin change: ", "Accepted new admin: "). The trailing space is faithful to the
	// contract source. Classification only; no canonical Trade is produced.
	EventActionAdmin = "XYK Pool: "
	// EventActionInitialize is topic[0] of pool-init events ("XYK LP token_a" / "XYK LP token_b"),
	// emitted once per pool deploy. Classification only.
	EventActionInitialize = "initialize"

	// EventActionCreate / FieldCreateLiquidityPool are the FACTORY's pool announcement,
	// ("create","liquidity_pool") with a body of one Address, the pool just deployed. Emitted by the
	// factory, not a pool, it is the decoder's live admission seam (ADR-0040 mechanism 1) and the
	// factory's ONLY ("create", ...) publish, so the pair is exhaustive for the action.
	EventActionCreate        = "create"
	FieldCreateLiquidityPool = "liquidity_pool"

	// AdminAction* are the stored slugs for the four admin-rotation topic[1] phrases.
	AdminActionReplaceRequested = "replace_requested"
	AdminActionReplaceSet       = "replace_set"
	AdminActionUndo             = "undo"
	AdminActionAccepted         = "accepted"
)

// Field names for `provide_liquidity` (5 events per call); the hyphens come from the contract source.
const (
	FieldPLSender              = "sender"
	FieldPLTokenA              = "token_a"
	FieldPLTokenAAmt           = "token_a-amount"
	FieldPLTokenB              = "token_b"
	FieldPLTokenBAmt           = "token_b-amount"
	ProvideLiquidityFieldCount = 5
)

// Field names for `withdraw_liquidity` (4 events per call, plus the optional [FieldWLAutoUnbonded]).
const (
	FieldWLSender               = "sender"
	FieldWLSharesAmount         = "shares_amount"
	FieldWLReturnAmountA        = "return_amount_a"
	FieldWLReturnAmountB        = "return_amount_b"
	FieldWLAutoUnbonded         = "auto unbonded" // optional — emitted only when withdrawing also unbonds
	WithdrawLiquidityFieldCount = 4
)

// Field names for `bond` / `unbond` (3 events per call, same shape for both).
const (
	FieldStakeUser   = "user"
	FieldStakeToken  = "token"
	FieldStakeAmount = "amount"
	StakeFieldCount  = 3
)

// ─── Reward actions ──────────────────────────────────────────────
//
// The gated stake contracts emit two reward actions beyond bond/unbond:
//
//	withdraw_rewards   (2 events): user, reward_token
//	distribute_rewards (1 event):  asset
//
// Neither event carries an amount; it surfaces on the reward token's SEP-41 `transfer` in the SAME
// op (event_index+1), a cross-decoder join not done here, so the amount is stored NULL rather
// than a misleading "0". distribute_rewards has no user field, so it is decoded from its single
// event, not the buffer.
const (
	EventActionWithdrawRewards   = "withdraw_rewards"
	EventActionDistributeRewards = "distribute_rewards"

	FieldWRUser               = "user"
	FieldWRRewardToken        = "reward_token"
	WithdrawRewardsFieldCount = 2

	FieldDRAsset = "asset"
)

// ─── Stake-contract lifecycle, factory and blend-pool admin actions ──
//
// Every shape below was measured on a gated emitter (test/fixtures/phoenix/event-shapes): a
// two-String-topic event with a single-value body, decoded without a correlation buffer.
//
//	("create_distribution_flow", "asset")                  stake, body Address(asset)
//	("Stake: Migration: ", "Start of migration for user: ") stake, body Address(user)
//	("Stake: Migration: ", "Query for user completed: ")    stake, body Address(user)
//	("Stake", "Migration for user completed and stored: ")  stake, body Address(user)
//	("Factory", "Updated Config")                           factory, body Void
//	("blend_pool", "set_delegate")                          pool, body Address
//	("blend_pool", "set_min_trading_a" | "_b")              pool, body i128
//	("toggle_trading", "enabled")                           pool, body Bool
//
// toggle_trading is recognised but projects no row: the phoenix_admin_events CHECK has no slug.
const (
	EventActionCreateDistributionFlow = "create_distribution_flow"
	EventActionStakeMigration         = "Stake: Migration: "
	EventActionStake                  = "Stake"
	EventActionFactory                = "Factory"
	EventActionBlendPool              = "blend_pool"
	EventActionToggleTrading          = "toggle_trading"

	// StakeAction* are the phoenix_stake_events.action slugs for stake-contract lifecycle events.
	StakeActionMigrationStarted   = "migration_started"
	StakeActionMigrationQueried   = "migration_queried"
	StakeActionMigrationCompleted = "migration_completed"

	// AdminAction* for the factory and blend-pool configuration events.
	AdminActionFactoryConfigUpdated = "factory_config_updated"
	AdminActionBlendSetDelegate     = "blend_set_delegate"
	AdminActionBlendSetMinTradingA  = "blend_set_min_trading_a"
	AdminActionBlendSetMinTradingB  = "blend_set_min_trading_b"
)

// Mainnet contract addresses — Phase-1 verified against
// Phoenix-Protocol-Group/phoenix-contracts `scripts/*.sh`.
const (
	MainnetFactory  = "CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI"
	MainnetMultihop = "CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G"

	// Deliberately no XLM SAC constant here. "CDLZFC3SY…" is NOT the XLM SAC on any network; it is
	// a synthetic fixture id. There is exactly one native-XLM SAC per network (see
	// canonical.Asset.SacContractID()); use aquarius.MainnetXLMSAC or canonical.XLMSacContractID.
)

// MainnetPools is the curated gated pool set, from the factory's `query_pools()` cross-checked
// against lake activity. The decoder also self-registers from the factory's create events, so
// this is a cold-start root for pools created outside the streamed window. A pool missing from
// BOTH fail-closes and surfaces as a recognition gap, never silently mis-attributed.
var MainnetPools = []string{
	"CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX",
	"CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH",
	"CD5XNKK3B6BEF2N7ULNHHGAMOKZ7P6456BFNIHRF4WNTEDKBRWAE7IAA",
	"CBISULYO5ZGS32WTNCBMEFCNKNSLFXCQ4Z3XHVDP4X4FLPSEALGSY3PS",
	"CDMXKSLG5GITGFYERUW2MRYOBUQCMRT2QE5Y4PU3QZ53EBFWUXAXUTBC",
	"CB5QUVK5GS3IU23TMFZQ3P5J24YBBZP5PHUQAEJ2SP5K55PFTJRUQG2L",
	"CC6MJZN3HFOJKXN42ANTSCLRFOMHLFXHWPNAX64DQNUEBDMUYMPHASAV",
	"CBW5G5SO5SDYUGQVU7RMZ2KJ34POM3AMODOBIV2RQYG4KJDUUBVC3P2T",
	"CCKOC2LJTPDBKDHTL3M5UO7HFZ2WFIHSOKCELMKQP3TLCIVUBKOQL4HB",
	"CCUCE5H5CKW3S7JBESGCES6ZGDMWLNRY3HOFET3OH33MXZWKXNJTKSM3",
	"CDQLKNH3725BUP4HPKQKMM7OO62FDVXVTO7RCYPID527MZHJG2F3QBJW",
	// An XYK String-schema pool the query_pools() snapshot missed; genuine (deployed in the
	// factory's pool-create transaction with its stake contract). Without it its served
	// phoenix_liquidity rows would score expected=0 under the gated re-derive.
	"CAZ6W4WHVGQBGURYTUOLCUOOHW6VQGAAPSPCD72VEDZMBBPY7H43AYEC",
}

// MainnetStakeContracts are the per-pool stake contracts that emit bond/unbond. They are separate
// addresses NOT returned by query_pools(), enumerated from lake activity. An unlisted one
// fail-closes into a recognition gap and gets added here.
var MainnetStakeContracts = []string{
	"CBRGNWGAC25CPLMOAMR7WBPOF5QTFA5RYXQH4DEJ4K65G2QFLTLMW7RO",
	"CAF3UJ45ZQJP6USFUIMVMGOUETUTXEC35R2247VJYIVQBGKTKBZKNBJ3",
	// CBBUVHCE… is deliberately absent: a bond-instrument contract whose WASM has none of the stake
	// literals; it only shares the "bond" topic word. The 13 below are genuine per-pool stake
	// contracts: the first 11 co-occur in their pool's factory create transaction, and
	// CDOXQONPND... and CDEQYRWFU... are driven by the phoenix reward keeper CBZ7M5B3Y4WW....
	"CABWEFVXUB3XWYPTWFETEGJR2WRGE2ZKYYLZDLV3EBUVFMOU4ENK4DJC", // ↔ pool CBHCRSVX (factory create @51,572,026)
	"CAIR3UPW2PEP27QZWX4XGMO65W6LJ3XCRA3F5G7Z3D52MNOVF5K5YZ56", // ↔ pool CBCZGGNO (factory create @51,572,030)
	"CDP6DT2YU75ZMOPTTCQ563H2XZDDWHPWKRQ6N2W5LNVE5HHRSB4MMRNQ", // ↔ pool CAZ6W4WH (factory create @51,572,101)
	"CB2S5X4H6ZMMCDQV4DNKEO2SBSW7T2YXVN5A7G2BBSN3VM73CQYIIZ3C", // ↔ pool CBISULYO (factory create @51,927,948)
	"CCP653KENMYCAYQ3PHJDT6PITMG4XYKVWV3OEDDCOAOS6Z4GOMXGYH3Z", // ↔ pool CDQLKNH3 (factory create @53,853,219)
	"CCIWIW6ESCCCFMEI5QOSUHDKTMBEMRJ22F7GPYNRKM2UI2FH6WYUKOUU", // ↔ pool CBW5G5SO (factory create @53,853,220)
	"CBULEXIMZ5C4CSUPZ4E5LXATWDZNS6MDM2A57DAUD5GXSUG4IWKLOSOC", // ↔ pool CDMXKSLG (factory create @53,955,603)
	"CD2YKNPX3JPTGDANJRPEJS42MPQLEVUVVRZKJYLLUSPJKQJA7LUANBO4", // ↔ pool CC6MJZN3 (factory create @54,953,243)
	"CDBMVFP7KJXW3YEFSLOU5GYUQHHJJI7QPZJPCSPDK6HHBCBZAMCHS2QY", // ↔ pool CB5QUVK5 (factory create @54,953,245)
	"CDH6JILIADIC5SKE6OZJAYV3GM62RTR4O54OMVNP4ZOK4HH4J2JWJPVW", // ↔ pool CCKOC2LJ (factory create @54,953,247)
	"CBDCTYZSZIOWCK5IGCQZNFUOJ53KMPYG2MG7GMVGE3A2LEYCFTDYYZ3S", // ↔ pool CCUCE5H5 (factory create @54,953,248)
	"CDOXQONPND365K6MHR3QBSVVTC3MKR44ORK6TI2GQXUXGGAS5SNDAYRI", // pre-lake; 260 shared tx w/ curated pools + keeper CBZ7M5B3
	"CDEQYRWFU3IHPRR6H6VOQRUU3JFS6DTUYUL4YAQSD3ALB5IPBTEOZUFM", // pre-lake; keeper CBZ7M5B3 + phoenix v1.1 stake-migration events
}

// MainnetMapPools are pools running the NEWER pool WASM whose swap emits a SINGLE ScvSymbol("swap")
// event with an ScvMap body instead of the older 8 ScvString-tuple events (Q5). Gating is by
// contract identity, so a pool upgrading from String to Map shape in place is already covered;
// only the decode dispatch depends on the topic shape.
var MainnetMapPools = []string{
	"CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S",
}

// MainnetGatedSet is the full curated child set the decoder seeds: String-schema pools, Map-schema
// pools and stake contracts. The multihop relay is deliberately EXCLUDED; it only relays to pools
// and emits no swap/liquidity/stake events, so gating loses nothing.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetPools)+len(MainnetMapPools)+len(MainnetStakeContracts))
	out = append(out, MainnetPools...)
	out = append(out, MainnetMapPools...)
	out = append(out, MainnetStakeContracts...)
	return out
}

// Pre-encoded base64 SCVal::String blobs for topic[0] and topic[1], computed at init. Phoenix
// emits both topic positions as Strings (not Symbols) because the pool contract publishes
// `(str_literal, str_literal)` tuples.
var (
	TopicSymbolSwap = scval.MustEncodeString(EventActionSwap) // topic[0]

	TopicSymbolSender         = scval.MustEncodeString(FieldSender)         // topic[1] variants
	TopicSymbolSellToken      = scval.MustEncodeString(FieldSellToken)      //
	TopicSymbolOfferAmount    = scval.MustEncodeString(FieldOfferAmount)    //
	TopicSymbolActualReceived = scval.MustEncodeString(FieldActualReceived) //
	TopicSymbolBuyToken       = scval.MustEncodeString(FieldBuyToken)       //
	TopicSymbolReturnAmount   = scval.MustEncodeString(FieldReturnAmount)   //
	TopicSymbolSpreadAmount   = scval.MustEncodeString(FieldSpreadAmount)   //
	TopicSymbolReferralFee    = scval.MustEncodeString(FieldReferralFee)    //
)

// TopicSymbolSwapMap is topic[0] of the NEWER Map-body swap schema: a SINGLE ScvSymbol("swap")
// (disc 0x0F) whose body is an ScvMap, distinct from the older ScvString("swap") (disc 0x0E). Map
// keys are underscore Symbols ("actual_received_amount"). See README Q5: pools upgrade in place
// and can change event SHAPE, not just fields.
var TopicSymbolSwapMap = scval.MustEncodeSymbol(EventActionSwap)

// The Map-schema pool WASM publishes provide/withdraw_liquidity the same way: one ScvSymbol topic,
// ScvMap body.
var (
	TopicSymbolProvideLiquidityMap  = scval.MustEncodeSymbol(EventActionProvideLiquidity)
	TopicSymbolWithdrawLiquidityMap = scval.MustEncodeSymbol(EventActionWithdrawLiquidity)
)

// Topic encodings for the stake-lifecycle, factory and blend-pool shapes
// listed beside EventActionCreateDistributionFlow.
var (
	TopicCreateDistributionFlow = scval.MustEncodeString(EventActionCreateDistributionFlow)
	TopicStakeMigration         = scval.MustEncodeString(EventActionStakeMigration)
	TopicStake                  = scval.MustEncodeString(EventActionStake)
	TopicFactory                = scval.MustEncodeString(EventActionFactory)
	TopicBlendPool              = scval.MustEncodeString(EventActionBlendPool)
	TopicToggleTrading          = scval.MustEncodeString(EventActionToggleTrading)

	TopicMigrationStarted     = scval.MustEncodeString("Start of migration for user: ")
	TopicMigrationQueried     = scval.MustEncodeString("Query for user completed: ")
	TopicMigrationCompleted   = scval.MustEncodeString("Migration for user completed and stored: ")
	TopicFactoryUpdatedConfig = scval.MustEncodeString("Updated Config")
	TopicBlendSetDelegate     = scval.MustEncodeString("set_delegate")
	TopicBlendSetMinTradingA  = scval.MustEncodeString("set_min_trading_a")
	TopicBlendSetMinTradingB  = scval.MustEncodeString("set_min_trading_b")
	TopicToggleTradingEnabled = scval.MustEncodeString("enabled")
)

// Liquidity-management topic[0] encodings + topic[1] field names. As with swap above, contracts
// publish tuple-literals such as `.publish(("provide_liquidity", "sender"), ...)`, so both slots
// serialise as ScVal::String.
var (
	TopicSymbolProvideLiquidity  = scval.MustEncodeString(EventActionProvideLiquidity)  // topic[0]
	TopicSymbolWithdrawLiquidity = scval.MustEncodeString(EventActionWithdrawLiquidity) // topic[0]
	TopicSymbolBond              = scval.MustEncodeString(EventActionBond)              // topic[0]
	TopicSymbolUnbond            = scval.MustEncodeString(EventActionUnbond)            // topic[0]
	TopicSymbolAdmin             = scval.MustEncodeString(EventActionAdmin)             // topic[0] for the 4 admin variants
	TopicSymbolInitialize        = scval.MustEncodeString(EventActionInitialize)        // topic[0] for the 2 init variants

	// Factory pool announcement, ("create","liquidity_pool"), both ScvString. Being Strings, the
	// lake's topic_0_sym column is EMPTY for these rows, so a walk keyed on it matches nothing;
	// the re-derive prefilter matches topics_xdr instead (internal/storage/clickhouse/event_reader.go).
	TopicSymbolCreate        = scval.MustEncodeString(EventActionCreate)        // topic[0]
	TopicCreateLiquidityPool = scval.MustEncodeString(FieldCreateLiquidityPool) // topic[1]

	// initialize topic[1] variants: the pool announces its two tokens.
	TopicInitTokenA = scval.MustEncodeString("XYK LP token_a") // topic[1]
	TopicInitTokenB = scval.MustEncodeString("XYK LP token_b")

	// TopicInitLPShareStaking is the per-pool STAKE contract's initialize topic[1] (body = one
	// Address). It maps to NO phoenix_initialize row, which models the pool's token slots, so it
	// is recognised but not projected. Stake contracts are gated and pass Matches(); recognising
	// the topic keeps them from counting as undecodable-but-matched blind spots in the
	// re-derive (reconcile.go).
	TopicInitLPShareStaking = scval.MustEncodeString("LP Share token staking contract")

	// admin (governance rotation) topic[1] phrases; the trailing space is faithful to the contract.
	TopicAdminReplaceRequested = scval.MustEncodeString("Admin replacement requested by old admin: ") // topic[1]
	TopicAdminReplaceSet       = scval.MustEncodeString("Replace with new admin: ")
	TopicAdminUndo             = scval.MustEncodeString("Undo admin change: ")
	TopicAdminAccepted         = scval.MustEncodeString("Accepted new admin: ")

	// adminActionByTopic maps each admin topic[1] blob to its stored AdminAction* slug.
	adminActionByTopic = map[string]string{
		TopicAdminReplaceRequested: AdminActionReplaceRequested,
		TopicAdminReplaceSet:       AdminActionReplaceSet,
		TopicAdminUndo:             AdminActionUndo,
		TopicAdminAccepted:         AdminActionAccepted,
	}

	// provide_liquidity topic[1] variants.
	TopicSymbolPLSender    = scval.MustEncodeString(FieldPLSender)
	TopicSymbolPLTokenA    = scval.MustEncodeString(FieldPLTokenA)
	TopicSymbolPLTokenAAmt = scval.MustEncodeString(FieldPLTokenAAmt)
	TopicSymbolPLTokenB    = scval.MustEncodeString(FieldPLTokenB)
	TopicSymbolPLTokenBAmt = scval.MustEncodeString(FieldPLTokenBAmt)

	// withdraw_liquidity topic[1] variants (4 required + 1 optional).
	TopicSymbolWLSender        = scval.MustEncodeString(FieldWLSender)
	TopicSymbolWLSharesAmount  = scval.MustEncodeString(FieldWLSharesAmount)
	TopicSymbolWLReturnAmountA = scval.MustEncodeString(FieldWLReturnAmountA)
	TopicSymbolWLReturnAmountB = scval.MustEncodeString(FieldWLReturnAmountB)
	TopicSymbolWLAutoUnbonded  = scval.MustEncodeString(FieldWLAutoUnbonded)

	// bond / unbond topic[1] variants (shared field set).
	TopicSymbolStakeUser   = scval.MustEncodeString(FieldStakeUser)
	TopicSymbolStakeToken  = scval.MustEncodeString(FieldStakeToken)
	TopicSymbolStakeAmount = scval.MustEncodeString(FieldStakeAmount)

	// withdraw_rewards / distribute_rewards topic[0] + topic[1] variants.
	TopicSymbolWithdrawRewards   = scval.MustEncodeString(EventActionWithdrawRewards)   // topic[0]
	TopicSymbolDistributeRewards = scval.MustEncodeString(EventActionDistributeRewards) // topic[0]
	TopicSymbolWRUser            = scval.MustEncodeString(FieldWRUser)
	TopicSymbolWRRewardToken     = scval.MustEncodeString(FieldWRRewardToken)
	TopicSymbolDRAsset           = scval.MustEncodeString(FieldDRAsset)
)

// Errors returned by the decode path.
var (
	// ErrUnknownField — topic[1] matched none of the 8 swap field names; usually a non-swap event,
	// skipped.
	ErrUnknownField = errors.New("phoenix: unknown swap field")

	// ErrIncompleteSwap — fewer than 8 fields populated when finalising. Defence-in-depth: the
	// buffer only returns complete RawSwaps.
	ErrIncompleteSwap = errors.New("phoenix: incomplete swap (need 8 fields)")

	// ErrMalformedPayload — field values have the wrong types or a negative amount.
	ErrMalformedPayload = errors.New("phoenix: malformed swap payload")

	// ErrZeroAmountSwap marks a fully-decoded swap with a ZERO leg: a genuine dust swap, not a
	// malformed payload. The adapter treats it (like canonical.ErrPairMismatch) as a recognised
	// no-op so the ADR-0033 re-derive counts expected=0 instead of failing the verdict closed.
	ErrZeroAmountSwap = errors.New("phoenix: zero-amount swap (recognised no-op)")

	// ErrIncompleteLiquidity — a liquidity decode ran before every required field landed.
	// Defence-in-depth: the buffer only returns completed records.
	ErrIncompleteLiquidity = errors.New("phoenix: incomplete liquidity event")

	// ErrIncompleteStake is ErrIncompleteLiquidity for the bond / unbond 3-event reassembly.
	ErrIncompleteStake = errors.New("phoenix: incomplete stake event")
)
