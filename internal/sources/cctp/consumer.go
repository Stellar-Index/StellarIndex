package cctp

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is the [consumer.Event] the CCTP Decoder emits — one per
// decoded contract event. It carries the cctp_events row shape
// (migration 0038): the universal identity fields, the promoted
// typed columns (Amount / Fee / Token / CounterpartyDomain), and the
// event-type-specific remainder in Attributes.
//
// Amount / Fee / Token are decimal-or-strkey strings; an empty value
// means "this event type carries no such field" and the sink writes
// SQL NULL. CounterpartyDomain is a *uint32 for the same reason —
// message_sent and mint_and_withdraw carry no domain.
//
// The indexer's event sink type-switches on this at its output
// channel (internal/pipeline/sink.go) and writes via
// Store.InsertCCTPEvent.
type Event struct {
	ContractID         string
	Ledger             uint32
	TxHash             string
	OpIndex            int
	ObservedAt         time.Time
	EventType          string // one of the Event* constants
	Amount             string // decimal i128; "" → none
	Fee                string // decimal i128; "" → none
	Token              string // Stellar Address strkey; "" → none
	CounterpartyDomain *uint32
	Attributes         map[string]any
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return "cctp.event" }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}

// eventFromDepositForBurn projects a decoded DepositForBurn into the
// canonical row Event. burn_token / max_fee / destination_domain are
// promoted; the BytesN<32> hex fields + topics land in Attributes.
func eventFromDepositForBurn(d DepositForBurn, observedAt time.Time) Event {
	domain := d.DestinationDomain
	return Event{
		ContractID:         d.ContractID,
		Ledger:             d.Ledger,
		TxHash:             d.TxHash,
		OpIndex:            d.OpIndex,
		ObservedAt:         observedAt,
		EventType:          EventDepositForBurn,
		Amount:             d.Amount,
		Fee:                d.MaxFee,
		Token:              d.BurnToken,
		CounterpartyDomain: &domain,
		Attributes: map[string]any{
			"depositor":                   d.Depositor,
			"mint_recipient":              d.MintRecipient,
			"destination_token_messenger": d.DestinationTokenMessenger,
			"destination_caller":          d.DestinationCaller,
			"min_finality_threshold":      d.MinFinalityThreshold,
			"hook_data":                   d.HookData,
		},
	}
}

// eventFromMintAndWithdraw projects a decoded MintAndWithdraw. It
// carries no destination/source domain, so CounterpartyDomain is nil.
func eventFromMintAndWithdraw(m MintAndWithdraw, observedAt time.Time) Event {
	return Event{
		ContractID: m.ContractID,
		Ledger:     m.Ledger,
		TxHash:     m.TxHash,
		OpIndex:    m.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventMintAndWithdraw,
		Amount:     m.Amount,
		Fee:        m.FeeCollected,
		Token:      m.MintToken,
		Attributes: map[string]any{
			"mint_recipient": m.MintRecipient,
		},
	}
}

// eventFromMessageSent projects a decoded MessageSent — a wire
// envelope with no amount, token or domain.
func eventFromMessageSent(s MessageSent, observedAt time.Time) Event {
	return Event{
		ContractID: s.ContractID,
		Ledger:     s.Ledger,
		TxHash:     s.TxHash,
		OpIndex:    s.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventMessageSent,
		Attributes: map[string]any{
			"message": s.Message,
		},
	}
}

// eventFromMessageReceived projects a decoded MessageReceived. The
// source_domain is promoted to CounterpartyDomain.
func eventFromMessageReceived(r MessageReceived, observedAt time.Time) Event {
	domain := r.SourceDomain
	return Event{
		ContractID:         r.ContractID,
		Ledger:             r.Ledger,
		TxHash:             r.TxHash,
		OpIndex:            r.OpIndex,
		ObservedAt:         observedAt,
		EventType:          EventMessageReceived,
		CounterpartyDomain: &domain,
		Attributes: map[string]any{
			"caller":                      r.Caller,
			"nonce":                       r.Nonce,
			"finality_threshold_executed": r.FinalityThresholdExecuted,
			"sender":                      r.Sender,
			"message_body":                r.MessageBody,
		},
	}
}

// eventFromMintAndForward projects a decoded MintAndForward. The
// forward_recipient lands in Attributes (same convention as
// mint_and_withdraw's mint_recipient).
func eventFromMintAndForward(m MintAndForward, observedAt time.Time) Event {
	return Event{
		ContractID: m.ContractID,
		Ledger:     m.Ledger,
		TxHash:     m.TxHash,
		OpIndex:    m.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventMintAndForward,
		Amount:     m.Amount,
		Token:      m.Token,
		Attributes: map[string]any{
			"forward_recipient": m.ForwardRecipient,
		},
	}
}

// eventFromOwnershipTransfer projects a decoded OwnershipTransfer.
// Governance event: carries no amount/fee/token/domain, so all four
// promoted columns stay empty/nil and every field lands in Attributes.
func eventFromOwnershipTransfer(o OwnershipTransfer, observedAt time.Time) Event {
	return Event{
		ContractID: o.ContractID,
		Ledger:     o.Ledger,
		TxHash:     o.TxHash,
		OpIndex:    o.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventOwnershipTransfer,
		Attributes: map[string]any{
			"live_until_ledger": o.LiveUntilLedger,
			"new_owner":         o.NewOwner,
			"old_owner":         o.OldOwner,
		},
	}
}

// eventFromOwnershipTransferCompleted projects a decoded
// OwnershipTransferCompleted. Governance event: no promoted columns.
func eventFromOwnershipTransferCompleted(o OwnershipTransferCompleted, observedAt time.Time) Event {
	return Event{
		ContractID: o.ContractID,
		Ledger:     o.Ledger,
		TxHash:     o.TxHash,
		OpIndex:    o.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventOwnershipTransferCompleted,
		Attributes: map[string]any{
			"new_owner": o.NewOwner,
		},
	}
}

// eventFromAdminChanged projects a decoded AdminChanged. Governance
// event: no promoted columns. old_admin may be "" (bootstrap — see
// [DecodeAdminChanged]); the sink still writes it as an empty jsonb
// string rather than omitting the key, so a query can distinguish
// "field present but empty" from "field never decoded".
func eventFromAdminChanged(a AdminChanged, observedAt time.Time) Event {
	return Event{
		ContractID: a.ContractID,
		Ledger:     a.Ledger,
		TxHash:     a.TxHash,
		OpIndex:    a.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventAdminChanged,
		Attributes: map[string]any{
			"new_admin": a.NewAdmin,
			"old_admin": a.OldAdmin,
		},
	}
}

// eventFromRemoteTokenMessengerAdded projects a decoded
// RemoteTokenMessengerAdded. `domain` promotes to CounterpartyDomain
// (same semantic as deposit_for_burn's destination_domain — the CCTP
// domain ID of the OTHER chain); token_messenger has no strkey form
// (it's a raw remote-chain identity) so it stays in Attributes as hex.
func eventFromRemoteTokenMessengerAdded(r RemoteTokenMessengerAdded, observedAt time.Time) Event {
	domain := r.Domain
	return Event{
		ContractID:         r.ContractID,
		Ledger:             r.Ledger,
		TxHash:             r.TxHash,
		OpIndex:            r.OpIndex,
		ObservedAt:         observedAt,
		EventType:          EventRemoteTokenMessengerAdded,
		CounterpartyDomain: &domain,
		Attributes: map[string]any{
			"token_messenger": r.TokenMessenger,
		},
	}
}

// eventFromTokenPairLinked projects a decoded TokenPairLinked.
// `local_token` promotes to Token (a genuine Stellar Address strkey,
// same convention as burn_token/mint_token) and `remote_domain`
// promotes to CounterpartyDomain; remote_token has no strkey form so
// it stays in Attributes as hex.
func eventFromTokenPairLinked(l TokenPairLinked, observedAt time.Time) Event {
	domain := l.RemoteDomain
	return Event{
		ContractID:         l.ContractID,
		Ledger:             l.Ledger,
		TxHash:             l.TxHash,
		OpIndex:            l.OpIndex,
		ObservedAt:         observedAt,
		EventType:          EventTokenPairLinked,
		Token:              l.LocalToken,
		CounterpartyDomain: &domain,
		Attributes: map[string]any{
			"remote_token": l.RemoteToken,
		},
	}
}

// ─── Lower-signal admin/governance events (ROADMAP #89c, 2026-07-09) ──
//
// All 16 are governance/config events: no promoted Amount/Fee, and no
// CounterpartyDomain (none of them carry a cross-chain domain field).
// Token promotes only where the field is a genuine local Stellar SAC
// address (set_burn_limit_per_message / swap_minter_config_set /
// token_decimal_config_added — matching token_pair_linked's
// local_token convention); every other address-shaped field (actor
// roles: admin, owner, pauser, rescuer, controller, denylister,
// attester manager, denylist target) stays in Attributes, matching
// ownership_transfer / admin_changed's precedent.

func eventFromAdminChangeStarted(a AdminChangeStarted, observedAt time.Time) Event {
	return Event{
		ContractID: a.ContractID,
		Ledger:     a.Ledger,
		TxHash:     a.TxHash,
		OpIndex:    a.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventAdminChangeStarted,
		Attributes: map[string]any{
			"new_admin": a.NewAdmin,
			"old_admin": a.OldAdmin,
		},
	}
}

func eventFromAttesterEnabled(a AttesterEnabled, observedAt time.Time) Event {
	return Event{
		ContractID: a.ContractID,
		Ledger:     a.Ledger,
		TxHash:     a.TxHash,
		OpIndex:    a.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventAttesterEnabled,
		Attributes: map[string]any{
			"attester": a.Attester,
		},
	}
}

func eventFromAttesterManagerUpdated(a AttesterManagerUpdated, observedAt time.Time) Event {
	return Event{
		ContractID: a.ContractID,
		Ledger:     a.Ledger,
		TxHash:     a.TxHash,
		OpIndex:    a.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventAttesterManagerUpdated,
		Attributes: map[string]any{
			"old_attester_manager": a.OldAttesterManager,
			"new_attester_manager": a.NewAttesterManager,
		},
	}
}

func eventFromDenylisted(d Denylisted, observedAt time.Time) Event {
	return Event{
		ContractID: d.ContractID,
		Ledger:     d.Ledger,
		TxHash:     d.TxHash,
		OpIndex:    d.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventDenylisted,
		Attributes: map[string]any{
			"account": d.Account,
		},
	}
}

func eventFromUnDenylisted(u UnDenylisted, observedAt time.Time) Event {
	return Event{
		ContractID: u.ContractID,
		Ledger:     u.Ledger,
		TxHash:     u.TxHash,
		OpIndex:    u.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventUnDenylisted,
		Attributes: map[string]any{
			"account": u.Account,
		},
	}
}

func eventFromDenylisterChanged(d DenylisterChanged, observedAt time.Time) Event {
	return Event{
		ContractID: d.ContractID,
		Ledger:     d.Ledger,
		TxHash:     d.TxHash,
		OpIndex:    d.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventDenylisterChanged,
		Attributes: map[string]any{
			"old_denylister": d.OldDenylister,
			"new_denylister": d.NewDenylister,
		},
	}
}

func eventFromFeeRecipientSet(f FeeRecipientSet, observedAt time.Time) Event {
	return Event{
		ContractID: f.ContractID,
		Ledger:     f.Ledger,
		TxHash:     f.TxHash,
		OpIndex:    f.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventFeeRecipientSet,
		Attributes: map[string]any{
			"fee_recipient": f.FeeRecipient,
		},
	}
}

func eventFromMaxMessageBodySizeUpdated(m MaxMessageBodySizeUpdated, observedAt time.Time) Event {
	return Event{
		ContractID: m.ContractID,
		Ledger:     m.Ledger,
		TxHash:     m.TxHash,
		OpIndex:    m.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventMaxMessageBodySizeUpdated,
		Attributes: map[string]any{
			"new_max_message_body_size": m.NewMaxMessageBodySize,
		},
	}
}

func eventFromMinFeeControllerSet(m MinFeeControllerSet, observedAt time.Time) Event {
	return Event{
		ContractID: m.ContractID,
		Ledger:     m.Ledger,
		TxHash:     m.TxHash,
		OpIndex:    m.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventMinFeeControllerSet,
		Attributes: map[string]any{
			"min_fee_controller": m.MinFeeController,
		},
	}
}

func eventFromPauserChanged(p PauserChanged, observedAt time.Time) Event {
	return Event{
		ContractID: p.ContractID,
		Ledger:     p.Ledger,
		TxHash:     p.TxHash,
		OpIndex:    p.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventPauserChanged,
		Attributes: map[string]any{
			"new_address": p.NewAddress,
		},
	}
}

func eventFromRescuerChanged(r RescuerChanged, observedAt time.Time) Event {
	return Event{
		ContractID: r.ContractID,
		Ledger:     r.Ledger,
		TxHash:     r.TxHash,
		OpIndex:    r.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventRescuerChanged,
		Attributes: map[string]any{
			"new_rescuer": r.NewRescuer,
		},
	}
}

func eventFromSetTokenController(s SetTokenController, observedAt time.Time) Event {
	return Event{
		ContractID: s.ContractID,
		Ledger:     s.Ledger,
		TxHash:     s.TxHash,
		OpIndex:    s.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventSetTokenController,
		Attributes: map[string]any{
			"token_controller": s.TokenController,
		},
	}
}

func eventFromSignatureThresholdUpdated(s SignatureThresholdUpdated, observedAt time.Time) Event {
	return Event{
		ContractID: s.ContractID,
		Ledger:     s.Ledger,
		TxHash:     s.TxHash,
		OpIndex:    s.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventSignatureThresholdUpdated,
		Attributes: map[string]any{
			"new_signature_threshold": s.NewSignatureThreshold,
			"old_signature_threshold": s.OldSignatureThreshold,
		},
	}
}

// eventFromSetBurnLimitPerMessage projects a decoded
// SetBurnLimitPerMessage. `token` promotes to Token (genuine Stellar
// SAC address); `burn_limit_per_message` is a POLICY CEILING, not a
// transfer amount, so it stays in Attributes rather than promoting to
// Event.Amount (Amount is reserved for movement amounts per the
// DepositForBurn/MintAndWithdraw convention).
func eventFromSetBurnLimitPerMessage(s SetBurnLimitPerMessage, observedAt time.Time) Event {
	return Event{
		ContractID: s.ContractID,
		Ledger:     s.Ledger,
		TxHash:     s.TxHash,
		OpIndex:    s.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventSetBurnLimitPerMessage,
		Token:      s.Token,
		Attributes: map[string]any{
			"burn_limit_per_message": s.BurnLimitPerMessage,
		},
	}
}

// eventFromSwapMinterConfigSet projects a decoded SwapMinterConfigSet.
// `token` promotes to Token; the nested config's allow_asset/
// swap_minter land flattened in Attributes.
func eventFromSwapMinterConfigSet(s SwapMinterConfigSet, observedAt time.Time) Event {
	return Event{
		ContractID: s.ContractID,
		Ledger:     s.Ledger,
		TxHash:     s.TxHash,
		OpIndex:    s.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventSwapMinterConfigSet,
		Token:      s.Token,
		Attributes: map[string]any{
			"allow_asset": s.AllowAsset,
			"swap_minter": s.SwapMinter,
		},
	}
}

// eventFromTokenDecimalConfigAdded projects a decoded
// TokenDecimalConfigAdded. `token` promotes to Token; the nested
// config's canonical_decimals/local_decimals land flattened in
// Attributes.
func eventFromTokenDecimalConfigAdded(t TokenDecimalConfigAdded, observedAt time.Time) Event {
	return Event{
		ContractID: t.ContractID,
		Ledger:     t.Ledger,
		TxHash:     t.TxHash,
		OpIndex:    t.OpIndex,
		ObservedAt: observedAt,
		EventType:  EventTokenDecimalConfigAdded,
		Token:      t.Token,
		Attributes: map[string]any{
			"canonical_decimals": t.CanonicalDecimals,
			"local_decimals":     t.LocalDecimals,
		},
	}
}
