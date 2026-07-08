// Package cctp decodes Circle's CCTP v2 contract events on
// Stellar (Soroban).
//
// Three on-chain contracts:
//
//	TokenMessengerMinter  CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL
//	MessageTransmitter    CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV
//	CctpForwarder         CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T
//
// Transfer-flow events:
//
//	deposit_for_burn   (TokenMessengerMinter) — outbound transfer
//	mint_and_withdraw  (TokenMessengerMinter) — inbound mint
//	message_sent       (MessageTransmitter)   — wire envelope (outbound)
//	message_received   (MessageTransmitter)   — wire envelope (inbound)
//	mint_and_forward   (CctpForwarder)        — inbound mint relayed onward
//
// Governance/admin events (all three contracts; verified against
// real mainnet events 2026-07-08, ROADMAP #89b topic-match audit):
//
//	ownership_transfer            — 2-step ownership transfer initiated
//	ownership_transfer_completed  — 2-step ownership transfer accepted
//	admin_changed                 — admin role reassigned
//	remote_token_messenger_added  (TokenMessengerMinter only) — remote-domain TokenMessenger registered
//	token_pair_linked             (TokenMessengerMinter only) — local↔remote token link registered
//
// One outbound `deposit_for_burn` call emits BOTH a DepositForBurn
// event AND a MessageSent event in the same transaction —
// correlate by (ledger, tx_hash) when assembling a logical
// outbound-transfer record. Same for inbound (MessageReceived +
// MintAndWithdraw).
//
// Design rationale and full per-event schemas extracted from the
// contracts' Rust source: docs/architecture/cctp-stellar-coverage.md.
//
// Wiring (#40): decode.go decodes; consumer.go projects each event
// into the canonical cctp.Event row; dispatcher_adapter.go is the
// dispatcher Decoder; the indexer's sink persists via
// Store.InsertCCTPEvent into the cctp_events hypertable
// (migration 0038, per-protocol table — operator-confirmed
// 2026-05-22). See README.md §Wiring.
package cctp

import (
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// SourceName is the registry key for this source.
const SourceName = "cctp"

// Mainnet contract addresses — verified 2026-05-20 against
// https://developers.circle.com/cctp/references/stellar-contracts
// + the upstream source repo github.com/circlefin/stellar-cctp.
const (
	MainnetTokenMessengerMinter = "CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL"
	MainnetMessageTransmitter   = "CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV"
	MainnetCctpForwarder        = "CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T"
)

// StellarDomainID is Stellar's CCTP domain identifier
// (`message_transmitter::get_local_domain()` returns this value).
// Other notable CCTP domains: Ethereum=0, Avalanche=1, Arbitrum=3,
// Solana=7. Full list at Circle docs.
const StellarDomainID uint32 = 27

// Event names — the symbol_short / Symbol::new strings emitted as
// topic[0] by each #[contractevent]. Verified against
// contracts/{token-messenger-minter-v2,message-transmitter-v2}/src/lib.rs.
const (
	EventDepositForBurn  = "deposit_for_burn"  // TokenMessengerMinter
	EventMintAndWithdraw = "mint_and_withdraw" // TokenMessengerMinter
	EventMessageSent     = "message_sent"      // MessageTransmitter
	EventMessageReceived = "message_received"  // MessageTransmitter
	EventMintAndForward  = "mint_and_forward"  // CctpForwarder — mint relayed onward to the recipient

	// Governance/admin events — verified against real mainnet lake
	// events (2026-07-08, ROADMAP #89b topic-match audit) across all
	// three CCTP contracts. Every contract implements the same
	// Ownable2Step + admin-role pattern, so a single Go type per
	// event kind covers all three emitters (matching mint_and_forward's
	// precedent of one struct regardless of which contract fires it).
	EventOwnershipTransfer          = "ownership_transfer"           // 2-step ownership transfer initiated
	EventOwnershipTransferCompleted = "ownership_transfer_completed" // 2-step ownership transfer accepted
	EventAdminChanged               = "admin_changed"                // admin role reassigned (old_admin may be void — bootstrap)
	EventRemoteTokenMessengerAdded  = "remote_token_messenger_added" // TokenMessengerMinter: a remote-domain TokenMessenger registered
	EventTokenPairLinked            = "token_pair_linked"            // TokenMessengerMinter: local token linked to a remote-domain token
)

// Topic[0] pre-encoded base64 — package-init constants so
// Classify() does single string-equal comparisons rather than
// full SCVal decodes per event. All four are >= 12 chars (a
// `deposit_for_burn` is 16) so the Soroban macro emits them as
// long-form ScSymbol via `Symbol::new(env, …)`, not the
// 9-char-capped `symbol_short!`. The wire shape is still ScSymbol
// in both cases; the macro picks the constructor by length.
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
)

// DepositForBurn is the canonical projection of one
// `DepositForBurn` event from TokenMessengerMinter (v2).
//
// Source schema (token-messenger-minter-v2/src/lib.rs:#[contractevent]):
//
//	pub struct DepositForBurn {
//	    #[topic] pub burn_token: Address,
//	    pub amount: i128,
//	    #[topic] pub depositor: Address,
//	    pub mint_recipient: BytesN<32>,
//	    pub destination_domain: u32,
//	    pub destination_token_messenger: BytesN<32>,
//	    pub destination_caller: BytesN<32>,
//	    pub max_fee: i128,
//	    #[topic] pub min_finality_threshold: u32,
//	    pub hook_data: Bytes,
//	}
//
// On the wire:
//
//	topics = ["deposit_for_burn", burn_token, depositor, min_finality_threshold]
//	body   = ScMap { amount, mint_recipient, destination_domain,
//	                 destination_token_messenger, destination_caller,
//	                 max_fee, hook_data }
//
// `mint_recipient` / `destination_token_messenger` /
// `destination_caller` are 32-byte buffers — for EVM destination
// chains the leading 12 bytes are zero padding and the trailing
// 20 bytes are the EVM address. We surface them as raw hex
// (lowercase, no 0x prefix) — downstream decides whether to
// re-format for a specific destination chain.
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

// MintAndWithdraw is the canonical projection of one
// `MintAndWithdraw` event from TokenMessengerMinter (v2).
//
// Source schema:
//
//	pub struct MintAndWithdraw {
//	    #[topic] pub mint_recipient: Address,
//	    pub amount: i128,
//	    #[topic] pub mint_token: Address,
//	    pub fee_collected: i128,
//	}
//
// Wire shape:
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

// MintAndForward is the canonical projection of one
// `mint_and_forward` event — the CctpForwarder minting and relaying
// onward to the final recipient. Discovered undecoded in the lake
// 2026-07-02 (board #31); schema reverse-engineered from real
// mainnet events (single Symbol topic; body map
// {amount: i128, forward_recipient: Address, token: Address}).
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

// OwnershipTransfer is the canonical projection of one
// `ownership_transfer` event. All three CCTP contracts implement an
// OpenZeppelin-style Ownable2Step admin pattern: this event fires
// when the CURRENT owner initiates a transfer; the new owner must
// separately accept before `ownership_transfer_completed` fires.
//
// Verified against real mainnet events (2026-07-08): ledgers
// 62211157 (TokenMessengerMinter), 62211185 (MessageTransmitter),
// 62211209 (CctpForwarder) — one per contract, `old_owner` populated
// in all three observed instances. No genesis-time void case has
// been seen on mainnet, but the decoder still type-tests `old_owner`
// (contract-schema-evolution stance, CLAUDE.md "Type-test before
// MustI128") in case a future upgrade emits it from an unset state.
//
// Wire shape (single-topic event; body ScMap):
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

// OwnershipTransferCompleted is the canonical projection of one
// `ownership_transfer_completed` event — the new owner accepted a
// pending [OwnershipTransfer].
//
// Verified against real mainnet events (2026-07-08): ledgers
// 62146641 (MessageTransmitter), 62146653 (TokenMessengerMinter),
// 62146669 (CctpForwarder) carry the bootstrap acceptance; ledgers
// 62225090/62225171/62225185 carry the later real transfer's
// acceptance. `new_owner` is the only field in both cases.
//
// Wire shape:
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

// AdminChanged is the canonical projection of one `admin_changed`
// event — the contract's operational admin role (distinct from
// `owner`) was reassigned.
//
// Verified against real mainnet events (2026-07-08): each contract
// emits this TWICE — once at bootstrap (`old_admin` is
// `ScValTypeScvVoid`, e.g. ledger 62146641/62146653/62146669) and
// once for the later real reassignment (`old_admin` populated, e.g.
// ledger 62225106/62225178/62225207). The decoder type-tests
// `old_admin` rather than assuming it's always an Address.
//
// Wire shape:
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

// RemoteTokenMessengerAdded is the canonical projection of one
// `remote_token_messenger_added` event — TokenMessengerMinter
// registering the counterpart TokenMessenger contract on another
// CCTP domain. Only ever observed from TokenMessengerMinter (26
// occurrences on mainnet as of 2026-07-08, one per supported remote
// domain, ledgers 62146653-63149586).
//
// Wire shape:
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

// TokenPairLinked is the canonical projection of one
// `token_pair_linked` event — TokenMessengerMinter registering which
// remote-domain token a local Stellar token burns/mints against.
// Only ever observed from TokenMessengerMinter (26 occurrences on
// mainnet as of 2026-07-08, paired 1:1 with
// [RemoteTokenMessengerAdded] by domain, ledgers 62146739-63149585).
//
// Wire shape:
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

// MessageSent is the canonical projection of one `MessageSent`
// event from MessageTransmitter (v2). Emitted alongside
// `DepositForBurn` for every outbound transfer.
//
// Source schema:
//
//	pub struct MessageSent {
//	    pub message: Bytes,
//	}
//
// Wire shape:
//
//	topics = ["message_sent"]   (single-topic event)
//	body   = Bytes (raw)
//
// The `message` bytes are the serialised cross-chain envelope —
// destination chain attestation services consume this; we
// preserve it as hex for cross-reference.
type MessageSent struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Message string // hex of the serialised envelope
}

// MessageReceived is the canonical projection of one
// `MessageReceived` event from MessageTransmitter (v2). Emitted
// alongside `MintAndWithdraw` for every inbound transfer.
//
// Source schema:
//
//	pub struct MessageReceived {
//	    #[topic] pub caller: Address,
//	    pub source_domain: u32,
//	    #[topic] pub nonce: BytesN<32>,
//	    pub sender: BytesN<32>,
//	    #[topic] pub finality_threshold_executed: u32,
//	    pub message_body: Bytes,
//	}
//
// Wire shape:
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
