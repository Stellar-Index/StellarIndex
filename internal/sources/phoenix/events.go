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
	// NOT the same address as Aquarius's XLM SAC — Phoenix uses
	// a different canonical form in its deploy scripts.
	MainnetXLMSAC = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
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
