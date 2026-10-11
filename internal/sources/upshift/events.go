// Package upshift decodes events from the Upshift tokenized yield vaults on
// Stellar (curated by Gami Labs and Stake Capital Group).
//
// A vault is ERC-4626-shaped and the vault contract IS the share token (SEP-41). RedStone prices
// `earnUSDC_FUNDAMENTAL` on that identity ([MainnetVaultEarnUSDC]), not the underlying's: a
// yield-bearing claim is a different instrument from USDC.
//
// Two vaults: earnUSDC (CCL3WITW…) and earnXLM (CC6TRAPQ…). Each underlying is proven by the SAC
// `transfer` one event ahead of a `deposit` for exactly its `assets` amount; ticker names are
// inferred, CONTRACT IDS are proven.
//
// Shapes: `deposit`/`withdraw` carry three Address topics and Map{assets, shares}. `transfer` is
// a bare i128 OR Map{amount, to_muxed_id} (CAP-67). Shares are not at the underlying's scale.
// Not decoded (zero rows, counted expected-zero by the ADR-0033 re-derive): custody-side events
// (they would double-count `deployed_assets_changed`), governance events, `approve`.
//
// Gating: the symbols are generic, so ADR-0035 contract-identity gating is essential; with no
// creation event the ADR-0040 curated set applies ([MainnetGatedSet] plus the DB warm).
package upshift

import (
	"errors"
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is stamped on every emitted event and every `upshift_vault_events` row.
const SourceName = "upshift"

// EventKind is the [consumer.Event] discriminator; one type carries all four decoded kinds (the
// sep41_transfers shape).
const EventKind = "upshift.vault_event"

// Vault contract ids on pubnet. They are IDENTITIES, not tunables: changing one re-points a served
// row, and for earnUSDC a published oracle price, at a different instrument.
const (
	// MainnetVaultEarnUSDC is the earnUSDC vault (native USDC in, shares out) and the base asset of
	// the RedStone `earnUSDC_FUNDAMENTAL` feed; internal/sources/redstone imports THIS constant so
	// the two cannot drift.
	MainnetVaultEarnUSDC = "CCL3WITWFFXIHV2I52ECV5DPIEOFSTU3PBPR53ILPLF2IP5KHECXRUTY"

	// MainnetVaultEarnXLM is the earnXLM vault: native XLM in, earnXLM shares out. Identified
	// like earnUSDC (package doc): one of only two contracts emitting the bespoke symbols, with
	// its underlying pinned by the native-SAC `transfer` sharing a tx with its first `deposit`.
	MainnetVaultEarnXLM = "CC6TRAPQD3NK7THUKWPV5SL2JHKQGNXZVB6S6MVYFSLRWAKEFUWZKZ7J"
)

// GenesisLedger is the protocol's first pubnet event (earnUSDC's `admin_set`). It is the density
// denominator (ADR-0031) and the gap detector's floor: an exact observed value, never an estimate.
const GenesisLedger uint32 = 62_623_313

// VaultMeta is the curated, lake-verified description of one vault. The underlying is EVIDENCE of
// what the vault holds; nothing here converts between the two scales or invents an asset.
type VaultMeta struct {
	// Label is the share ticker, INFERRED from the underlying (no event carries it). Never an
	// identity; the contract id is.
	Label string

	// Underlying is the C-strkey of the SAC whose `transfer` accompanies this vault's `deposit`
	// in the same transaction, for the same amount.
	Underlying string

	// UnderlyingAsset is the SEP-11 asset string in that SAC transfer's topic[3], the proof of
	// what Underlying is.
	UnderlyingAsset string

	// FirstEventLedger is the vault's own first event in the lake.
	FirstEventLedger uint32
}

// MainnetVaults is the curated Upshift vault set on pubnet, the ADR-0040 trust root; the lake
// sweep in the package doc shows these are the only contracts emitting the bespoke vocabulary.
//
// Completeness is load-bearing: a vault missing here has its events DROPPED, not
// mis-attributed. Re-run the bespoke-symbol sweep, or add a `protocol_contracts` row, to admit
// a new one without a redeploy.
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

// MainnetGatedSet returns the vault allowlist seeded into the decoder's identity registry.
// It is derived from [MainnetVaults] so the gated and documented sets cannot drift, and sorted
// so the projector's contract-id prefilter and the reconcile's lake read are byte-stable.
func MainnetGatedSet() []string {
	out := make([]string, 0, len(MainnetVaults))
	for id := range MainnetVaults {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Decoded event kinds, stamped on Event.Kind and bound by the `upshift_vault_events.event_kind` CHECK.
const (
	EventDeposit               = "deposit"
	EventWithdraw              = "withdraw"
	EventTransfer              = "transfer"
	EventDeployedAssetsChanged = "deployed_assets_changed"
)

// Recognized-but-undecoded kinds, classified so the ADR-0033 re-derive counts their ledgers as
// expected-ZERO rather than blind spots (package doc says why each is skipped).
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

// Pre-encoded base64 SCVal Symbol blobs for byte-equality matching on topic[0], so the reject path
// parses no SCVal. Each was checked against the lake's `topics_xdr[1]`.
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

// Decoder errors. ErrMalformedPayload is INDETERMINATE (body off the proven schema) and stays an
// error so the completeness verdict goes blind rather than silently under-counting.
var (
	ErrNotUpshiftEvent  = errors.New("upshift: topic[0] is not an Upshift vault event")
	ErrMalformedPayload = errors.New("upshift: event body does not match the proven schema")
	ErrShortTopic       = errors.New("upshift: topic vector too short for event variant")
)
