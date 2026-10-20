// Package cctp decodes Circle's CCTP v2 contract events on Stellar (Soroban) from
// TokenMessengerMinter, MessageTransmitter and CctpForwarder (Mainnet* constants below).
//
// An outbound deposit_for_burn emits both a DepositForBurn and a MessageSent in one transaction;
// inbound pairs MessageReceived with MintAndWithdraw. Correlate by (ledger, tx_hash). Per-event
// schemas: docs/protocols/cctp.md.
package cctp

import (
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this source.
const SourceName = "cctp"

// GenesisLedger is the ledger of the MessageTransmitter's first on-chain event, the lower bound for any re-derive or gap scan; ProtocolMeta.GenesisLedger for "cctp" must equal it.
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

// DepositForBurn is the canonical projection of one `deposit_for_burn` event from TokenMessengerMinter (v2); 32-byte address fields are kept as raw lowercase hex (schema in docs/protocols/cctp.md).
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

// MintAndWithdraw is the canonical projection of one `mint_and_withdraw` event from TokenMessengerMinter (v2).
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

// MintAndForward is the canonical projection of one `mint_and_forward` event: the CctpForwarder minting and relaying onward to the final recipient.
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

// OwnershipTransfer is the canonical projection of one `ownership_transfer` event.
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

// OwnershipTransferCompleted is the canonical projection of one `ownership_transfer_completed` event: the new owner accepted a pending [OwnershipTransfer].
type OwnershipTransferCompleted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewOwner string // Stellar Address strkey
}

// AdminChanged is the canonical projection of one `admin_changed` event: the contract's operational admin role (distinct from `owner`) was reassigned.
type AdminChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAdmin string // Stellar Address strkey
	OldAdmin string // Stellar Address strkey; "" if void (bootstrap — no previous admin)
}

// RemoteTokenMessengerAdded is the canonical projection of one `remote_token_messenger_added` event: TokenMessengerMinter registering the counterpart TokenMessenger on another CCTP domain (one per supported remote domain).
type RemoteTokenMessengerAdded struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Domain         uint32 // CCTP domain ID of the remote chain
	TokenMessenger string // hex; BytesN<32> — the remote TokenMessenger's identity on that domain
}

// TokenPairLinked is the canonical projection of one `token_pair_linked` event: TokenMessengerMinter registering which remote-domain token a local Stellar token burns/mints against.
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

// MessageSent is the canonical projection of one `message_sent` event from MessageTransmitter (v2), emitted alongside `deposit_for_burn` for every outbound transfer.
type MessageSent struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Message string // hex of the serialised envelope
}

// MessageReceived is the canonical projection of one `message_received` event from MessageTransmitter (v2), emitted alongside `mint_and_withdraw` for every inbound transfer.
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

// AdminChangeStarted is the canonical projection of one `admin_change_started` event: fires when an admin change is initiated (`admin_changed` fires when it takes effect).
type AdminChangeStarted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAdmin string // Stellar Address strkey
	OldAdmin string // Stellar Address strkey; "" if void
}

// AttesterEnabled is the canonical projection of one `attester_enabled` event: MessageTransmitter enabling one attester's signing key.
type AttesterEnabled struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Attester string // hex; BytesN<20> — secp256k1-recovered attester address
}

// AttesterManagerUpdated is the canonical projection of one `attester_manager_updated` event: the attester-manager role was reassigned; old_attester_manager is Void at bootstrap and is type-tested via [scval.AsAddressOrVoid].
type AttesterManagerUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	OldAttesterManager string // Stellar Address strkey; "" if void (bootstrap)
	NewAttesterManager string // Stellar Address strkey
}

// Denylisted is the canonical projection of one `denylisted` event: an account was added to TokenMessengerMinter's denylist.
type Denylisted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Account string // Stellar Address strkey
}

// UnDenylisted is the canonical projection of one `un_denylisted` event: an account was removed from TokenMessengerMinter's denylist (the inverse of [Denylisted]).
type UnDenylisted struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Account string // Stellar Address strkey
}

// DenylisterChanged is the canonical projection of one `denylister_changed` event: the denylister role was reassigned.
type DenylisterChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	OldDenylister string // Stellar Address strkey; "" if void (bootstrap)
	NewDenylister string // Stellar Address strkey
}

// FeeRecipientSet is the canonical projection of one `fee_recipient_set` event: the address that receives collected fees changed.
type FeeRecipientSet struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	FeeRecipient string // Stellar Address strkey
}

// MaxMessageBodySizeUpdated is the canonical projection of one `max_message_body_size_updated` event: MessageTransmitter's message size ceiling changed.
type MaxMessageBodySizeUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewMaxMessageBodySize uint32
}

// MinFeeControllerSet is the canonical projection of one `min_fee_controller_set` event: the min-fee-controller role was reassigned.
type MinFeeControllerSet struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	MinFeeController string // Stellar Address strkey
}

// PauserChanged is the canonical projection of one `pauser_changed` event: the pause-role address was reassigned; the body field is `new_address`, not `new_pauser`.
type PauserChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewAddress string // Stellar Address strkey
}

// RescuerChanged is the canonical projection of one `rescuer_changed` event: the rescue-role address was reassigned.
type RescuerChanged struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewRescuer string // Stellar Address strkey
}

// SetTokenController is the canonical projection of one `set_token_controller` event: the token-controller role was reassigned.
type SetTokenController struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	TokenController string // Stellar Address strkey
}

// SignatureThresholdUpdated is the canonical projection of one `signature_threshold_updated` event: MessageTransmitter's required attestation signature count changed.
type SignatureThresholdUpdated struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	NewSignatureThreshold uint32
	OldSignatureThreshold uint32
}

// SetBurnLimitPerMessage is the canonical projection of one `set_burn_limit_per_message` event: the per-message burn ceiling for one local token was set.
type SetBurnLimitPerMessage struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Token               string // Stellar Address strkey (contract) — the local SAC
	BurnLimitPerMessage string // i128 canonical-decimals; a policy ceiling, kept out of Event.Amount
}

// SwapMinterConfigSet is the canonical projection of one `swap_minter_config_set` event: a swap-minter configuration was set for one local token.
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

// TokenDecimalConfigAdded is the canonical projection of one `token_decimal_config_added` event: a canonical/local decimal mapping was registered for one local token (CCTP canonical decimals need not match the chain's local decimals, e.g. canonical 6 vs local 7).
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
