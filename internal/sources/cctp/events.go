// Package cctp decodes Circle's CCTP v2 contract events on Stellar (Soroban) from
// TokenMessengerMinter, MessageTransmitter and CctpForwarder (Mainnet* constants below).
//
// An outbound deposit_for_burn emits both a DepositForBurn and a MessageSent in one transaction;
// inbound pairs MessageReceived with MintAndWithdraw. Correlate by (ledger, tx_hash). The
// governance/admin event list lives in decode.go and the cctp_events CHECK constraint.
//
// Per-event schemas: docs/protocols/cctp.md. Wiring: README.md section Wiring.
package cctp

import (
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this source.
const SourceName = "cctp"

// GenesisLedger is the ledger of the MessageTransmitter's first on-chain event, the lower bound for
// any re-derive or gap scan. ProtocolMeta.GenesisLedger for "cctp" must equal it.
const GenesisLedger uint32 = 62_146_641

// Mainnet contract addresses, per https://developers.circle.com/cctp/references/stellar-contracts.
const (
	MainnetTokenMessengerMinter = "CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL"
	MainnetMessageTransmitter   = "CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV"
	MainnetCctpForwarder        = "CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T"
)

// StellarDomainID is Stellar's CCTP domain identifier, as `get_local_domain()` returns it.
const StellarDomainID uint32 = 27

// Event names: the strings emitted as topic[0] by each #[contractevent].
const (
	EventDepositForBurn  = "deposit_for_burn"  // TokenMessengerMinter
	EventMintAndWithdraw = "mint_and_withdraw" // TokenMessengerMinter
	EventMessageSent     = "message_sent"      // MessageTransmitter
	EventMessageReceived = "message_received"  // MessageTransmitter
	EventMintAndForward  = "mint_and_forward"  // CctpForwarder — mint relayed onward to the recipient

	// Governance/admin events. Every contract implements the same Ownable2Step + admin-role
	// pattern, so one Go type per event kind covers all three emitters.
	EventOwnershipTransfer          = "ownership_transfer"           // 2-step ownership transfer initiated
	EventOwnershipTransferCompleted = "ownership_transfer_completed" // 2-step ownership transfer accepted
	EventAdminChanged               = "admin_changed"                // admin role reassigned (old_admin may be void — bootstrap)
	EventRemoteTokenMessengerAdded  = "remote_token_messenger_added" // TokenMessengerMinter: a remote-domain TokenMessenger registered
	EventTokenPairLinked            = "token_pair_linked"            // TokenMessengerMinter: local token linked to a remote-domain token

	// Lower-signal admin/governance events. All CCTP topics are Symbols, so topic_0_sym is never
	// empty.
	EventAdminChangeStarted        = "admin_change_started"          // 2-step admin change initiated (old_admin may be void — bootstrap)
	EventAttesterEnabled           = "attester_enabled"              // MessageTransmitter: an attester public key was enabled
	EventAttesterManagerUpdated    = "attester_manager_updated"      // MessageTransmitter: attester-manager role reassigned (old may be void — bootstrap)
	EventDenylisted                = "denylisted"                    // TokenMessengerMinter: an account entered the denylist
	EventDenylisterChanged         = "denylister_changed"            // TokenMessengerMinter: denylister role reassigned (old may be void — bootstrap)
	EventFeeRecipientSet           = "fee_recipient_set"             // TokenMessengerMinter: fee-recipient address changed
	EventMaxMessageBodySizeUpdated = "max_message_body_size_updated" // MessageTransmitter: message size ceiling changed
	EventMinFeeControllerSet       = "min_fee_controller_set"        // TokenMessengerMinter: min-fee-controller role reassigned
	EventPauserChanged             = "pauser_changed"                // pause-role address reassigned
	EventRescuerChanged            = "rescuer_changed"               // rescue-role address reassigned
	EventSetBurnLimitPerMessage    = "set_burn_limit_per_message"    // TokenMessengerMinter: per-message burn ceiling set for a local token
	EventSetTokenController        = "set_token_controller"          // TokenMessengerMinter: token-controller role reassigned
	EventSignatureThresholdUpdated = "signature_threshold_updated"   // MessageTransmitter: attestation signature threshold changed
	EventSwapMinterConfigSet       = "swap_minter_config_set"        // TokenMessengerMinter: swap-minter config set for a local token
	EventTokenDecimalConfigAdded   = "token_decimal_config_added"    // TokenMessengerMinter: canonical/local decimal mapping added for a local token
	EventUnDenylisted              = "un_denylisted"                 // TokenMessengerMinter: an account left the denylist
)

// Topic[0] pre-encoded base64, so Classify() does string-equal comparisons rather than a full SCVal
// decode per event. The wire shape is ScSymbol whichever Soroban macro produced it.
var (
	TopicSymbolDepositForBurn             = scval.MustEncodeSymbol(EventDepositForBurn)
	TopicSymbolMintAndWithdraw            = scval.MustEncodeSymbol(EventMintAndWithdraw)
	TopicSymbolMessageSent                = scval.MustEncodeSymbol(EventMessageSent)
	TopicSymbolMessageReceived            = scval.MustEncodeSymbol(EventMessageReceived)
	TopicSymbolMintAndForward             = scval.MustEncodeSymbol(EventMintAndForward)
	TopicSymbolOwnershipTransfer          = scval.MustEncodeSymbol(EventOwnershipTransfer)
	TopicSymbolOwnershipTransferCompleted = scval.MustEncodeSymbol(EventOwnershipTransferCompleted)
	TopicSymbolAdminChanged               = scval.MustEncodeSymbol(EventAdminChanged)
	TopicSymbolRemoteTokenMessengerAdded  = scval.MustEncodeSymbol(EventRemoteTokenMessengerAdded)
	TopicSymbolTokenPairLinked            = scval.MustEncodeSymbol(EventTokenPairLinked)

	TopicSymbolAdminChangeStarted        = scval.MustEncodeSymbol(EventAdminChangeStarted)
	TopicSymbolAttesterEnabled           = scval.MustEncodeSymbol(EventAttesterEnabled)
	TopicSymbolAttesterManagerUpdated    = scval.MustEncodeSymbol(EventAttesterManagerUpdated)
	TopicSymbolDenylisted                = scval.MustEncodeSymbol(EventDenylisted)
	TopicSymbolDenylisterChanged         = scval.MustEncodeSymbol(EventDenylisterChanged)
	TopicSymbolFeeRecipientSet           = scval.MustEncodeSymbol(EventFeeRecipientSet)
	TopicSymbolMaxMessageBodySizeUpdated = scval.MustEncodeSymbol(EventMaxMessageBodySizeUpdated)
	TopicSymbolMinFeeControllerSet       = scval.MustEncodeSymbol(EventMinFeeControllerSet)
	TopicSymbolPauserChanged             = scval.MustEncodeSymbol(EventPauserChanged)
	TopicSymbolRescuerChanged            = scval.MustEncodeSymbol(EventRescuerChanged)
	TopicSymbolSetBurnLimitPerMessage    = scval.MustEncodeSymbol(EventSetBurnLimitPerMessage)
	TopicSymbolSetTokenController        = scval.MustEncodeSymbol(EventSetTokenController)
	TopicSymbolSignatureThresholdUpdated = scval.MustEncodeSymbol(EventSignatureThresholdUpdated)
	TopicSymbolSwapMinterConfigSet       = scval.MustEncodeSymbol(EventSwapMinterConfigSet)
	TopicSymbolTokenDecimalConfigAdded   = scval.MustEncodeSymbol(EventTokenDecimalConfigAdded)
	TopicSymbolUnDenylisted              = scval.MustEncodeSymbol(EventUnDenylisted)
)

// DepositForBurn is the canonical projection of one `deposit_for_burn` event
// from TokenMessengerMinter (v2); schema in docs/protocols/cctp.md.
//
// Wire: topics = ["deposit_for_burn", burn_token, depositor, min_finality_threshold]; body = ScMap
// { amount, mint_recipient, destination_domain, destination_token_messenger, destination_caller,
// max_fee, hook_data }.
//
// mint_recipient, destination_token_messenger and destination_caller are 32-byte buffers (EVM
// destinations: 12 zero-pad bytes, then the 20-byte address). They are kept as raw lowercase hex
// without 0x; downstream re-formats per destination chain.
type DepositForBurn struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string // RFC 3339
	ContractID string

	// Topics
	BurnToken            string // Stellar Address strkey
	Depositor            string // Stellar Address strkey
	MinFinalityThreshold uint32 // attestation finality requirement

	// Body
	Amount                    string // i128 canonical-decimals; see CCTP docs §canonical amounts
	MintRecipient             string // hex; BytesN<32>
	DestinationDomain         uint32 // CCTP domain ID (0=Ethereum, 1=Avalanche, ...)
	DestinationTokenMessenger string // hex; BytesN<32>
	DestinationCaller         string // hex; BytesN<32>; zero-hex = any-caller
	MaxFee                    string // i128 canonical-decimals
	HookData                  string // hex; opaque post-mint payload
}

// MintAndWithdraw is the canonical projection of one `mint_and_withdraw` event from
// TokenMessengerMinter (v2).
//
//	topics = ["mint_and_withdraw", mint_recipient, mint_token]
//	body   = ScMap { amount, fee_collected }
type MintAndWithdraw struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	MintRecipient string // Stellar Address strkey
	MintToken     string // Stellar Address strkey

	Amount       string // i128
	FeeCollected string // i128
}

// MintAndForward is the canonical projection of one `mint_and_forward` event: the CctpForwarder
// minting and relaying onward to the final recipient. Single Symbol topic; body map
// {amount: i128, forward_recipient: Address, token: Address}.
type MintAndForward struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	ForwardRecipient string // Stellar Address strkey
	Token            string // Stellar Address strkey (contract)
	Amount           string // i128
}

// OwnershipTransfer is the canonical projection of one `ownership_transfer` event. Ownable2Step:
// this fires when the current owner initiates a transfer; the new owner must accept before
// `ownership_transfer_completed` fires. `old_owner` is type-tested in case an upgrade emits it
// unset.
//
//	topics = ["ownership_transfer"]
//	body   = { live_until_ledger: u32, new_owner: Address, old_owner: Address }
type OwnershipTransfer struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	LiveUntilLedger uint32 // ledger after which the pending transfer expires
	NewOwner        string // Stellar Address strkey
	OldOwner        string // Stellar Address strkey; "" if void
}

// OwnershipTransferCompleted is the canonical projection of one `ownership_transfer_completed`
// event: the new owner accepted a pending [OwnershipTransfer]. `new_owner` is the only field,
// for both bootstrap and later transfers.
//
//	topics = ["ownership_transfer_completed"]
//	body   = { new_owner: Address }
type OwnershipTransferCompleted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewOwner string // Stellar Address strkey
}

// AdminChanged is the canonical projection of one `admin_changed` event: the contract's
// operational admin role (distinct from `owner`) was reassigned.
//
// At bootstrap `old_admin` is ScvVoid, later reassignments populate it, so the decoder type-tests
// `old_admin` rather than assuming an Address.
//
//	topics = ["admin_changed"]
//	body   = { new_admin: Address, old_admin: Address | Void }
type AdminChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAdmin string // Stellar Address strkey
	OldAdmin string // Stellar Address strkey; "" if void (bootstrap — no previous admin)
}

// RemoteTokenMessengerAdded is the canonical projection of one `remote_token_messenger_added`
// event: TokenMessengerMinter registering the counterpart TokenMessenger on another CCTP domain
// (one per supported remote domain).
//
//	topics = ["remote_token_messenger_added"]
//	body   = { domain: u32, token_messenger: BytesN<32> }
type RemoteTokenMessengerAdded struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Domain         uint32 // CCTP domain ID of the remote chain
	TokenMessenger string // hex; BytesN<32> — the remote TokenMessenger's identity on that domain
}

// TokenPairLinked is the canonical projection of one `token_pair_linked` event:
// TokenMessengerMinter registering which remote-domain token a local Stellar token burns/mints
// against. Pairs 1:1 with [RemoteTokenMessengerAdded] by domain.
//
//	topics = ["token_pair_linked"]
//	body   = { local_token: Address, remote_domain: u32, remote_token: BytesN<32> }
type TokenPairLinked struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	LocalToken   string // Stellar Address strkey (contract) — the local SAC
	RemoteDomain uint32 // CCTP domain ID of the remote chain
	RemoteToken  string // hex; BytesN<32> — the remote chain's token identity
}

// MessageSent is the canonical projection of one `message_sent` event from MessageTransmitter
// (v2), emitted alongside `deposit_for_burn` for every outbound transfer.
//
//	topics = ["message_sent"]   (single-topic event)
//	body   = Bytes (raw)
//
// The bytes are the cross-chain envelope destination-chain attestation services consume; kept as hex.
type MessageSent struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Message string // hex of the serialised envelope
}

// MessageReceived is the canonical projection of one `message_received` event from
// MessageTransmitter (v2), emitted alongside `mint_and_withdraw` for every inbound transfer.
//
//	topics = ["message_received", caller, nonce, finality_threshold_executed]
//	body   = ScMap { source_domain, sender, message_body }
type MessageReceived struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	// Topics
	Caller                    string // Stellar Address strkey
	Nonce                     string // hex; BytesN<32>
	FinalityThresholdExecuted uint32

	// Body
	SourceDomain uint32 // CCTP domain ID
	Sender       string // hex; BytesN<32>
	MessageBody  string // hex
}

// ─── Lower-signal admin/governance events ───────────────────────────
//
// All are rare; the schemas come from real lake events, as upstream documents almost none of them.

// AdminChangeStarted is the canonical projection of one `admin_change_started` event: fires when
// an admin change is initiated (`admin_changed` fires when it takes effect). `old_admin` is
// type-tested via [scval.AsAddressOrVoid] because it is void at bootstrap.
//
// Body: { new_admin: Address, old_admin: Address | Void }.
type AdminChangeStarted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAdmin string // Stellar Address strkey
	OldAdmin string // Stellar Address strkey; "" if void
}

// AttesterEnabled is the canonical projection of one `attester_enabled` event: MessageTransmitter
// enabling one attester's signing key. One event per attester, so several can share a tx.
// Two topics; body is an empty map.
//
//	topics = ["attester_enabled", attester]
//	body   = {}
type AttesterEnabled struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Attester string // hex; BytesN<20> — secp256k1-recovered attester address
}

// AttesterManagerUpdated is the canonical projection of one `attester_manager_updated` event: the
// attester-manager role was reassigned. `old_attester_manager` is ScvVoid at bootstrap, so it is
// type-tested via [scval.AsAddressOrVoid]; note the void is in a TOPIC field, not the body.
// Three topics; body is an empty map.
//
//	topics = ["attester_manager_updated", old_attester_manager, new_attester_manager]
//	body   = {}
type AttesterManagerUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	OldAttesterManager string // Stellar Address strkey; "" if void (bootstrap)
	NewAttesterManager string // Stellar Address strkey
}

// Denylisted is the canonical projection of one `denylisted` event: an account was added to
// TokenMessengerMinter's denylist. Two topics; body is an empty map.
//
//	topics = ["denylisted", account]
//	body   = {}
type Denylisted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Account string // Stellar Address strkey
}

// UnDenylisted is the canonical projection of one `un_denylisted` event: an account was removed
// from TokenMessengerMinter's denylist (the inverse of [Denylisted]). Two topics; body is an
// empty map.
//
//	topics = ["un_denylisted", account]
//	body   = {}
type UnDenylisted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Account string // Stellar Address strkey
}

// DenylisterChanged is the canonical projection of one `denylister_changed` event: the denylister
// role was reassigned. `old_denylister` is ScvVoid at bootstrap and type-tested via
// [scval.AsAddressOrVoid], as in [AttesterManagerUpdated]. Three topics; body is an empty map.
//
//	topics = ["denylister_changed", old_denylister, new_denylister]
//	body   = {}
type DenylisterChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	OldDenylister string // Stellar Address strkey; "" if void (bootstrap)
	NewDenylister string // Stellar Address strkey
}

// FeeRecipientSet is the canonical projection of one `fee_recipient_set` event: the address that
// receives collected fees changed. Single topic; body ScMap.
//
//	topics = ["fee_recipient_set"]
//	body   = { fee_recipient: Address }
type FeeRecipientSet struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	FeeRecipient string // Stellar Address strkey
}

// MaxMessageBodySizeUpdated is the canonical projection of one `max_message_body_size_updated`
// event: MessageTransmitter's message size ceiling changed. Single topic; body ScMap.
//
//	topics = ["max_message_body_size_updated"]
//	body   = { new_max_message_body_size: u32 }
type MaxMessageBodySizeUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewMaxMessageBodySize uint32
}

// MinFeeControllerSet is the canonical projection of one `min_fee_controller_set` event: the
// min-fee-controller role was reassigned. Two topics; body is an empty map.
//
//	topics = ["min_fee_controller_set", min_fee_controller]
//	body   = {}
type MinFeeControllerSet struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	MinFeeController string // Stellar Address strkey
}

// PauserChanged is the canonical projection of one `pauser_changed` event: the pause-role address
// was reassigned. Emitted by all three contracts. The body field is named `new_address`, not
// `new_pauser`. Single topic; body ScMap.
//
//	topics = ["pauser_changed"]
//	body   = { new_address: Address }
type PauserChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAddress string // Stellar Address strkey
}

// RescuerChanged is the canonical projection of one `rescuer_changed` event: the rescue-role
// address was reassigned. Emitted by all three contracts. Single topic; body ScMap.
//
//	topics = ["rescuer_changed"]
//	body   = { new_rescuer: Address }
type RescuerChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewRescuer string // Stellar Address strkey
}

// SetTokenController is the canonical projection of one `set_token_controller` event: the
// token-controller role was reassigned. Single topic; body ScMap.
//
//	topics = ["set_token_controller"]
//	body   = { token_controller: Address }
type SetTokenController struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	TokenController string // Stellar Address strkey
}

// SignatureThresholdUpdated is the canonical projection of one `signature_threshold_updated`
// event: MessageTransmitter's required attestation signature count changed. Single topic; body
// ScMap.
//
//	topics = ["signature_threshold_updated"]
//	body   = { new_signature_threshold: u32, old_signature_threshold: u32 }
type SignatureThresholdUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewSignatureThreshold uint32
	OldSignatureThreshold uint32
}

// SetBurnLimitPerMessage is the canonical projection of one `set_burn_limit_per_message` event:
// the per-message burn ceiling for one local token was set. `token` is a genuine Stellar Address
// strkey (the SAC of [TokenPairLinked]'s `local_token`), so it promotes to [Event.Token].
//
//	topics = ["set_burn_limit_per_message", token]
//	body   = { burn_limit_per_message: i128 }
type SetBurnLimitPerMessage struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Token               string // Stellar Address strkey (contract) — the local SAC
	BurnLimitPerMessage string // i128 canonical-decimals; a policy ceiling, kept out of Event.Amount
}

// SwapMinterConfigSet is the canonical projection of one `swap_minter_config_set` event: a
// swap-minter configuration was set for one local token. The body's `swap_minter_config` is a
// NESTED map, flattened here into two fields. Two topics; body ScMap.
//
//	topics = ["swap_minter_config_set", token]
//	body   = { swap_minter_config: { allow_asset: Address, swap_minter: Address } }
type SwapMinterConfigSet struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Token      string // Stellar Address strkey (contract) — the local SAC
	AllowAsset string // Stellar Address strkey
	SwapMinter string // Stellar Address strkey
}

// TokenDecimalConfigAdded is the canonical projection of one `token_decimal_config_added` event:
// a canonical/local decimal mapping was registered for one local token (CCTP canonical decimals
// need not match the chain's local decimals, e.g. canonical 6 vs local 7). The body's
// `token_decimal_config` is a nested map, flattened here into two fields.
//
//	topics = ["token_decimal_config_added", token]
//	body   = { token_decimal_config: { canonical_decimals: u32, local_decimals: u32 } }
type TokenDecimalConfigAdded struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Token             string // Stellar Address strkey (contract) — the local SAC
	CanonicalDecimals uint32
	LocalDecimals     uint32
}
