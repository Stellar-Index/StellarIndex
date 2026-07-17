package cctp

import (
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of Circle CCTP v2. It is a
// stateless topic Decoder — unlike Soroswap there is no swap/sync
// correlation: each CCTP event (transfer-flow or governance/admin)
// decodes independently into one cctp_events row. The
// deposit_for_burn ↔ message_sent pairing the architecture doc
// describes is a downstream concern, correlatable later by (ledger,
// tx_hash); the decoder does not buffer.
//
// Matching is by topic[0] symbol AND contract id. CLAUDE.md ("Comet
// uses a shared topic") warns that another contract could emit the
// same symbol bytes, so Matches also gates on the event coming from
// one of the three known CCTP contracts.
type Decoder struct{}

// NewDecoder constructs a CCTP Decoder. Stateless — the returned
// value is safe to share.
func NewDecoder() *Decoder { return &Decoder{} }

// withIndex stamps the intra-op event index onto the projected Event and
// wraps it as the decoder's single-event output. EventIndex (the position
// of this event within its operation's contract-event list) is the
// cctp_events PK discriminator (migration 0112) that keeps two events of
// the SAME event_type emitted by ONE operation from collapsing to a single
// row (C2-13a). idx comes from the source events.Event.EventIndex, which
// the production dispatcher populates from the LCM and the re-derive path
// reads back from the lake's event_index column.
func withIndex(idx int, e Event) []consumer.Event {
	e.EventIndex = uint32(idx) //nolint:gosec // EventIndex is non-negative by Soroban spec.
	return []consumer.Event{e}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// cctpContracts is the set of contract C-strkeys whose events this
// decoder claims. Live ingest only ever sees the current mainnet
// deployment; the set is small and a redeploy is an operator-visible
// event, so a hard-coded set is the right shape (matching the
// arch doc's Option A — contract-id filtering downstream of the
// topic match).
var cctpContracts = map[string]struct{}{
	MainnetTokenMessengerMinter: {},
	MainnetMessageTransmitter:   {},
	MainnetCctpForwarder:        {},
}

// MainnetContracts returns the known Circle CCTP v2 contract set —
// the recognition-attribution pin for the ADR-0033 catalogue
// (board #31: without contract pinning, an unhandled cctp topic fell
// into the system-wide recognition bucket instead of capping this
// source).
func MainnetContracts() []string {
	out := make([]string, 0, len(cctpContracts))
	for id := range cctpContracts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsCCTPContract reports whether id is one of the known Circle CCTP v2
// contracts on Stellar mainnet.
func IsCCTPContract(id string) bool {
	_, ok := cctpContracts[id]
	return ok
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Claims an event when its
// topic[0] is one of the four CCTP symbols AND it was emitted by a
// known CCTP contract.
func (*Decoder) Matches(ev events.Event) bool {
	return IsCCTPContract(ev.ContractID) && Classify(&ev) != ""
}

// Decode implements [dispatcher.Decoder]. Emits exactly one
// [Event] per recognised CCTP event, or nothing for an event that
// doesn't match (the dispatcher already filtered via Matches, but
// Decode re-checks so a direct caller is safe). A decode error is
// non-fatal per the dispatcher contract — counted and skipped.
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
	// Unreachable while Classify and this switch stay in lockstep —
	// Classify already returned non-empty above, and every kind it
	// can return has a case. Returning the sentinel makes the
	// defensive guard real: if a future Classify case lands without a
	// matching switch arm, the dispatcher counts it as a decode error
	// rather than silently dropping the event.
	return nil, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
}
