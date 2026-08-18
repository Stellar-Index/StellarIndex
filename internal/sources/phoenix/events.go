// Package phoenix ingests trade events from the Phoenix Soroban DEX.
//
// Design reference: internal/sources/phoenix/README.md and
// docs/discovery/dexes-amms/phoenix.md. Read the Q1–Q5 quirks
// before modifying the decoder, especially the 8-events-per-swap
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
// instead. Verified 2026-04-23 against mainnet: every Phoenix swap
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
// Phoenix's pool contract (both volatile `contracts/pool/` and
// stableswap `contracts/pool_stable/`) emits the same N-event-per-
// action shape as `swap` for liquidity management:
//
//	provide_liquidity (5 events): sender, token_a, token_a-amount,
//	                              token_b, token_b-amount
//	withdraw_liquidity (4 events): sender, shares_amount,
//	                               return_amount_a, return_amount_b
//
// The withdraw path also OPTIONALLY emits a 5th
// `("withdraw_liquidity", "auto unbonded")` event with a tuple body
// (stake_amount, stake_timestamp). We classify it but do not require
// it for the withdraw correlation to complete — most withdrawals
// don't auto-unbond.
//
// Stake contract (`contracts/stake/`) emits its own 3-event-per-
// action shape for bond/unbond:
//
//	bond   (3 events): user, token, amount
//	unbond (3 events): user, token, amount
//
// Field strings are the literal contract source — keep spellings
// identical, including the `-amount` hyphens on the liquidity-token
// fields. The contract emits all topics as String (not Symbol):
// soroban-sdk serialises tuple-literal strings as ScVal::String.

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
// ROADMAP #89 residual (2026-07-10): a read-only lake topic census
// against the gated stake-contract set found two more stake-contract
// actions classifyAny didn't recognize. Real-lake-bytes confirmed the
// exact field sets (ledgers 53587626 / 53588319, stake contracts
// CBRGNWGAC25… / CAF3UJ45ZQJ…):
//
//	withdraw_rewards   (2 events): user, reward_token
//	distribute_rewards (1 event):  asset
//
// Neither event carries an amount. The paid-out / distributed amount
// surfaces on the reward token's own SEP-41 `transfer` event emitted
// in the SAME op (event_index+1, verified on both real samples) — a
// SAC contract event, not a stake-contract field-event, so it is NOT
// correlated here (would require cross-decoder joins on tx_hash+
// op_index against sep41_transfers, out of scope for this pass). The
// events are stored with a NULL amount rather than a misleading "0"
// (see phoenix_stake_events.amount, migration 0098).
//
// distribute_rewards is a POOL-WIDE announcement — it carries no user
// field on the wire (verified: every real sample across 3 stake
// contracts omits it) — so it is decoded directly from its single
// event rather than through the correlation buffer.
const (
	EventActionWithdrawRewards   = "withdraw_rewards"
	EventActionDistributeRewards = "distribute_rewards"

	FieldWRUser               = "user"
	FieldWRRewardToken        = "reward_token"
	WithdrawRewardsFieldCount = 2

	FieldDRAsset = "asset"
)

// Mainnet contract addresses — Phase-1 verified against
// Phoenix-Protocol-Group/phoenix-contracts `scripts/*.sh`.
const (
	MainnetFactory  = "CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI"
	MainnetMultihop = "CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G"

	// XLM SAC as referenced by Phoenix's scripts. Note this is
	// REMOVED 2026-07-26 (audit C4-012 follow-through): this constant
	// carried "CDLZFC3SY…", which is NOT the XLM SAC on any network —
	// it is the synthetic contract id used across test/integration
	// fixtures. The comment above it ("Phoenix uses a different
	// canonical form") rationalised a copy-paste error; there is
	// exactly one native-XLM SAC per network, derivable from
	// canonical.Asset.SacContractID() and pinned by
	// internal/canonical/sac_test.go. The constant was never
	// referenced outside its declaration — kept unused, it was a
	// booby trap: the moment anything read it, XLM would stop being
	// XLM. Use aquarius.MainnetXLMSAC (the correct CAS3J7GY… value)
	// or canonical.SacContractID directly.
)

// MainnetPools is the curated gated pool set (ADR-0040 §1 mechanism
// 2 — curated-set registry). Source: the factory's `query_pools()`
// RPC view cross-checked against lake event activity, recorded in
// docs/protocols/phoenix.md (last verified 2026-06-12). The factory's
// `("create","liquidity_pool")` events PREDATE the lake's earliest
// ledger, so live self-registration can never seed these — this
// in-code seed is load-bearing, not a warm-start optimisation. A
// pool missing from this list fail-closes and surfaces as an
// ADR-0033 recognition gap (visible, never silently mis-attributed).
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
	// Added 2026-08-18 (phoenix projection-completeness gap): a legacy
	// XYK String-schema pool the 2026-05-01 query_pools() snapshot
	// missed. VERIFIED genuine by factory deployment — it co-occurs in
	// the phoenix factory's pool-create transaction at ledger 51,572,101
	// (deployed together with its stake contract CDP6DT2Y…), and emits
	// the legacy 8-event ScvString swap (23,672 field-events) +
	// provide/withdraw_liquidity + ("initialize","XYK LP token_*")
	// surface. Its 455 served phoenix_liquidity rows scored expected=0
	// under the gated re-derive until this seeding (swap activity ended
	// ~ledger 54.5M). Evidence: r1 lake stellar.contract_events, factory
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
	"CBBUVHCEML7UE46XXZXLTMGKFMKX7KOC2XAKI3TW6WBQBKWMSARMU3YM",
	// Added 2026-08-18 (phoenix projection-completeness gap): 13 genuine
	// per-pool stake contracts the 2026-05-01 lake-activity snapshot
	// missed. Together they landed 2,513 rows in phoenix_stake_events
	// that the gated re-derive scored expected=0 — and several are STILL
	// emitting near tip (e.g. CDOXQONPND… bond/unbond to ledger 64.0M),
	// so the old gate was a LIVE drop, not just a reconcile artifact.
	// Each emits the phoenix stake surface (bond/unbond → user/token/
	// amount, withdraw_rewards, distribute_rewards,
	// create_distribution_flow, ("initialize","LP Share token staking
	// contract")). VERIFIED genuine (r1 lake stellar.contract_events,
	// 2026-08-18):
	//   • the first 11 each co-occur in their pool's phoenix-factory
	//     create transaction (the factory deploys pool + stake together)
	//     — a hard on-chain deployment link, paired 1:1 with a curated
	//     pool above;
	//   • CDOXQONPND… shares 260 transactions with curated phoenix pools
	//     and is driven by the phoenix reward keeper CBZ7M5B3Y4WW…, which
	//     also drives the three seeded stakes above;
	//   • CDEQYRWFU… (created before the lake window, like the pools) is
	//     driven by that same keeper and emits the phoenix stake v1.1
	//     migration events (`Stake: Migration: `, `Start of migration for
	//     user: `, `Query for user completed: `) a foreign contract has
	//     no reason to replicate.
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
// 8 fields as underscore-spelled Symbol keys) instead of the legacy 8
// ScvString-tuple events (Q5). Same factory + `("create",
// "liquidity_pool")` event set; enumerated from the factory create-
// event walk cross-checked against lake activity (docs/protocols/
// phoenix.md). Both schemas are gated + decoded (decode.go:
// actionSwap / actionSwapMap). Because gating is by contract identity,
// a curated pool that upgrades from the String to the Map shape in
// place (contract-schema-evolution.md) is already covered — only the
// decode dispatch depends on the topic shape, not this list.
// CBENABXP appeared 2026-07-02 (factory "Updated Config" + create in
// the same window).
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
// serializes string literals as ScvString. Verified against real
// mainnet capture 2026-04-23.
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
// swap schema (post-2026-07-02 pools, e.g. CBENABXP…): a SINGLE
// ScvSymbol("swap") topic (disc 0x0F) whose body is an ScvMap of every
// swap field — distinct from the legacy 8-event ScvString("swap")
// schema above (disc 0x0E). The Map keys are Symbols spelled with
// underscores ("actual_received_amount"), not the legacy spaced String
// ("actual received amount"). Decoded by decode.go::decodeSwapMap; see
// README Q5 and docs/architecture/contract-schema-evolution.md (Soroban
// pools upgrade in place and can change event SHAPE, not just fields).
var TopicSymbolSwapMap = scval.MustEncodeSymbol(EventActionSwap)

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
	// Seeding the per-pool stake contracts into the gated set (2026-08-18)
	// made these events Matches(); recognising this topic[1] in
	// decodeInitializeEvent is what stops it erroring on them — 20 real lake
	// events (20 ledgers, first=51,572,026) that were otherwise counted as
	// undecodable-but-matched blind spots by the ADR-0033 projection
	// re-derive (reconcile.go).
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
	// or produce a nonsense Trade (zero amount, same base/quote).
	ErrMalformedPayload = errors.New("phoenix: malformed swap payload")

	// ErrIncompleteLiquidity — bubbles up if decodeProvideLiquidity /
	// decodeWithdrawLiquidity is called before every required field
	// has landed. Defence-in-depth: the buffer only returns completed
	// records, so callers shouldn't see this in normal flow.
	ErrIncompleteLiquidity = errors.New("phoenix: incomplete liquidity event")

	// ErrIncompleteStake — same shape as ErrIncompleteLiquidity, for
	// the bond / unbond 3-event reassembly.
	ErrIncompleteStake = errors.New("phoenix: incomplete stake event")
)
