package cctp

import (
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of Circle CCTP v2.
type Decoder struct{}

// NewDecoder constructs a stateless CCTP Decoder that is safe to share.
func NewDecoder() *Decoder { return &Decoder{} }

// withIndex stamps the intra-op event index onto the projected Event and wraps it as the
// decoder's single-event output. EventIndex is the cctp_events PK discriminator that keeps two
// events of the same event_type from one operation from collapsing into a single row; the
// dispatcher and the lake re-derive path both supply it.
func withIndex(idx int, e Event) []consumer.Event {
	e.EventIndex = uint32(idx) //nolint:gosec // EventIndex is non-negative by Soroban spec.
	return []consumer.Event{e}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// cctpContracts is the set of contract C-strkeys whose events this decoder claims. It is small
// and a redeploy is operator-visible, so a hard-coded set suffices.
var cctpContracts = map[string]struct{}{
	MainnetTokenMessengerMinter: {},
	MainnetMessageTransmitter:   {},
	MainnetCctpForwarder:        {},
}

// MainnetContracts returns the known Circle CCTP v2 contract set, the recognition-attribution pin for the ADR-0033 catalogue.
func MainnetContracts() []string {
	out := make([]string, 0, len(cctpContracts))
	for id := range cctpContracts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsCCTPContract reports whether id is one of the known Circle CCTP v2 contracts on Stellar mainnet.
func IsCCTPContract(id string) bool {
	_, ok := cctpContracts[id]
	return ok
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]: it claims an event only when topic[0] is a CCTP symbol AND a known CCTP contract emitted it.
func (*Decoder) Matches(ev events.Event) bool {
	return IsCCTPContract(ev.ContractID) && Classify(&ev) != ""
}

// Decode implements [dispatcher.Decoder], re-checking Matches so a direct caller is safe; a decode error is non-fatal (counted and skipped).
func (*Decoder) Decode(ev events.Event) ([]consumer.Event, error) { //nolint:gocognit,gocyclo,cyclop,funlen // one case per CCTP event kind; the flat dispatch table stays auditable against the upstream contract's event list (same exemption as blend's decodeByKind)
	kind := Classify(&ev)
	if kind == "" || !IsCCTPContract(ev.ContractID) {
		return nil, nil
	}

	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, fmt.Errorf("cctp: %s: %w", kind, err)
	}

	switch kind {
	case EventDepositForBurn:
		d, err := DecodeDepositForBurn(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromDepositForBurn(d, observedAt)), nil
	case EventMintAndWithdraw:
		m, err := DecodeMintAndWithdraw(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMintAndWithdraw(m, observedAt)), nil
	case EventMessageSent:
		s, err := DecodeMessageSent(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMessageSent(s, observedAt)), nil
	case EventMessageReceived:
		r, err := DecodeMessageReceived(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMessageReceived(r, observedAt)), nil
	case EventMintAndForward:
		m, err := DecodeMintAndForward(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMintAndForward(m, observedAt)), nil
	case EventOwnershipTransfer:
		o, err := DecodeOwnershipTransfer(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromOwnershipTransfer(o, observedAt)), nil
	case EventOwnershipTransferCompleted:
		o, err := DecodeOwnershipTransferCompleted(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromOwnershipTransferCompleted(o, observedAt)), nil
	case EventAdminChanged:
		a, err := DecodeAdminChanged(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromAdminChanged(a, observedAt)), nil
	case EventRemoteTokenMessengerAdded:
		r, err := DecodeRemoteTokenMessengerAdded(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromRemoteTokenMessengerAdded(r, observedAt)), nil
	case EventTokenPairLinked:
		l, err := DecodeTokenPairLinked(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromTokenPairLinked(l, observedAt)), nil
	case EventAdminChangeStarted:
		a, err := DecodeAdminChangeStarted(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromAdminChangeStarted(a, observedAt)), nil
	case EventAttesterEnabled:
		a, err := DecodeAttesterEnabled(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromAttesterEnabled(a, observedAt)), nil
	case EventAttesterManagerUpdated:
		a, err := DecodeAttesterManagerUpdated(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromAttesterManagerUpdated(a, observedAt)), nil
	case EventDenylisted:
		d, err := DecodeDenylisted(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromDenylisted(d, observedAt)), nil
	case EventUnDenylisted:
		u, err := DecodeUnDenylisted(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromUnDenylisted(u, observedAt)), nil
	case EventDenylisterChanged:
		d, err := DecodeDenylisterChanged(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromDenylisterChanged(d, observedAt)), nil
	case EventFeeRecipientSet:
		f, err := DecodeFeeRecipientSet(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromFeeRecipientSet(f, observedAt)), nil
	case EventMaxMessageBodySizeUpdated:
		m, err := DecodeMaxMessageBodySizeUpdated(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMaxMessageBodySizeUpdated(m, observedAt)), nil
	case EventMinFeeControllerSet:
		m, err := DecodeMinFeeControllerSet(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromMinFeeControllerSet(m, observedAt)), nil
	case EventPauserChanged:
		p, err := DecodePauserChanged(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromPauserChanged(p, observedAt)), nil
	case EventRescuerChanged:
		r, err := DecodeRescuerChanged(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromRescuerChanged(r, observedAt)), nil
	case EventSetTokenController:
		s, err := DecodeSetTokenController(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromSetTokenController(s, observedAt)), nil
	case EventSignatureThresholdUpdated:
		s, err := DecodeSignatureThresholdUpdated(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromSignatureThresholdUpdated(s, observedAt)), nil
	case EventSetBurnLimitPerMessage:
		s, err := DecodeSetBurnLimitPerMessage(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromSetBurnLimitPerMessage(s, observedAt)), nil
	case EventSwapMinterConfigSet:
		s, err := DecodeSwapMinterConfigSet(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromSwapMinterConfigSet(s, observedAt)), nil
	case EventTokenDecimalConfigAdded:
		t, err := DecodeTokenDecimalConfigAdded(&ev)
		if err != nil {
			return nil, err
		}
		return withIndex(ev.EventIndex, eventFromTokenDecimalConfigAdded(t, observedAt)), nil
	}
	// Unreachable while Classify and this switch stay in lockstep. The sentinel makes a Classify
	// case without a switch arm a counted decode error rather than a silent drop.
	return nil, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
}
