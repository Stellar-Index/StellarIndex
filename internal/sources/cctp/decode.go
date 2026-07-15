package cctp

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ErrUnknownEvent flags an event whose topic[0] symbol isn't one
// of CCTP's four. Per CLAUDE.md ("Comet uses a shared topic")
// the dispatcher matches by topic bytes; another protocol could
// in theory emit the same symbol. Consumer should drop by
// ContractID before invoking the decoder.
var ErrUnknownEvent = errors.New("cctp: unknown event topic")

// ErrMalformedBody surfaces a schema drift — body Map missing a
// field the contract source declares, or a field with the wrong
// SCVal kind. Per CLAUDE.md ("Soroban DeFi contracts upgrade in
// place"), a contract upgrade could change field shapes; this
// error is the canary.
var ErrMalformedBody = errors.New("cctp: malformed event body")

// ErrMalformedTopic flags a topic slice shorter than the event
// type requires.
var ErrMalformedTopic = errors.New("cctp: malformed event topics")

// Classify reports which CCTP event the given Event is, or empty
// string if topic[0] doesn't match. Contract-ID filtering happens
// DOWNSTREAM.
func Classify(e *events.Event) string { //nolint:gocyclo,gocognit,cyclop // one case per CCTP topic (26, the full mainnet census — docs/protocols/cctp.md); flattening keeps the dispatch table auditable against the contract source
	if len(e.Topic) < 1 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolDepositForBurn:
		return EventDepositForBurn
	case TopicSymbolMintAndWithdraw:
		return EventMintAndWithdraw
	case TopicSymbolMessageSent:
		return EventMessageSent
	case TopicSymbolMessageReceived:
		return EventMessageReceived
	case TopicSymbolMintAndForward:
		return EventMintAndForward
	case TopicSymbolOwnershipTransfer:
		return EventOwnershipTransfer
	case TopicSymbolOwnershipTransferCompleted:
		return EventOwnershipTransferCompleted
	case TopicSymbolAdminChanged:
		return EventAdminChanged
	case TopicSymbolRemoteTokenMessengerAdded:
		return EventRemoteTokenMessengerAdded
	case TopicSymbolTokenPairLinked:
		return EventTokenPairLinked
	case TopicSymbolAdminChangeStarted:
		return EventAdminChangeStarted
	case TopicSymbolAttesterEnabled:
		return EventAttesterEnabled
	case TopicSymbolAttesterManagerUpdated:
		return EventAttesterManagerUpdated
	case TopicSymbolDenylisted:
		return EventDenylisted
	case TopicSymbolDenylisterChanged:
		return EventDenylisterChanged
	case TopicSymbolFeeRecipientSet:
		return EventFeeRecipientSet
	case TopicSymbolMaxMessageBodySizeUpdated:
		return EventMaxMessageBodySizeUpdated
	case TopicSymbolMinFeeControllerSet:
		return EventMinFeeControllerSet
	case TopicSymbolPauserChanged:
		return EventPauserChanged
	case TopicSymbolRescuerChanged:
		return EventRescuerChanged
	case TopicSymbolSetBurnLimitPerMessage:
		return EventSetBurnLimitPerMessage
	case TopicSymbolSetTokenController:
		return EventSetTokenController
	case TopicSymbolSignatureThresholdUpdated:
		return EventSignatureThresholdUpdated
	case TopicSymbolSwapMinterConfigSet:
		return EventSwapMinterConfigSet
	case TopicSymbolTokenDecimalConfigAdded:
		return EventTokenDecimalConfigAdded
	case TopicSymbolUnDenylisted:
		return EventUnDenylisted
	}
	return ""
}

// DecodeDepositForBurn turns one DepositForBurn-shaped Event into
// a canonical DepositForBurn value.
//
// Topic layout:
//
//	topic[0] = Symbol("deposit_for_burn")
//	topic[1] = Address(burn_token)
//	topic[2] = Address(depositor)
//	topic[3] = U32(min_finality_threshold)
//
// Body (ScMap, alphabetical-by-key):
//
//	{ amount: i128, destination_caller: BytesN<32>,
//	  destination_domain: u32, destination_token_messenger: BytesN<32>,
//	  hook_data: Bytes, max_fee: i128, mint_recipient: BytesN<32> }
//
// Per ADR-0013 we use scval helpers; the inferred-type entries
// slice keeps xdr out of this file's import list.
func DecodeDepositForBurn(e *events.Event) (DepositForBurn, error) { //nolint:gocognit,funlen,gocyclo // straight-line field decode; splitting fans the per-field error context across helpers
	if len(e.Topic) < 4 {
		return DepositForBurn{}, fmt.Errorf("%w: deposit_for_burn needs 4 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}

	burnTokenSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn topic[1] parse: %w", err)
	}
	burnToken, err := scval.AsAddressStrkey(burnTokenSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn burn_token: %w", err)
	}

	depositorSV, err := scval.Parse(e.Topic[2])
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn topic[2] parse: %w", err)
	}
	depositor, err := scval.AsAddressStrkey(depositorSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn depositor: %w", err)
	}

	minFinSV, err := scval.Parse(e.Topic[3])
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn topic[3] parse: %w", err)
	}
	minFinalityThreshold, err := scval.AsU32(minFinSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn min_finality_threshold: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn body not a map: %w", err)
	}

	amountSV, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'amount': %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn amount: %w", err)
	}

	mintRecipientSV, err := scval.MustMapField(entries, "mint_recipient")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'mint_recipient': %w", ErrMalformedBody, err)
	}
	mintRecipientBytes, err := scval.AsBytes(mintRecipientSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn mint_recipient: %w", err)
	}

	destDomainSV, err := scval.MustMapField(entries, "destination_domain")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'destination_domain': %w", ErrMalformedBody, err)
	}
	destinationDomain, err := scval.AsU32(destDomainSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn destination_domain: %w", err)
	}

	destTokenMsgSV, err := scval.MustMapField(entries, "destination_token_messenger")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'destination_token_messenger': %w", ErrMalformedBody, err)
	}
	destinationTokenMessenger, err := scval.AsBytes(destTokenMsgSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn destination_token_messenger: %w", err)
	}

	destCallerSV, err := scval.MustMapField(entries, "destination_caller")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'destination_caller': %w", ErrMalformedBody, err)
	}
	destinationCaller, err := scval.AsBytes(destCallerSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn destination_caller: %w", err)
	}

	maxFeeSV, err := scval.MustMapField(entries, "max_fee")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'max_fee': %w", ErrMalformedBody, err)
	}
	maxFee, err := scval.AsAmountFromI128(maxFeeSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn max_fee: %w", err)
	}

	hookDataSV, err := scval.MustMapField(entries, "hook_data")
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("%w: missing 'hook_data': %w", ErrMalformedBody, err)
	}
	hookData, err := scval.AsBytes(hookDataSV)
	if err != nil {
		return DepositForBurn{}, fmt.Errorf("cctp: deposit_for_burn hook_data: %w", err)
	}

	return DepositForBurn{
		Ledger:                    e.Ledger,
		TxHash:                    e.TxHash,
		OpIndex:                   e.OperationIndex,
		ClosedAt:                  e.LedgerClosedAt,
		ContractID:                e.ContractID,
		BurnToken:                 burnToken,
		Depositor:                 depositor,
		MinFinalityThreshold:      minFinalityThreshold,
		Amount:                    amount.String(),
		MintRecipient:             hex.EncodeToString(mintRecipientBytes),
		DestinationDomain:         destinationDomain,
		DestinationTokenMessenger: hex.EncodeToString(destinationTokenMessenger),
		DestinationCaller:         hex.EncodeToString(destinationCaller),
		MaxFee:                    maxFee.String(),
		HookData:                  hex.EncodeToString(hookData),
	}, nil
}

// DecodeMintAndWithdraw turns one MintAndWithdraw event into the
// canonical struct.
//
// Topic layout:
//
//	topic[0] = Symbol("mint_and_withdraw")
//	topic[1] = Address(mint_recipient)
//	topic[2] = Address(mint_token)
//
// Body: { amount: i128, fee_collected: i128 }.
func DecodeMintAndWithdraw(e *events.Event) (MintAndWithdraw, error) {
	if len(e.Topic) < 3 {
		return MintAndWithdraw{}, fmt.Errorf("%w: mint_and_withdraw needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}

	mintRecipientSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw topic[1] parse: %w", err)
	}
	mintRecipient, err := scval.AsAddressStrkey(mintRecipientSV)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw mint_recipient: %w", err)
	}

	mintTokenSV, err := scval.Parse(e.Topic[2])
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw topic[2] parse: %w", err)
	}
	mintToken, err := scval.AsAddressStrkey(mintTokenSV)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw mint_token: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw body not a map: %w", err)
	}

	amountSV, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("%w: missing 'amount': %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw amount: %w", err)
	}

	feeSV, err := scval.MustMapField(entries, "fee_collected")
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("%w: missing 'fee_collected': %w", ErrMalformedBody, err)
	}
	feeCollected, err := scval.AsAmountFromI128(feeSV)
	if err != nil {
		return MintAndWithdraw{}, fmt.Errorf("cctp: mint_and_withdraw fee_collected: %w", err)
	}

	return MintAndWithdraw{
		Ledger:        e.Ledger,
		TxHash:        e.TxHash,
		OpIndex:       e.OperationIndex,
		ClosedAt:      e.LedgerClosedAt,
		ContractID:    e.ContractID,
		MintRecipient: mintRecipient,
		MintToken:     mintToken,
		Amount:        amount.String(),
		FeeCollected:  feeCollected.String(),
	}, nil
}

// DecodeMessageSent turns one MessageSent event into the canonical
// struct. Topic-only event (one topic, body carries the message).
//
// Wire shape:
//
//	topic[0] = Symbol("message_sent")
//	value    = ScMap { message: Bytes }   (or raw ScBytes if the
//	                                       macro layout shifts)
func DecodeMessageSent(e *events.Event) (MessageSent, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return MessageSent{}, fmt.Errorf("cctp: message_sent body parse: %w", err)
	}
	// Map-body path: #[contractevent] wraps the single field in a
	// named-Map. Fall through to raw Bytes if the macro shifts.
	if entries, mapErr := scval.AsMap(body); mapErr == nil {
		msgSV, err := scval.MustMapField(entries, "message")
		if err != nil {
			return MessageSent{}, fmt.Errorf("%w: missing 'message': %w", ErrMalformedBody, err)
		}
		msg, err := scval.AsBytes(msgSV)
		if err != nil {
			return MessageSent{}, fmt.Errorf("cctp: message_sent message: %w", err)
		}
		return MessageSent{
			Ledger:     e.Ledger,
			TxHash:     e.TxHash,
			OpIndex:    e.OperationIndex,
			ClosedAt:   e.LedgerClosedAt,
			ContractID: e.ContractID,
			Message:    hex.EncodeToString(msg),
		}, nil
	}
	// Raw-Bytes fallback.
	raw, err := scval.AsBytes(body)
	if err != nil {
		return MessageSent{}, fmt.Errorf("cctp: message_sent body neither map nor bytes: %w", err)
	}
	return MessageSent{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		Message:    hex.EncodeToString(raw),
	}, nil
}

// DecodeMessageReceived turns one MessageReceived event into the
// canonical struct.
//
// Topic layout:
//
//	topic[0] = Symbol("message_received")
//	topic[1] = Address(caller)
//	topic[2] = BytesN<32>(nonce)
//	topic[3] = U32(finality_threshold_executed)
//
// Body: { source_domain: u32, sender: BytesN<32>, message_body: Bytes }.
func DecodeMessageReceived(e *events.Event) (MessageReceived, error) {
	if len(e.Topic) < 4 {
		return MessageReceived{}, fmt.Errorf("%w: message_received needs 4 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}

	callerSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received topic[1] parse: %w", err)
	}
	caller, err := scval.AsAddressStrkey(callerSV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received caller: %w", err)
	}

	nonceSV, err := scval.Parse(e.Topic[2])
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received topic[2] parse: %w", err)
	}
	nonceBytes, err := scval.AsBytes(nonceSV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received nonce: %w", err)
	}

	finExecSV, err := scval.Parse(e.Topic[3])
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received topic[3] parse: %w", err)
	}
	finalityExecuted, err := scval.AsU32(finExecSV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received finality_threshold_executed: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received body not a map: %w", err)
	}

	sourceDomainSV, err := scval.MustMapField(entries, "source_domain")
	if err != nil {
		return MessageReceived{}, fmt.Errorf("%w: missing 'source_domain': %w", ErrMalformedBody, err)
	}
	sourceDomain, err := scval.AsU32(sourceDomainSV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received source_domain: %w", err)
	}

	senderSV, err := scval.MustMapField(entries, "sender")
	if err != nil {
		return MessageReceived{}, fmt.Errorf("%w: missing 'sender': %w", ErrMalformedBody, err)
	}
	senderBytes, err := scval.AsBytes(senderSV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received sender: %w", err)
	}

	messageBodySV, err := scval.MustMapField(entries, "message_body")
	if err != nil {
		return MessageReceived{}, fmt.Errorf("%w: missing 'message_body': %w", ErrMalformedBody, err)
	}
	messageBodyBytes, err := scval.AsBytes(messageBodySV)
	if err != nil {
		return MessageReceived{}, fmt.Errorf("cctp: message_received message_body: %w", err)
	}

	return MessageReceived{
		Ledger:                    e.Ledger,
		TxHash:                    e.TxHash,
		OpIndex:                   e.OperationIndex,
		ClosedAt:                  e.LedgerClosedAt,
		ContractID:                e.ContractID,
		Caller:                    caller,
		Nonce:                     hex.EncodeToString(nonceBytes),
		FinalityThresholdExecuted: finalityExecuted,
		SourceDomain:              sourceDomain,
		Sender:                    hex.EncodeToString(senderBytes),
		MessageBody:               hex.EncodeToString(messageBodyBytes),
	}, nil
}

// DecodeMintAndForward turns one mint_and_forward event into the
// canonical struct.
//
// Topic layout (verified against mainnet ledger 63098002):
//
//	topic[0] = Symbol("mint_and_forward")   — the ONLY topic
//
// Body: { amount: i128, forward_recipient: Address, token: Address }.
func DecodeMintAndForward(e *events.Event) (MintAndForward, error) {
	if len(e.Topic) < 1 {
		return MintAndForward{}, fmt.Errorf("%w: mint_and_forward needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return MintAndForward{}, fmt.Errorf("cctp: mint_and_forward body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return MintAndForward{}, fmt.Errorf("cctp: mint_and_forward body not a map: %w", err)
	}
	amountSV, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return MintAndForward{}, fmt.Errorf("%w: missing 'amount': %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return MintAndForward{}, fmt.Errorf("cctp: mint_and_forward amount: %w", err)
	}
	recipSV, err := scval.MustMapField(entries, "forward_recipient")
	if err != nil {
		return MintAndForward{}, fmt.Errorf("%w: missing 'forward_recipient': %w", ErrMalformedBody, err)
	}
	recip, err := scval.AsAddressStrkey(recipSV)
	if err != nil {
		return MintAndForward{}, fmt.Errorf("cctp: mint_and_forward forward_recipient: %w", err)
	}
	tokenSV, err := scval.MustMapField(entries, "token")
	if err != nil {
		return MintAndForward{}, fmt.Errorf("%w: missing 'token': %w", ErrMalformedBody, err)
	}
	token, err := scval.AsAddressStrkey(tokenSV)
	if err != nil {
		return MintAndForward{}, fmt.Errorf("cctp: mint_and_forward token: %w", err)
	}
	return MintAndForward{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,

		ForwardRecipient: recip,
		Token:            token,
		Amount:           amount.String(),
	}, nil
}

// DecodeOwnershipTransfer turns one `ownership_transfer` event into
// the canonical struct. Single-topic event; body ScMap.
//
// Body: { live_until_ledger: u32, new_owner: Address, old_owner: Address }.
func DecodeOwnershipTransfer(e *events.Event) (OwnershipTransfer, error) {
	if len(e.Topic) < 1 {
		return OwnershipTransfer{}, fmt.Errorf("%w: ownership_transfer needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("cctp: ownership_transfer body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("cctp: ownership_transfer body not a map: %w", err)
	}

	liveUntilSV, err := scval.MustMapField(entries, "live_until_ledger")
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("%w: missing 'live_until_ledger': %w", ErrMalformedBody, err)
	}
	liveUntilLedger, err := scval.AsU32(liveUntilSV)
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("cctp: ownership_transfer live_until_ledger: %w", err)
	}

	newOwnerSV, err := scval.MustMapField(entries, "new_owner")
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("%w: missing 'new_owner': %w", ErrMalformedBody, err)
	}
	newOwner, err := scval.AsAddressStrkey(newOwnerSV)
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("cctp: ownership_transfer new_owner: %w", err)
	}

	oldOwnerSV, err := scval.MustMapField(entries, "old_owner")
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("%w: missing 'old_owner': %w", ErrMalformedBody, err)
	}
	oldOwner, err := scval.AsAddressOrVoid(oldOwnerSV)
	if err != nil {
		return OwnershipTransfer{}, fmt.Errorf("cctp: ownership_transfer old_owner: %w", err)
	}

	return OwnershipTransfer{
		Ledger:          e.Ledger,
		TxHash:          e.TxHash,
		OpIndex:         e.OperationIndex,
		ClosedAt:        e.LedgerClosedAt,
		ContractID:      e.ContractID,
		LiveUntilLedger: liveUntilLedger,
		NewOwner:        newOwner,
		OldOwner:        oldOwner,
	}, nil
}

// DecodeOwnershipTransferCompleted turns one
// `ownership_transfer_completed` event into the canonical struct.
// Single-topic event; body ScMap.
//
// Body: { new_owner: Address }.
func DecodeOwnershipTransferCompleted(e *events.Event) (OwnershipTransferCompleted, error) {
	if len(e.Topic) < 1 {
		return OwnershipTransferCompleted{}, fmt.Errorf("%w: ownership_transfer_completed needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return OwnershipTransferCompleted{}, fmt.Errorf("cctp: ownership_transfer_completed body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return OwnershipTransferCompleted{}, fmt.Errorf("cctp: ownership_transfer_completed body not a map: %w", err)
	}

	newOwnerSV, err := scval.MustMapField(entries, "new_owner")
	if err != nil {
		return OwnershipTransferCompleted{}, fmt.Errorf("%w: missing 'new_owner': %w", ErrMalformedBody, err)
	}
	newOwner, err := scval.AsAddressStrkey(newOwnerSV)
	if err != nil {
		return OwnershipTransferCompleted{}, fmt.Errorf("cctp: ownership_transfer_completed new_owner: %w", err)
	}

	return OwnershipTransferCompleted{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		NewOwner:   newOwner,
	}, nil
}

// DecodeAdminChanged turns one `admin_changed` event into the
// canonical struct. Single-topic event; body ScMap.
//
// Body: { new_admin: Address, old_admin: Address | Void }. Verified
// against real mainnet events (2026-07-08): the bootstrap instance
// of this event carries `old_admin = Void` — type-tested via
// [scval.AsAddressOrVoid] rather than assumed to always be an Address.
func DecodeAdminChanged(e *events.Event) (AdminChanged, error) {
	if len(e.Topic) < 1 {
		return AdminChanged{}, fmt.Errorf("%w: admin_changed needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminChanged{}, fmt.Errorf("cctp: admin_changed body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return AdminChanged{}, fmt.Errorf("cctp: admin_changed body not a map: %w", err)
	}

	newAdminSV, err := scval.MustMapField(entries, "new_admin")
	if err != nil {
		return AdminChanged{}, fmt.Errorf("%w: missing 'new_admin': %w", ErrMalformedBody, err)
	}
	newAdmin, err := scval.AsAddressStrkey(newAdminSV)
	if err != nil {
		return AdminChanged{}, fmt.Errorf("cctp: admin_changed new_admin: %w", err)
	}

	oldAdminSV, err := scval.MustMapField(entries, "old_admin")
	if err != nil {
		return AdminChanged{}, fmt.Errorf("%w: missing 'old_admin': %w", ErrMalformedBody, err)
	}
	oldAdmin, err := scval.AsAddressOrVoid(oldAdminSV)
	if err != nil {
		return AdminChanged{}, fmt.Errorf("cctp: admin_changed old_admin: %w", err)
	}

	return AdminChanged{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		NewAdmin:   newAdmin,
		OldAdmin:   oldAdmin,
	}, nil
}

// DecodeRemoteTokenMessengerAdded turns one
// `remote_token_messenger_added` event into the canonical struct.
// Single-topic event; body ScMap. TokenMessengerMinter only.
//
// Body: { domain: u32, token_messenger: BytesN<32> }.
func DecodeRemoteTokenMessengerAdded(e *events.Event) (RemoteTokenMessengerAdded, error) {
	if len(e.Topic) < 1 {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("%w: remote_token_messenger_added needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("cctp: remote_token_messenger_added body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("cctp: remote_token_messenger_added body not a map: %w", err)
	}

	domainSV, err := scval.MustMapField(entries, "domain")
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("%w: missing 'domain': %w", ErrMalformedBody, err)
	}
	domain, err := scval.AsU32(domainSV)
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("cctp: remote_token_messenger_added domain: %w", err)
	}

	tokenMessengerSV, err := scval.MustMapField(entries, "token_messenger")
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("%w: missing 'token_messenger': %w", ErrMalformedBody, err)
	}
	tokenMessenger, err := scval.AsBytes(tokenMessengerSV)
	if err != nil {
		return RemoteTokenMessengerAdded{}, fmt.Errorf("cctp: remote_token_messenger_added token_messenger: %w", err)
	}

	return RemoteTokenMessengerAdded{
		Ledger:         e.Ledger,
		TxHash:         e.TxHash,
		OpIndex:        e.OperationIndex,
		ClosedAt:       e.LedgerClosedAt,
		ContractID:     e.ContractID,
		Domain:         domain,
		TokenMessenger: hex.EncodeToString(tokenMessenger),
	}, nil
}

// DecodeTokenPairLinked turns one `token_pair_linked` event into the
// canonical struct. Single-topic event; body ScMap. TokenMessengerMinter
// only.
//
// Body: { local_token: Address, remote_domain: u32, remote_token: BytesN<32> }.
func DecodeTokenPairLinked(e *events.Event) (TokenPairLinked, error) {
	if len(e.Topic) < 1 {
		return TokenPairLinked{}, fmt.Errorf("%w: token_pair_linked needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("cctp: token_pair_linked body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("cctp: token_pair_linked body not a map: %w", err)
	}

	localTokenSV, err := scval.MustMapField(entries, "local_token")
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("%w: missing 'local_token': %w", ErrMalformedBody, err)
	}
	localToken, err := scval.AsAddressStrkey(localTokenSV)
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("cctp: token_pair_linked local_token: %w", err)
	}

	remoteDomainSV, err := scval.MustMapField(entries, "remote_domain")
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("%w: missing 'remote_domain': %w", ErrMalformedBody, err)
	}
	remoteDomain, err := scval.AsU32(remoteDomainSV)
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("cctp: token_pair_linked remote_domain: %w", err)
	}

	remoteTokenSV, err := scval.MustMapField(entries, "remote_token")
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("%w: missing 'remote_token': %w", ErrMalformedBody, err)
	}
	remoteToken, err := scval.AsBytes(remoteTokenSV)
	if err != nil {
		return TokenPairLinked{}, fmt.Errorf("cctp: token_pair_linked remote_token: %w", err)
	}

	return TokenPairLinked{
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      e.OperationIndex,
		ClosedAt:     e.LedgerClosedAt,
		ContractID:   e.ContractID,
		LocalToken:   localToken,
		RemoteDomain: remoteDomain,
		RemoteToken:  hex.EncodeToString(remoteToken),
	}, nil
}

// ─── Lower-signal admin/governance events (ROADMAP #89c, 2026-07-09) ──

// DecodeAdminChangeStarted turns one `admin_change_started` event into
// the canonical struct. Single-topic event; body ScMap.
//
// Body: { new_admin: Address, old_admin: Address | Void }.
func DecodeAdminChangeStarted(e *events.Event) (AdminChangeStarted, error) {
	if len(e.Topic) < 1 {
		return AdminChangeStarted{}, fmt.Errorf("%w: admin_change_started needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("cctp: admin_change_started body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("cctp: admin_change_started body not a map: %w", err)
	}

	newAdminSV, err := scval.MustMapField(entries, "new_admin")
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("%w: missing 'new_admin': %w", ErrMalformedBody, err)
	}
	newAdmin, err := scval.AsAddressStrkey(newAdminSV)
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("cctp: admin_change_started new_admin: %w", err)
	}

	oldAdminSV, err := scval.MustMapField(entries, "old_admin")
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("%w: missing 'old_admin': %w", ErrMalformedBody, err)
	}
	oldAdmin, err := scval.AsAddressOrVoid(oldAdminSV)
	if err != nil {
		return AdminChangeStarted{}, fmt.Errorf("cctp: admin_change_started old_admin: %w", err)
	}

	return AdminChangeStarted{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		NewAdmin:   newAdmin,
		OldAdmin:   oldAdmin,
	}, nil
}

// DecodeAttesterEnabled turns one `attester_enabled` event into the
// canonical struct. 2-topic event; body carries no fields.
//
// Topic layout:
//
//	topic[0] = Symbol("attester_enabled")
//	topic[1] = BytesN<20>(attester)
func DecodeAttesterEnabled(e *events.Event) (AttesterEnabled, error) {
	if len(e.Topic) < 2 {
		return AttesterEnabled{}, fmt.Errorf("%w: attester_enabled needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	attesterSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return AttesterEnabled{}, fmt.Errorf("cctp: attester_enabled topic[1] parse: %w", err)
	}
	attester, err := scval.AsBytes(attesterSV)
	if err != nil {
		return AttesterEnabled{}, fmt.Errorf("cctp: attester_enabled attester: %w", err)
	}
	return AttesterEnabled{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		Attester:   hex.EncodeToString(attester),
	}, nil
}

// DecodeAttesterManagerUpdated turns one `attester_manager_updated`
// event into the canonical struct. 3-topic event; body carries no
// fields.
//
// Topic layout:
//
//	topic[0] = Symbol("attester_manager_updated")
//	topic[1] = Address(old_attester_manager) | Void
//	topic[2] = Address(new_attester_manager)
//
// old_attester_manager is type-tested via [scval.AsAddressOrVoid] — the
// real mainnet bootstrap instance (ledger 62146641) carries it Void.
func DecodeAttesterManagerUpdated(e *events.Event) (AttesterManagerUpdated, error) {
	if len(e.Topic) < 3 {
		return AttesterManagerUpdated{}, fmt.Errorf("%w: attester_manager_updated needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	oldSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return AttesterManagerUpdated{}, fmt.Errorf("cctp: attester_manager_updated topic[1] parse: %w", err)
	}
	oldMgr, err := scval.AsAddressOrVoid(oldSV)
	if err != nil {
		return AttesterManagerUpdated{}, fmt.Errorf("cctp: attester_manager_updated old_attester_manager: %w", err)
	}
	newSV, err := scval.Parse(e.Topic[2])
	if err != nil {
		return AttesterManagerUpdated{}, fmt.Errorf("cctp: attester_manager_updated topic[2] parse: %w", err)
	}
	newMgr, err := scval.AsAddressStrkey(newSV)
	if err != nil {
		return AttesterManagerUpdated{}, fmt.Errorf("cctp: attester_manager_updated new_attester_manager: %w", err)
	}
	return AttesterManagerUpdated{
		Ledger:             e.Ledger,
		TxHash:             e.TxHash,
		OpIndex:            e.OperationIndex,
		ClosedAt:           e.LedgerClosedAt,
		ContractID:         e.ContractID,
		OldAttesterManager: oldMgr,
		NewAttesterManager: newMgr,
	}, nil
}

// DecodeDenylisted turns one `denylisted` event into the canonical
// struct. 2-topic event; body carries no fields.
//
// Topic layout:
//
//	topic[0] = Symbol("denylisted")
//	topic[1] = Address(account)
func DecodeDenylisted(e *events.Event) (Denylisted, error) {
	if len(e.Topic) < 2 {
		return Denylisted{}, fmt.Errorf("%w: denylisted needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	accountSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return Denylisted{}, fmt.Errorf("cctp: denylisted topic[1] parse: %w", err)
	}
	account, err := scval.AsAddressStrkey(accountSV)
	if err != nil {
		return Denylisted{}, fmt.Errorf("cctp: denylisted account: %w", err)
	}
	return Denylisted{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		Account:    account,
	}, nil
}

// DecodeUnDenylisted turns one `un_denylisted` event into the
// canonical struct. 2-topic event; body carries no fields. Same shape
// as [DecodeDenylisted].
//
// Topic layout:
//
//	topic[0] = Symbol("un_denylisted")
//	topic[1] = Address(account)
func DecodeUnDenylisted(e *events.Event) (UnDenylisted, error) {
	if len(e.Topic) < 2 {
		return UnDenylisted{}, fmt.Errorf("%w: un_denylisted needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	accountSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return UnDenylisted{}, fmt.Errorf("cctp: un_denylisted topic[1] parse: %w", err)
	}
	account, err := scval.AsAddressStrkey(accountSV)
	if err != nil {
		return UnDenylisted{}, fmt.Errorf("cctp: un_denylisted account: %w", err)
	}
	return UnDenylisted{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		Account:    account,
	}, nil
}

// DecodeDenylisterChanged turns one `denylister_changed` event into
// the canonical struct. 3-topic event; body carries no fields. Same
// old|void / new topic shape as [DecodeAttesterManagerUpdated].
//
// Topic layout:
//
//	topic[0] = Symbol("denylister_changed")
//	topic[1] = Address(old_denylister) | Void
//	topic[2] = Address(new_denylister)
func DecodeDenylisterChanged(e *events.Event) (DenylisterChanged, error) {
	if len(e.Topic) < 3 {
		return DenylisterChanged{}, fmt.Errorf("%w: denylister_changed needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	oldSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return DenylisterChanged{}, fmt.Errorf("cctp: denylister_changed topic[1] parse: %w", err)
	}
	oldDenylister, err := scval.AsAddressOrVoid(oldSV)
	if err != nil {
		return DenylisterChanged{}, fmt.Errorf("cctp: denylister_changed old_denylister: %w", err)
	}
	newSV, err := scval.Parse(e.Topic[2])
	if err != nil {
		return DenylisterChanged{}, fmt.Errorf("cctp: denylister_changed topic[2] parse: %w", err)
	}
	newDenylister, err := scval.AsAddressStrkey(newSV)
	if err != nil {
		return DenylisterChanged{}, fmt.Errorf("cctp: denylister_changed new_denylister: %w", err)
	}
	return DenylisterChanged{
		Ledger:        e.Ledger,
		TxHash:        e.TxHash,
		OpIndex:       e.OperationIndex,
		ClosedAt:      e.LedgerClosedAt,
		ContractID:    e.ContractID,
		OldDenylister: oldDenylister,
		NewDenylister: newDenylister,
	}, nil
}

// DecodeFeeRecipientSet turns one `fee_recipient_set` event into the
// canonical struct. Single-topic event; body ScMap.
//
// Body: { fee_recipient: Address }.
func DecodeFeeRecipientSet(e *events.Event) (FeeRecipientSet, error) {
	if len(e.Topic) < 1 {
		return FeeRecipientSet{}, fmt.Errorf("%w: fee_recipient_set needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return FeeRecipientSet{}, fmt.Errorf("cctp: fee_recipient_set body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return FeeRecipientSet{}, fmt.Errorf("cctp: fee_recipient_set body not a map: %w", err)
	}
	feeRecipientSV, err := scval.MustMapField(entries, "fee_recipient")
	if err != nil {
		return FeeRecipientSet{}, fmt.Errorf("%w: missing 'fee_recipient': %w", ErrMalformedBody, err)
	}
	feeRecipient, err := scval.AsAddressStrkey(feeRecipientSV)
	if err != nil {
		return FeeRecipientSet{}, fmt.Errorf("cctp: fee_recipient_set fee_recipient: %w", err)
	}
	return FeeRecipientSet{
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      e.OperationIndex,
		ClosedAt:     e.LedgerClosedAt,
		ContractID:   e.ContractID,
		FeeRecipient: feeRecipient,
	}, nil
}

// DecodeMaxMessageBodySizeUpdated turns one
// `max_message_body_size_updated` event into the canonical struct.
// Single-topic event; body ScMap.
//
// Body: { new_max_message_body_size: u32 }.
func DecodeMaxMessageBodySizeUpdated(e *events.Event) (MaxMessageBodySizeUpdated, error) {
	if len(e.Topic) < 1 {
		return MaxMessageBodySizeUpdated{}, fmt.Errorf("%w: max_message_body_size_updated needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return MaxMessageBodySizeUpdated{}, fmt.Errorf("cctp: max_message_body_size_updated body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return MaxMessageBodySizeUpdated{}, fmt.Errorf("cctp: max_message_body_size_updated body not a map: %w", err)
	}
	newSizeSV, err := scval.MustMapField(entries, "new_max_message_body_size")
	if err != nil {
		return MaxMessageBodySizeUpdated{}, fmt.Errorf("%w: missing 'new_max_message_body_size': %w", ErrMalformedBody, err)
	}
	newSize, err := scval.AsU32(newSizeSV)
	if err != nil {
		return MaxMessageBodySizeUpdated{}, fmt.Errorf("cctp: max_message_body_size_updated new_max_message_body_size: %w", err)
	}
	return MaxMessageBodySizeUpdated{
		Ledger:                e.Ledger,
		TxHash:                e.TxHash,
		OpIndex:               e.OperationIndex,
		ClosedAt:              e.LedgerClosedAt,
		ContractID:            e.ContractID,
		NewMaxMessageBodySize: newSize,
	}, nil
}

// DecodeMinFeeControllerSet turns one `min_fee_controller_set` event
// into the canonical struct. 2-topic event; body carries no fields.
//
// Topic layout:
//
//	topic[0] = Symbol("min_fee_controller_set")
//	topic[1] = Address(min_fee_controller)
func DecodeMinFeeControllerSet(e *events.Event) (MinFeeControllerSet, error) {
	if len(e.Topic) < 2 {
		return MinFeeControllerSet{}, fmt.Errorf("%w: min_fee_controller_set needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	controllerSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return MinFeeControllerSet{}, fmt.Errorf("cctp: min_fee_controller_set topic[1] parse: %w", err)
	}
	controller, err := scval.AsAddressStrkey(controllerSV)
	if err != nil {
		return MinFeeControllerSet{}, fmt.Errorf("cctp: min_fee_controller_set min_fee_controller: %w", err)
	}
	return MinFeeControllerSet{
		Ledger:           e.Ledger,
		TxHash:           e.TxHash,
		OpIndex:          e.OperationIndex,
		ClosedAt:         e.LedgerClosedAt,
		ContractID:       e.ContractID,
		MinFeeController: controller,
	}, nil
}

// DecodePauserChanged turns one `pauser_changed` event into the
// canonical struct. Single-topic event; body ScMap. NOTE: the body
// field is named `new_address`, not `new_pauser` — confirmed against
// the real mainnet event, not assumed from the topic name.
//
// Body: { new_address: Address }.
func DecodePauserChanged(e *events.Event) (PauserChanged, error) {
	if len(e.Topic) < 1 {
		return PauserChanged{}, fmt.Errorf("%w: pauser_changed needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return PauserChanged{}, fmt.Errorf("cctp: pauser_changed body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return PauserChanged{}, fmt.Errorf("cctp: pauser_changed body not a map: %w", err)
	}
	newAddrSV, err := scval.MustMapField(entries, "new_address")
	if err != nil {
		return PauserChanged{}, fmt.Errorf("%w: missing 'new_address': %w", ErrMalformedBody, err)
	}
	newAddr, err := scval.AsAddressStrkey(newAddrSV)
	if err != nil {
		return PauserChanged{}, fmt.Errorf("cctp: pauser_changed new_address: %w", err)
	}
	return PauserChanged{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		NewAddress: newAddr,
	}, nil
}

// DecodeRescuerChanged turns one `rescuer_changed` event into the
// canonical struct. Single-topic event; body ScMap.
//
// Body: { new_rescuer: Address }.
func DecodeRescuerChanged(e *events.Event) (RescuerChanged, error) {
	if len(e.Topic) < 1 {
		return RescuerChanged{}, fmt.Errorf("%w: rescuer_changed needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return RescuerChanged{}, fmt.Errorf("cctp: rescuer_changed body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return RescuerChanged{}, fmt.Errorf("cctp: rescuer_changed body not a map: %w", err)
	}
	newRescuerSV, err := scval.MustMapField(entries, "new_rescuer")
	if err != nil {
		return RescuerChanged{}, fmt.Errorf("%w: missing 'new_rescuer': %w", ErrMalformedBody, err)
	}
	newRescuer, err := scval.AsAddressStrkey(newRescuerSV)
	if err != nil {
		return RescuerChanged{}, fmt.Errorf("cctp: rescuer_changed new_rescuer: %w", err)
	}
	return RescuerChanged{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		NewRescuer: newRescuer,
	}, nil
}

// DecodeSetTokenController turns one `set_token_controller` event into
// the canonical struct. Single-topic event; body ScMap.
//
// Body: { token_controller: Address }.
func DecodeSetTokenController(e *events.Event) (SetTokenController, error) {
	if len(e.Topic) < 1 {
		return SetTokenController{}, fmt.Errorf("%w: set_token_controller needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return SetTokenController{}, fmt.Errorf("cctp: set_token_controller body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SetTokenController{}, fmt.Errorf("cctp: set_token_controller body not a map: %w", err)
	}
	controllerSV, err := scval.MustMapField(entries, "token_controller")
	if err != nil {
		return SetTokenController{}, fmt.Errorf("%w: missing 'token_controller': %w", ErrMalformedBody, err)
	}
	controller, err := scval.AsAddressStrkey(controllerSV)
	if err != nil {
		return SetTokenController{}, fmt.Errorf("cctp: set_token_controller token_controller: %w", err)
	}
	return SetTokenController{
		Ledger:          e.Ledger,
		TxHash:          e.TxHash,
		OpIndex:         e.OperationIndex,
		ClosedAt:        e.LedgerClosedAt,
		ContractID:      e.ContractID,
		TokenController: controller,
	}, nil
}

// DecodeSignatureThresholdUpdated turns one
// `signature_threshold_updated` event into the canonical struct.
// Single-topic event; body ScMap.
//
// Body: { new_signature_threshold: u32, old_signature_threshold: u32 }.
func DecodeSignatureThresholdUpdated(e *events.Event) (SignatureThresholdUpdated, error) { //nolint:dupl // parallel to DecodeMaxMessageBodySizeUpdated's shape but a distinct 2-field event; extracting a shared helper would obscure which fields belong to which contract event
	if len(e.Topic) < 1 {
		return SignatureThresholdUpdated{}, fmt.Errorf("%w: signature_threshold_updated needs 1 topic, got %d", ErrMalformedTopic, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("cctp: signature_threshold_updated body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("cctp: signature_threshold_updated body not a map: %w", err)
	}
	newSV, err := scval.MustMapField(entries, "new_signature_threshold")
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("%w: missing 'new_signature_threshold': %w", ErrMalformedBody, err)
	}
	newThreshold, err := scval.AsU32(newSV)
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("cctp: signature_threshold_updated new_signature_threshold: %w", err)
	}
	oldSV, err := scval.MustMapField(entries, "old_signature_threshold")
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("%w: missing 'old_signature_threshold': %w", ErrMalformedBody, err)
	}
	oldThreshold, err := scval.AsU32(oldSV)
	if err != nil {
		return SignatureThresholdUpdated{}, fmt.Errorf("cctp: signature_threshold_updated old_signature_threshold: %w", err)
	}
	return SignatureThresholdUpdated{
		Ledger:                e.Ledger,
		TxHash:                e.TxHash,
		OpIndex:               e.OperationIndex,
		ClosedAt:              e.LedgerClosedAt,
		ContractID:            e.ContractID,
		NewSignatureThreshold: newThreshold,
		OldSignatureThreshold: oldThreshold,
	}, nil
}

// DecodeSetBurnLimitPerMessage turns one `set_burn_limit_per_message`
// event into the canonical struct.
//
// Topic layout:
//
//	topic[0] = Symbol("set_burn_limit_per_message")
//	topic[1] = Address(token)
//
// Body: { burn_limit_per_message: i128 }.
func DecodeSetBurnLimitPerMessage(e *events.Event) (SetBurnLimitPerMessage, error) {
	if len(e.Topic) < 2 {
		return SetBurnLimitPerMessage{}, fmt.Errorf("%w: set_burn_limit_per_message needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	tokenSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("cctp: set_burn_limit_per_message topic[1] parse: %w", err)
	}
	token, err := scval.AsAddressStrkey(tokenSV)
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("cctp: set_burn_limit_per_message token: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("cctp: set_burn_limit_per_message body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("cctp: set_burn_limit_per_message body not a map: %w", err)
	}
	limitSV, err := scval.MustMapField(entries, "burn_limit_per_message")
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("%w: missing 'burn_limit_per_message': %w", ErrMalformedBody, err)
	}
	limit, err := scval.AsAmountFromI128(limitSV)
	if err != nil {
		return SetBurnLimitPerMessage{}, fmt.Errorf("cctp: set_burn_limit_per_message burn_limit_per_message: %w", err)
	}

	return SetBurnLimitPerMessage{
		Ledger:              e.Ledger,
		TxHash:              e.TxHash,
		OpIndex:             e.OperationIndex,
		ClosedAt:            e.LedgerClosedAt,
		ContractID:          e.ContractID,
		Token:               token,
		BurnLimitPerMessage: limit.String(),
	}, nil
}

// DecodeSwapMinterConfigSet turns one `swap_minter_config_set` event
// into the canonical struct. The body's `swap_minter_config` field is
// a NESTED map — flattened into AllowAsset / SwapMinter.
//
// Topic layout:
//
//	topic[0] = Symbol("swap_minter_config_set")
//	topic[1] = Address(token)
//
// Body: { swap_minter_config: { allow_asset: Address, swap_minter: Address } }.
func DecodeSwapMinterConfigSet(e *events.Event) (SwapMinterConfigSet, error) { //nolint:dupl // parallel structure to DecodeTokenDecimalConfigAdded's nested-map unwrap but distinct field names/types; a shared helper would obscure which fields belong to which contract event
	if len(e.Topic) < 2 {
		return SwapMinterConfigSet{}, fmt.Errorf("%w: swap_minter_config_set needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	tokenSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set topic[1] parse: %w", err)
	}
	token, err := scval.AsAddressStrkey(tokenSV)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set token: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set body not a map: %w", err)
	}
	configSV, err := scval.MustMapField(entries, "swap_minter_config")
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("%w: missing 'swap_minter_config': %w", ErrMalformedBody, err)
	}
	configEntries, err := scval.AsMap(configSV)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set swap_minter_config not a map: %w", err)
	}

	allowAssetSV, err := scval.MustMapField(configEntries, "allow_asset")
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("%w: missing 'allow_asset': %w", ErrMalformedBody, err)
	}
	allowAsset, err := scval.AsAddressStrkey(allowAssetSV)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set allow_asset: %w", err)
	}

	swapMinterSV, err := scval.MustMapField(configEntries, "swap_minter")
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("%w: missing 'swap_minter': %w", ErrMalformedBody, err)
	}
	swapMinter, err := scval.AsAddressStrkey(swapMinterSV)
	if err != nil {
		return SwapMinterConfigSet{}, fmt.Errorf("cctp: swap_minter_config_set swap_minter: %w", err)
	}

	return SwapMinterConfigSet{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ClosedAt:   e.LedgerClosedAt,
		ContractID: e.ContractID,
		Token:      token,
		AllowAsset: allowAsset,
		SwapMinter: swapMinter,
	}, nil
}

// DecodeTokenDecimalConfigAdded turns one `token_decimal_config_added`
// event into the canonical struct. The body's `token_decimal_config`
// field is a NESTED map — flattened into CanonicalDecimals /
// LocalDecimals.
//
// Topic layout:
//
//	topic[0] = Symbol("token_decimal_config_added")
//	topic[1] = Address(token)
//
// Body: { token_decimal_config: { canonical_decimals: u32, local_decimals: u32 } }.
func DecodeTokenDecimalConfigAdded(e *events.Event) (TokenDecimalConfigAdded, error) { //nolint:dupl // parallel structure to DecodeSwapMinterConfigSet's nested-map unwrap but distinct field names/types; a shared helper would obscure which fields belong to which contract event
	if len(e.Topic) < 2 {
		return TokenDecimalConfigAdded{}, fmt.Errorf("%w: token_decimal_config_added needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	tokenSV, err := scval.Parse(e.Topic[1])
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added topic[1] parse: %w", err)
	}
	token, err := scval.AsAddressStrkey(tokenSV)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added token: %w", err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added body not a map: %w", err)
	}
	configSV, err := scval.MustMapField(entries, "token_decimal_config")
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("%w: missing 'token_decimal_config': %w", ErrMalformedBody, err)
	}
	configEntries, err := scval.AsMap(configSV)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added token_decimal_config not a map: %w", err)
	}

	canonicalSV, err := scval.MustMapField(configEntries, "canonical_decimals")
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("%w: missing 'canonical_decimals': %w", ErrMalformedBody, err)
	}
	canonical, err := scval.AsU32(canonicalSV)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added canonical_decimals: %w", err)
	}

	localSV, err := scval.MustMapField(configEntries, "local_decimals")
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("%w: missing 'local_decimals': %w", ErrMalformedBody, err)
	}
	local, err := scval.AsU32(localSV)
	if err != nil {
		return TokenDecimalConfigAdded{}, fmt.Errorf("cctp: token_decimal_config_added local_decimals: %w", err)
	}

	return TokenDecimalConfigAdded{
		Ledger:            e.Ledger,
		TxHash:            e.TxHash,
		OpIndex:           e.OperationIndex,
		ClosedAt:          e.LedgerClosedAt,
		ContractID:        e.ContractID,
		Token:             token,
		CanonicalDecimals: canonical,
		LocalDecimals:     local,
	}, nil
}
