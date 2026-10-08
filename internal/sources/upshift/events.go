// Package upshift decodes on-chain events from the Upshift tokenized
// vaults on Stellar — institutional yield vaults curated by Gami Labs
// and Stake Capital Group, with Upshift supplying the vault contract.
//
// A vault is ERC-4626-shaped: it accepts one underlying asset and mints
// proportional SHARES, and the vault contract IS the share token (it
// emits SEP-41 `transfer` / `approve` for its own shares). That is the
// identity RedStone prices `earnUSDC_FUNDAMENTAL` on
// ([MainnetVaultEarnUSDC]), deliberately NOT the underlying's id: a
// yield-bearing claim on USDC is a different instrument from USDC.
//
// # Shape of the protocol, proven from the certified ClickHouse lake
//
// TWO vault contracts exist: the only contracts at or after ledger
// 62,000,000 emitting any of the protocol's five bespoke symbols
// (`deployed_assets_changed`, `wallet_deployed_updated`,
// `deposit_to_subaccount`, `withdraw_from_subaccount`,
// `wallet_net_deployed_seeded`). They were deployed minutes apart onto
// the same admin and operator, each with its own custody subaccount:
//
//	CCL3WITW… (earnUSDC)   first event `admin_set` @ 62,623,313
//	CC6TRAPQ… (earnXLM)    first event `admin_set` @ 62,623,319
//
// Each vault's underlying is proven by the SAC `transfer` one event ahead
// of a `deposit` in the same transaction for exactly its `assets` amount
// (USDC:GA5ZSEJY… at 62,938,336; native at 62,638,654). The ticker names
// are inferred from the underlying; the CONTRACT IDS the decoder gates on
// are proven.
//
// CC2DNHE5EFPPVJ47BJCRIQCEZ7KVHOATVR5QMNBEKKZQF2EZO7U7YTHT, circulated as
// a vault address, has ZERO events in the lake and is deliberately absent
// from [MainnetVaults] (TestMainnetVaults_ExcludesTheZeroEventAddress).
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
// The `deposit` / `withdraw` topic addresses are (caller, receiver,
// owner), the OpenZeppelin ERC-4626 ordering. Two withdraws carry three
// distinct addresses (63,812,795 and 63,812,816): in the second, the G
// account in the middle slot had approved and transferred its shares to
// the outer C contract six ledgers earlier, so the contract was caller
// and owner and the G account the receiver. CAVEAT: every `deposit`
// carries three identical addresses, so deposit's ordering is carried
// over from withdraw, not independently proven.
//
// Shares are NOT at the underlying's scale: genesis-era deposits mint
// shares == assets × 1,000,000 (the ERC-4626 decimals offset of 6), and
// later events drift off that ratio as the share price accrues. Assets
// and shares are stored as RAW i128 (ADR-0003); no ratio or decimals
// assumption is applied.
//
// # What is NOT decoded, and why
//
//   - deposit_to_subaccount / withdraw_from_subaccount /
//     wallet_deployed_updated / wallet_net_deployed_seeded — the custody
//     side of what `deployed_assets_changed` already reports; decoding
//     them would double-count deployed capital.
//   - subaccount_added / admin_set / operator_set / vault_paused /
//     vault_unpaused — governance, no economic state (the pause pair was
//     first emitted at 64,715,361 by the same audited WASM 4b3d9f6b…).
//   - approve — an allowance, not a balance change. The share-token
//     audit trail lives in internal/sources/sep41_transfers when the
//     vault is watched; `transfer` is decoded HERE too because it is
//     vault economics, and the two write different tables.
//
// All ten are still RECOGNIZED by [classify] and gated by
// [Decoder.Matches]: they decode to ZERO rows with no error, so the
// ADR-0033 re-derive counts their ledgers as expected-zero instead of
// going blind on them.
//
// # Gating
//
// `deposit`, `withdraw` and `transfer` are among the most generic symbols
// on the network (a 20,000-ledger census found four contracts emitting
// `deposit`), so ADR-0035 contract-identity gating is essential. Neither
// vault has a creation event, so there is no factory to anchor on: the
// ADR-0040 CURATED-SET applies, as in internal/sources/comet. The trust
// root is [MainnetGatedSet] plus the `protocol_contracts` DB warm; a new
// vault must be operator-admitted, and until then its events surface as
// an ADR-0033 recognition gap, never a silent mis-attribution.
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
	// shares out. Identified from the lake by the same
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
// (see the package doc): these are the only two contracts
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
	EventVaultPaused             = "vault_paused"
	EventVaultUnpaused           = "vault_unpaused"
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
	TopicSymbolVaultPaused           = scval.MustEncodeSymbol(EventVaultPaused)
	TopicSymbolVaultUnpaused         = scval.MustEncodeSymbol(EventVaultUnpaused)
)

// Decoder errors. ErrMalformedPayload is INDETERMINATE — a body that
// did not match the proven schema — and stays an error so the
// completeness verdict goes blind rather than silently under-counting.
var (
	ErrNotUpshiftEvent  = errors.New("upshift: topic[0] is not an Upshift vault event")
	ErrMalformedPayload = errors.New("upshift: event body does not match the proven schema")
	ErrShortTopic       = errors.New("upshift: topic vector too short for event variant")
)
