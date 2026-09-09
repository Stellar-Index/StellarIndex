// Package upshift decodes on-chain events from the Upshift tokenized
// vaults on Stellar — institutional yield vaults curated by Gami Labs
// and Stake Capital Group, with Upshift supplying the vault contract.
//
// A vault is an ERC-4626-shaped tokenized vault: it accepts one
// underlying asset and mints proportional SHARES. The vault contract IS
// the share token — it emits the SEP-41 `transfer` / `approve` events
// for its own shares — so one contract id serves both "the vault" and
// "the share instrument". That is the same identity the RedStone feed
// registry prices `earnUSDC_FUNDAMENTAL` on
// ([MainnetVaultEarnUSDC], internal/sources/redstone/feeds.go), and it
// is deliberately NOT the underlying's id: a yield-bearing claim on
// USDC is a different instrument from USDC.
//
// # Shape of the protocol, proven from the certified ClickHouse lake
//
// Swept 2026-09-09 over `stellar.contract_events` (ledgers up to
// 64,345,4xx). TWO vault contracts exist and no others: the two are the
// ONLY contracts in the lake at or after ledger 62,000,000 that emit
// any of this protocol's five bespoke symbols (`deployed_assets_changed`,
// `wallet_deployed_updated`, `deposit_to_subaccount`,
// `withdraw_from_subaccount`, `wallet_net_deployed_seeded`).
//
//	CCL3WITW… (earnUSDC)   first event `admin_set` @ 62,623,313
//	  deposit 116 · deployed_assets_changed 37 · deposit_to_subaccount 36
//	  withdraw 35 · wallet_deployed_updated 17 · transfer 8 · approve 6
//	  withdraw_from_subaccount 4 · admin_set / operator_set /
//	  subaccount_added / wallet_net_deployed_seeded 1 each
//
//	CC6TRAPQ… (earnXLM)    first event `admin_set` @ 62,623,319
//	  deposit 68 · withdraw 38 · deployed_assets_changed 24
//	  wallet_deployed_updated 15 · deposit_to_subaccount 6 · transfer 1
//	  approve 1 · admin_set / operator_set / subaccount_added /
//	  wallet_net_deployed_seeded 1 each
//
// The two were deployed in the same batch minutes apart — `admin_set` 6
// ledgers apart, `operator_set` 11, `subaccount_added` 8 — onto the same
// admin (GDYNPLQ7…) and the same operator (GCDR6QB2…), each with its OWN
// custody subaccount (GDJ7WZYS… for earnUSDC, GAPUSI7E… for earnXLM).
// Each vault's UNDERLYING asset is proven by the SAC `transfer` that
// lands in the SAME transaction, one event index ahead of the `deposit`,
// for exactly the `assets` amount the deposit reports:
//
//	earnUSDC ← CCW67TSZ… `transfer` … String("USDC:GA5ZSEJY…KZVN"), amount 2000000
//	           == deposit.assets 2000000              (ledger 62,938,336)
//	earnXLM  ← CAS3J7GY… `transfer` … String("native"),  amount 5000000
//	           == deposit.assets 5000000              (ledger 62,638,654)
//
// The ticker names are the one inferred part: no event carries the
// share token's symbol, so "earnUSDC" / "earnXLM" are read off the
// underlying each vault holds plus the protocol's published pair. The
// CONTRACT IDS are what the decoder gates on and those are proven.
//
// A third address, CC2DNHE5EFPPVJ47BJCRIQCEZ7KVHOATVR5QMNBEKKZQF2EZO7U7YTHT,
// circulated as this vault's address and has ZERO events of any kind in
// the lake. It is not a vault and is deliberately absent from
// [MainnetVaults]; TestMainnetVaults_ExcludesTheZeroEventAddress pins that.
//
// # Event shapes (every field name read off real lake bytes)
//
//	deposit    topics [Symbol, Address, Address, Address]
//	           body   Map{ assets: i128, shares: i128 }
//	withdraw   topics [Symbol, Address, Address, Address]
//	           body   Map{ assets: i128, shares: i128 }
//	transfer   topics [Symbol, from Address, to Address]
//	           body   i128 OR Map{ amount: i128, to_muxed_id: … }  (CAP-67)
//	deployed_assets_changed
//	           topics [Symbol, operator Address]
//	           body   Map{ old_amount: i128, new_amount: i128 }
//
// The three `deposit` / `withdraw` topic addresses are (caller,
// receiver, owner) — the OpenZeppelin ERC-4626 `Withdraw` ordering.
// Exactly TWO events in the two vaults' combined history carry three
// DIFFERENT addresses, one per vault, and they corroborate each other:
//
//	63,812,795  earnXLM   (C CD5YZRFQ…, G GCNC7GXV…, C CD5YZRFQ…)
//	63,812,816  earnUSDC  (C CCJ43PID…, G GCNC7GXV…, C CCJ43PID…)
//
// The SAME G account sits in the middle slot of both while the outer
// contract differs — one holder redeeming both vaults in one session, 21
// ledgers apart, through a different router each time. Six ledgers
// before the earnUSDC one, that G account approved that C contract as a
// spender (`approve` @ 63,812,810) and transferred its shares to it
// (`transfer` @ 63,812,811). So at burn time the contract both CALLED
// and OWNED the shares, and the G account is who received the
// underlying — (caller, receiver, owner), not (caller, owner, receiver).
//
// CAVEAT, stated because it matters: all 184 `deposit` events carry the
// three addresses IDENTICAL, so deposit's reading is carried over from
// withdraw (same topic arity, same body schema) and is not
// independently proven.
//
// Shares are NOT at the underlying's scale. Every genesis-era deposit
// mints shares == assets × 1,000,000 exactly (2000000 → 2000000000000;
// 5000000 → 5000000000000), i.e. the OpenZeppelin ERC-4626 decimals
// OFFSET of 6 that hardens a vault against the inflation attack. Later
// events drift off that ratio as the share price accrues (withdraw @
// 63,812,816 redeems 9,919,211,002,150 shares for 10,000,000 assets —
// ~0.8% above par). This package therefore stores assets and shares as
// RAW i128 (canonical.Amount / NUMERIC per ADR-0003) and does NOT
// divide one by the other or apply a decimals assumption; the offset is
// recorded here as evidence, not applied as a constant.
//
// # What is NOT decoded, and why
//
//   - deposit_to_subaccount / withdraw_from_subaccount /
//     wallet_deployed_updated / wallet_net_deployed_seeded — the
//     CUSTODY-side mirror of the capital that
//     `deployed_assets_changed` already reports at vault level. They
//     name the custodian's internal wallet topology (one subaccount
//     today) and report the same movement a second time; decoding them
//     into the same table would double-count deployed capital.
//   - subaccount_added / admin_set / operator_set — governance. No
//     economic state.
//   - approve — a SEP-41 allowance, not a balance change. The vault's
//     share-token audit trail (`transfer` + `approve`) has a home of
//     its own in internal/sources/sep41_transfers whenever an operator
//     puts the vault in `watched_sep41_contracts`; `transfer` is
//     decoded HERE as well because a share movement is vault economics
//     (it reassigns the claim without minting or burning) and the two
//     sources write different tables, so there is no double write.
//
// All eight are still RECOGNIZED by [classify] and gated by
// [Decoder.Matches]: they decode to ZERO rows with no error, so the
// ADR-0033 re-derive counts their ledgers as expected-zero instead of
// going blind on them.
//
// # Gating
//
// ADR-0035 requires contract-identity gating, and this protocol makes
// the case plainly: `deposit`, `withdraw` and `transfer` are among the
// most generic symbols on the network — a bounded 20,000-ledger census
// (63,000,000–63,020,000) found FOUR distinct contracts emitting
// `deposit` and 77 such events, of which the vaults are a minority. A
// topic-only decoder would attribute a stranger's vault flows to this
// protocol and, worse, would let any contract publish share/asset
// numbers under this source's name.
//
// There is no factory to anchor on: neither vault has a creation event
// in the lake (each one's first event is its own `admin_set`), so the
// ADR-0040 CURATED-SET mechanism applies rather than the factory
// fan-out — the same shape internal/sources/comet uses. The trust root
// is [MainnetGatedSet] plus the `protocol_contracts` DB warm, and a new
// Upshift vault must be operator-admitted before its events are
// attributed. That fails CLOSED: an un-admitted vault's events surface
// as an ADR-0033 recognition gap, never as a silent mis-attribution.
package upshift

import (
	"errors"
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the canonical string stamped on every event this
// package emits and on every `upshift_vault_events` row.
const SourceName = "upshift"

// EventKind is the [consumer.Event] discriminator for every row this
// package emits. One event type carries all four decoded kinds (the
// sep41_transfers shape), so there is one kind string.
const EventKind = "upshift.vault_event"

// Vault contract ids on pubnet. Held as named constants because they
// are IDENTITIES, not tunables: changing one re-points a served row —
// and, for earnUSDC, a published oracle price — at a different
// instrument.
const (
	// MainnetVaultEarnUSDC is the earnUSDC vault: native Stellar USDC
	// in, earnUSDC shares out. It is also the base asset of the
	// RedStone `earnUSDC_FUNDAMENTAL` feed — internal/sources/redstone
	// imports THIS constant so the two can never drift apart.
	MainnetVaultEarnUSDC = "CCL3WITWFFXIHV2I52ECV5DPIEOFSTU3PBPR53ILPLF2IP5KHECXRUTY"

	// MainnetVaultEarnXLM is the earnXLM vault: native XLM in, earnXLM
	// shares out. Identified from the lake on 2026-09-09 by the same
	// method used for earnUSDC (see the package doc): it is one of only
	// two contracts emitting this protocol's bespoke symbols, it was
	// deployed in the same batch onto the same operator and custody
	// subaccount, and its underlying is pinned by the native-SAC
	// `transfer` that shares a transaction with its first `deposit`.
	MainnetVaultEarnXLM = "CC6TRAPQD3NK7THUKWPV5SL2JHKQGNXZVB6S6MVYFSLRWAKEFUWZKZ7J"
)

// GenesisLedger is the ledger of the protocol's first event on pubnet —
// earnUSDC's `admin_set` at 62,623,313, six ledgers ahead of earnXLM's.
// This is the density denominator (ADR-0031) and the gap detector's
// floor: an exact observed value, never a rounded estimate.
const GenesisLedger uint32 = 62_623_313

// VaultMeta is the curated, lake-verified description of one vault. The
// underlying is recorded as EVIDENCE of what the vault holds — nothing
// in this package converts between the two scales (see the package doc
// on the decimals offset), and no row is written with an invented asset.
type VaultMeta struct {
	// Label is the share token's ticker as published by the protocol.
	// INFERRED from the underlying, not read from any event — no event
	// carries the share symbol. Never used as an identity; the contract
	// id is.
	Label string

	// Underlying is the C-strkey of the Stellar Asset Contract whose
	// `transfer` accompanies this vault's `deposit` in the same
	// transaction, for the same amount.
	Underlying string

	// UnderlyingAsset is the SEP-11 asset string that same SAC transfer
	// carries in its topic[3] — the proof of what Underlying is.
	UnderlyingAsset string

	// FirstEventLedger is the vault's own first event in the lake.
	FirstEventLedger uint32
}

// MainnetVaults is the curated Upshift vault set on pubnet — the
// ADR-0040 curated-set trust root. Verified complete against the lake
// on 2026-09-09 (see the package doc): these are the only two contracts
// emitting the protocol's bespoke event vocabulary.
//
// Completeness of this map is load-bearing exactly as a factory set
// would be: a vault missing here has its events DROPPED, not
// mis-attributed. Re-run the bespoke-symbol sweep to admit a new one,
// or admit it as a `protocol_contracts` row without a redeploy.
var MainnetVaults = map[string]VaultMeta{
	MainnetVaultEarnUSDC: {
		Label:            "earnUSDC",
		Underlying:       "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		UnderlyingAsset:  "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		FirstEventLedger: 62_623_313,
	},
	MainnetVaultEarnXLM: {
		Label:            "earnXLM",
		Underlying:       "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		UnderlyingAsset:  "native",
		FirstEventLedger: 62_623_319,
	},
}

// MainnetGatedSet returns the curated vault allowlist the decoder seeds
// into its contract-identity registry.
//
// Derived from [MainnetVaults] rather than listed a second time — a
// hand-kept parallel list is exactly the drift that would gate one set
// and document another — and sorted so the projector's contract-id
// prefilter and the reconcile's lake read are byte-stable across runs.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetVaults))
	for id := range MainnetVaults {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Decoded event kinds — the values stamped on Event.Kind and on the
// `upshift_vault_events.event_kind` CHECK constraint (migration 0157).
const (
	EventDeposit               = "deposit"
	EventWithdraw              = "withdraw"
	EventTransfer              = "transfer"
	EventDeployedAssetsChanged = "deployed_assets_changed"
)

// Recognized-but-undecoded kinds. Gated and classified so the ADR-0033
// re-derive counts their ledgers as expected-ZERO rather than treating
// them as blind spots; see the package doc for why each is skipped.
const (
	EventApprove                 = "approve"
	EventDepositToSubaccount     = "deposit_to_subaccount"
	EventWithdrawFromSubaccount  = "withdraw_from_subaccount"
	EventWalletDeployedUpdated   = "wallet_deployed_updated"
	EventWalletNetDeployedSeeded = "wallet_net_deployed_seeded"
	EventSubaccountAdded         = "subaccount_added"
	EventAdminSet                = "admin_set"
	EventOperatorSet             = "operator_set"
)

// Pre-encoded base64 SCVal Symbol blobs, for byte-equality matching on
// topic[0] with no SCVal parse on the reject path. Every one of these
// was checked byte-for-byte against the lake's own `topics_xdr[1]`.
var (
	TopicSymbolDeposit               = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolWithdraw              = scval.MustEncodeSymbol(EventWithdraw)
	TopicSymbolTransfer              = scval.MustEncodeSymbol(EventTransfer)
	TopicSymbolDeployedAssetsChanged = scval.MustEncodeSymbol(EventDeployedAssetsChanged)
	TopicSymbolApprove               = scval.MustEncodeSymbol(EventApprove)
	TopicSymbolDepositToSubaccount   = scval.MustEncodeSymbol(EventDepositToSubaccount)
	TopicSymbolWithdrawFromSub       = scval.MustEncodeSymbol(EventWithdrawFromSubaccount)
	TopicSymbolWalletDeployedUpd     = scval.MustEncodeSymbol(EventWalletDeployedUpdated)
	TopicSymbolWalletNetDeplSeeded   = scval.MustEncodeSymbol(EventWalletNetDeployedSeeded)
	TopicSymbolSubaccountAdded       = scval.MustEncodeSymbol(EventSubaccountAdded)
	TopicSymbolAdminSet              = scval.MustEncodeSymbol(EventAdminSet)
	TopicSymbolOperatorSet           = scval.MustEncodeSymbol(EventOperatorSet)
)

// Decoder errors. ErrMalformedPayload is INDETERMINATE — a body that
// did not match the proven schema — and stays an error so the
// completeness verdict goes blind rather than silently under-counting.
var (
	ErrNotUpshiftEvent  = errors.New("upshift: topic[0] is not an Upshift vault event")
	ErrMalformedPayload = errors.New("upshift: event body does not match the proven schema")
	ErrShortTopic       = errors.New("upshift: topic vector too short for event variant")
)
