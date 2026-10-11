// Package phoenix ingests trade events from the Phoenix Soroban DEX.
//
// Design reference: internal/sources/phoenix/README.md and
// docs/protocols/phoenix.md. Read the README's Q1–Q5 quirks before
// modifying the decoder, especially the 8-events-per-swap
// correlation (Q1).
package phoenix

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName — stable identifier.
const SourceName = "phoenix"

// Phoenix emits a constant-product swap as 8 distinct events, each
// carrying a single field value. These constants name the fields
// exactly as they appear in contracts/pool/src/contract.rs:1172-1185.
// The string spelling MATTERS — "actual received amount" has
// embedded spaces (Q2), which means it CAN'T be encoded as an
// ScvSymbol (identifier-only) — soroban-sdk emits it as ScvString
// instead. Verified against mainnet: every Phoenix swap
// topic slot is ScvString, not ScvSymbol.
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

// SwapFieldCount is the number of distinct events per swap (Q1).
// A trade is ready to emit only when all 8 slots of the RawSwap
// are populated.
const SwapFieldCount = 8

// EventActionSwap — the value of topic[0] for every swap-field
// event. topic[1] carries the per-field name.
const EventActionSwap = "swap"

// ─── Liquidity actions ──────────────────────────────────────────
//
// Pool contracts (volatile and stableswap) emit the same N-event-per-action
// shape as `swap`:
//
//	provide_liquidity (5 events): sender, token_a, token_a-amount,
//	                              token_b, token_b-amount
//	withdraw_liquidity (4 events): sender, shares_amount,
//	                               return_amount_a, return_amount_b
//
// withdraw may also emit a 5th ("withdraw_liquidity", "auto unbonded") event with
// a tuple body (stake_amount, stake_timestamp); it is classified but not
// required for the correlation to complete.
//
// The stake contract emits 3 events per bond / unbond: user, token, amount.
//
// Field strings are the literal contract source; keep spellings identical,
// including the `-amount` hyphens. All topics are ScVal::String, not Symbol
// (soroban-sdk serialises tuple-literal strings that way).

const (
	EventActionProvideLiquidity  = "provide_liquidity"
	EventActionWithdrawLiquidity = "withdraw_liquidity"
	EventActionBond              = "bond"
	EventActionUnbond            = "unbond"
	// EventActionAdmin is the topic[0] of every governance/admin
	// rotation event emitted by the XYK pool contract:
	//   ("XYK Pool: ", "Admin replacement requested by old admin: ")
	//   ("XYK Pool: ", "Replace with new admin: ")
	//   ("XYK Pool: ", "Undo admin change: ")
	//   ("XYK Pool: ", "Accepted new admin: ")
	// The literal includes a trailing space; that's faithful to the
	// contract source (pool/src/contract.rs:784-836). We don't
	// produce a canonical Trade for these — classification only.
	EventActionAdmin = "XYK Pool: "
	// EventActionInitialize is the topic[0] of pool-init events:
	//   ("initialize", "XYK LP token_a")
	//   ("initialize", "XYK LP token_b")
	// Emitted once per pool deploy. Same classification-only intent.
	EventActionInitialize = "initialize"

	// EventActionCreate / FieldCreateLiquidityPool are the FACTORY's
	// pool announcement: ("create","liquidity_pool") with a body of one
	// Address — the pool the factory just deployed. Unlike every other
	// action here, this one is emitted by the factory, not by a pool, and
	// it is the decoder's live admission seam (ADR-0040 §1 mechanism 1).
	// Upstream (contracts/factory/src/contract.rs, identical at v1.0.0,
	// v1.1.0, v2.0.0 and main) it is the ONLY ("create", …) publish in
	// the factory, so the pair below is exhaustive for the action.
	EventActionCreate        = "create"
	FieldCreateLiquidityPool = "liquidity_pool"

	// AdminAction* are the stored slugs for the four admin-rotation
	// topic[1] phrases (phoenix_admin_events.admin_action, migration 0132).
	AdminActionReplaceRequested = "replace_requested"
	AdminActionReplaceSet       = "replace_set"
	AdminActionUndo             = "undo"
	AdminActionAccepted         = "accepted"
)

// Field names for `provide_liquidity` (5 events per call).
// The `token_a-amount` / `token_b-amount` hyphens come from the
// contract source — see contracts/pool/src/contract.rs:346-355.
const (
	FieldPLSender              = "sender"
	FieldPLTokenA              = "token_a"
	FieldPLTokenAAmt           = "token_a-amount"
	FieldPLTokenB              = "token_b"
	FieldPLTokenBAmt           = "token_b-amount"
	ProvideLiquidityFieldCount = 5
)

// Field names for `withdraw_liquidity` (4 events per call, plus the
// optional `auto unbonded` 5th — see [FieldWLAutoUnbonded]).
const (
	FieldWLSender               = "sender"
	FieldWLSharesAmount         = "shares_amount"
	FieldWLReturnAmountA        = "return_amount_a"
	FieldWLReturnAmountB        = "return_amount_b"
	FieldWLAutoUnbonded         = "auto unbonded" // optional — emitted only when withdrawing also unbonds
	WithdrawLiquidityFieldCount = 4
)

// Field names for `bond` / `unbond` (3 events per call, same shape
// for both actions — see contracts/stake/src/contract.rs:165-167
// and 196-198).
const (
	FieldStakeUser   = "user"
	FieldStakeToken  = "token"
	FieldStakeAmount = "amount"
	StakeFieldCount  = 3
)

// ─── Reward actions ──────────────────────────────────────────────
//
// A lake topic census of the gated stake contracts found two reward
// actions beyond bond/unbond. Real lake bytes confirm the exact field
// sets (ledgers 53587626 / 53588319, stake contracts CBRGNWGAC25… /
// CAF3UJ45ZQJ…):
//
//	withdraw_rewards   (2 events): user, reward_token
//	distribute_rewards (1 event):  asset
//
// Neither event carries an amount. It surfaces on the reward token's own SEP-41
// `transfer` event in the SAME op (event_index+1), which is NOT correlated here
// (that needs a cross-decoder join against sep41_transfers). The events are
// stored with a NULL amount rather than a misleading "0"
// (see phoenix_stake_events.amount, migration 0098).
//
// distribute_rewards is a POOL-WIDE announcement with no user field on the wire,
// so it is decoded directly from its single event, not through the correlation
// buffer.
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
// Every shape below was measured in the lake on a gated emitter; real
// rows are pinned in test/fixtures/phoenix/event-shapes. Each is a
// two-String-topic event with a single-value body, decoded without a
// correlation buffer.
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
// toggle_trading is recognised but projects no row: serving it needs an
// admin_action slug the phoenix_admin_events CHECK does not allow yet.
const (
	EventActionCreateDistributionFlow = "create_distribution_flow"
	EventActionStakeMigration         = "Stake: Migration: "
	EventActionStake                  = "Stake"
	EventActionFactory                = "Factory"
	EventActionBlendPool              = "blend_pool"
	EventActionToggleTrading          = "toggle_trading"

	// StakeAction* are the phoenix_stake_events.action slugs for the
	// stake-contract lifecycle events (migration 0195).
	StakeActionMigrationStarted   = "migration_started"
	StakeActionMigrationQueried   = "migration_queried"
	StakeActionMigrationCompleted = "migration_completed"

	// AdminAction* for the factory and blend-pool configuration events
	// (phoenix_admin_events.admin_action, migration 0195).
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

	// Deliberately no XLM SAC constant here. "CDLZFC3SY…" is NOT the
	// XLM SAC on any network; it is the synthetic contract id used
	// across test/integration fixtures. There is exactly one native-XLM
	// SAC per network, derivable from canonical.Asset.SacContractID()
	// and pinned by internal/canonical/sac_test.go. Use
	// aquarius.MainnetXLMSAC (CAS3J7GY…) or canonical.XLMSacContractID.
)

// MainnetPools is the curated gated pool set (ADR-0040 §1 mechanism
// 2 — curated-set registry). Source: the factory's `query_pools()`
// RPC view cross-checked against lake event activity, recorded in
// docs/protocols/phoenix.md. The factory's
// `("create","liquidity_pool")` events ARE in the lake, from ledger
// 51,572,026 (real captures: test/fixtures/phoenix/factory-create), and
// the decoder self-registers from them, so this list is a cold-start
// warm root, the same role blend's and sushiswap_v3's curated tables
// play, not the sole trust root. It still matters: it covers the pools
// whose creation event is outside any window being streamed. A pool
// missing from BOTH this list and the factory's in-window announcement
// fail-closes and surfaces as an ADR-0033
// recognition gap (visible, never silently mis-attributed).
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
	// An XYK String-schema pool the query_pools() snapshot missed.
	// VERIFIED genuine by factory deployment: it co-occurs in the
	// phoenix factory's pool-create transaction at ledger 51,572,101
	// (deployed together with its stake contract CDP6DT2Y…), and emits
	// the 8-event ScvString swap (23,672 field-events) +
	// provide/withdraw_liquidity + ("initialize","XYK LP token_*")
	// surface. Without it, its 455 served phoenix_liquidity rows score
	// expected=0 under the gated re-derive (swap activity ended ~ledger
	// 54.5M). Evidence: r1 lake stellar.contract_events, factory
	// CB4SVAWJ… create-tx co-occurrence.
	"CAZ6W4WHVGQBGURYTUOLCUOOHW6VQGAAPSPCD72VEDZMBBPY7H43AYEC",
}

// MainnetStakeContracts — the per-pool stake contracts that emit
// bond/unbond (separate addresses NOT returned by query_pools();
// enumerated from lake activity, docs/protocols/phoenix.md). The
// page notes more may exist (one per pool) that haven't emitted yet
// — an unlisted one fail-closes into a recognition gap and gets
// added here.
var MainnetStakeContracts = []string{
	"CBRGNWGAC25CPLMOAMR7WBPOF5QTFA5RYXQH4DEJ4K65G2QFLTLMW7RO",
	"CAF3UJ45ZQJP6USFUIMVMGOUETUTXEC35R2247VJYIVQBGKTKBZKNBJ3",
	// CBBUVHCE… is deliberately absent: a bond-instrument contract whose WASM
	// has none of the stake literals; it only shares the "bond" topic word.
	// The 13 below are genuine per-pool stake contracts the lake-activity
	// snapshot missed; several still emit near tip, so leaving them out of the
	// gate drops live events. Each emits the phoenix stake surface (bond/unbond,
	// withdraw_rewards, distribute_rewards, create_distribution_flow,
	// ("initialize","LP Share token staking contract")). VERIFIED genuine
	// (r1 lake stellar.contract_events):
	//   • the first 11 each co-occur in their pool's phoenix-factory create
	//     transaction (the factory deploys pool + stake together), paired 1:1
	//     with a curated pool above;
	//   • CDOXQONPND… shares 260 transactions with curated phoenix pools and is
	//     driven by the phoenix reward keeper CBZ7M5B3Y4WW…, which also drives
	//     the seeded stakes above;
	//   • CDEQYRWFU… is driven by that same keeper and emits the phoenix stake
	//     v1.1 migration events a foreign contract has no reason to replicate.
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

// MainnetMapPools are Phoenix pools running the NEWER pool WASM whose
// swap emits a SINGLE ScvSymbol("swap") event with an ScvMap body (all
// 8 fields as underscore-spelled Symbol keys) instead of the older 8
// ScvString-tuple events (Q5). Same factory + `("create",
// "liquidity_pool")` event set; enumerated from the factory create-
// event walk cross-checked against lake activity (docs/protocols/
// phoenix.md). Both schemas are gated + decoded (decode.go:
// actionSwap / actionSwapMap). Because gating is by contract identity,
// a curated pool that upgrades from the String to the Map shape in
// place (ingest-pipeline.md#contract-schema-evolution) is already covered — only the
// decode dispatch depends on the topic shape, not this list.
// CBENABXP's factory create landed in the same window as a factory
// "Updated Config" event.
var MainnetMapPools = []string{
	"CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S",
}

// MainnetGatedSet is the full curated child set the decoder seeds:
// String-schema pools + Map-schema pools + stake contracts. The
// multihop relay is deliberately EXCLUDED — it emits no
// swap/liquidity/stake events (it relays to pools), so gating loses
// nothing (docs/protocols/phoenix.md).
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetPools)+len(MainnetMapPools)+len(MainnetStakeContracts))
	out = append(out, MainnetPools...)
	out = append(out, MainnetMapPools...)
	out = append(out, MainnetStakeContracts...)
	return out
}

// Pre-encoded base64 SCVal::String blobs for topic[0] and topic[1],
// computed at init via scval.MustEncodeString. Phoenix emits both
// topic positions as Strings (not Symbols) because the pool contract
// publishes `(str_literal, str_literal)` tuples — soroban-sdk
// serializes string literals as ScvString. Verified against a real
// mainnet capture.
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

// TopicSymbolSwapMap is the topic[0] of the NEWER single-event Map-body
// swap schema (pools on the newer WASM, e.g. CBENABXP…): a SINGLE
// ScvSymbol("swap") topic (disc 0x0F) whose body is an ScvMap of every
// swap field — distinct from the older 8-event ScvString("swap")
// schema above (disc 0x0E). The Map keys are Symbols spelled with
// underscores ("actual_received_amount"), not the older spaced String
// ("actual received amount"). Decoded by decode.go::decodeSwapMap; see
// README Q5 and docs/architecture/ingest-pipeline.md#contract-schema-evolution (Soroban
// pools upgrade in place and can change event SHAPE, not just fields).
var TopicSymbolSwapMap = scval.MustEncodeSymbol(EventActionSwap)

// The Map-schema pool WASM publishes provide_liquidity / withdraw_liquidity
// the same way: one ScvSymbol topic, ScvMap body. Decoded by
// decode.go::decodeProvideLiquidityMap / decodeWithdrawLiquidityMap.
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

// Liquidity-management topic[0] encodings + topic[1] field names.
// Same ScString-discriminator reasoning as swap above: contracts
// publish via tuple-literals like
// `.publish(("provide_liquidity", "sender"), …)` so both slots
// serialise as ScVal::String.
var (
	TopicSymbolProvideLiquidity  = scval.MustEncodeString(EventActionProvideLiquidity)  // topic[0]
	TopicSymbolWithdrawLiquidity = scval.MustEncodeString(EventActionWithdrawLiquidity) // topic[0]
	TopicSymbolBond              = scval.MustEncodeString(EventActionBond)              // topic[0]
	TopicSymbolUnbond            = scval.MustEncodeString(EventActionUnbond)            // topic[0]
	TopicSymbolAdmin             = scval.MustEncodeString(EventActionAdmin)             // topic[0] for the 4 admin variants
	TopicSymbolInitialize        = scval.MustEncodeString(EventActionInitialize)        // topic[0] for the 2 init variants

	// Factory pool announcement — ("create","liquidity_pool"), both
	// ScvString. Confirmed against the real lake captures under
	// test/fixtures/phoenix/factory-create (old and new pools alike).
	// Because they are Strings and not Symbols, the lake's
	// convenience column topic_0_sym is EMPTY for these rows — a lake
	// walk keyed on it matches nothing, which is why the re-derive
	// prefilter matches topics_xdr (internal/storage/clickhouse/
	// event_reader.go).
	TopicSymbolCreate        = scval.MustEncodeString(EventActionCreate)        // topic[0]
	TopicCreateLiquidityPool = scval.MustEncodeString(FieldCreateLiquidityPool) // topic[1]

	// initialize topic[1] variants — the pool announces its two tokens
	// as ("initialize", "XYK LP token_a" | "XYK LP token_b").
	TopicInitTokenA = scval.MustEncodeString("XYK LP token_a") // topic[1]
	TopicInitTokenB = scval.MustEncodeString("XYK LP token_b")

	// TopicInitLPShareStaking is the per-pool STAKE contract's initialize
	// topic[1] — ("initialize", "LP Share token staking contract"). Unlike
	// the pool's two token_a/token_b announcements above, this is a stake
	// contract announcing its own LP-share token at deploy (body = a single
	// Address). It maps to NO phoenix_initialize row (that table models the
	// pool's token slots); it is recognized-but-NOT-projected — the raw
	// event is preserved in the soroban_events landing zone (ADR-0029), same
	// stance as the actionUnknown / 0-mainnet-occurrence admin events.
	// The stake contracts are in the gated set, so these events pass
	// Matches(); recognising this topic[1] in decodeInitializeEvent keeps it
	// from erroring on them. Otherwise 20 real lake events (20 ledgers,
	// first=51,572,026) would count as undecodable-but-matched blind spots
	// in the ADR-0033 projection re-derive (reconcile.go).
	TopicInitLPShareStaking = scval.MustEncodeString("LP Share token staking contract")

	// admin (governance rotation) topic[1] variants — ("XYK Pool: ",
	// <phrase>). The phrases include a trailing space, faithful to
	// pool/src/contract.rs:784-836.
	TopicAdminReplaceRequested = scval.MustEncodeString("Admin replacement requested by old admin: ") // topic[1]
	TopicAdminReplaceSet       = scval.MustEncodeString("Replace with new admin: ")
	TopicAdminUndo             = scval.MustEncodeString("Undo admin change: ")
	TopicAdminAccepted         = scval.MustEncodeString("Accepted new admin: ")

	// adminActionByTopic maps each admin topic[1] blob to its stored
	// slug (see the AdminAction* constants).
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
	// ErrUnknownField — topic[1] didn't match any of the 8 expected
	// field names. Usually means a non-swap event (e.g. deposit,
	// withdraw) — classified as "not our problem" and skipped.
	ErrUnknownField = errors.New("phoenix: unknown swap field")

	// ErrIncompleteSwap — fewer than 8 fields populated when asked
	// to finalise. Should never bubble up in normal flow; buffer
	// only returns complete RawSwaps.
	ErrIncompleteSwap = errors.New("phoenix: incomplete swap (need 8 fields)")

	// ErrMalformedPayload — field values don't match expected types
	// or carry a negative amount.
	ErrMalformedPayload = errors.New("phoenix: malformed swap payload")

	// ErrZeroAmountSwap marks a fully-decoded swap with a ZERO leg — a
	// genuine dust swap, not a malformed payload. The dispatcher adapter
	// treats it (like a self-pair, canonical.ErrPairMismatch) as a
	// recognised no-op: zero rows, no decode error, so the ADR-0033
	// re-derive counts expected=0 instead of failing the verdict closed
	// (aquarius ErrZeroAmountTrade, comet ErrNonPositiveAmounts).
	ErrZeroAmountSwap = errors.New("phoenix: zero-amount swap (recognised no-op)")

	// ErrIncompleteLiquidity — bubbles up if decodeProvideLiquidity /
	// decodeWithdrawLiquidity is called before every required field
	// has landed. Defence-in-depth: the buffer only returns completed
	// records, so callers shouldn't see this in normal flow.
	ErrIncompleteLiquidity = errors.New("phoenix: incomplete liquidity event")

	// ErrIncompleteStake — same shape as ErrIncompleteLiquidity, for
	// the bond / unbond 3-event reassembly.
	ErrIncompleteStake = errors.New("phoenix: incomplete stake event")
)
