package upshift

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify returns the event kind for a topic[0] this protocol emits,
// or "" for anything else. Byte-equality against the pre-encoded
// Symbols — no SCVal parse on the reject path.
//
// It enumerates EVERY symbol either vault has ever emitted (the census
// in the package doc), not just the four that produce rows: the
// EVERY-event policy means a recognized-but-undecoded event is counted
// as expected-zero by the ADR-0033 re-derive instead of leaving its
// ledger blind. A symbol NOT in this list is a schema change we do not
// claim — Matches returns false and the recognition audit surfaces it.
//
// classify is deliberately topic-only. It is ROUTING, not attribution:
// the contract-identity gate lives in [Decoder.Matches], which the
// dispatcher consults first (ADR-0035).
func classify(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolTransfer:
		return EventTransfer
	case TopicSymbolDeployedAssetsChanged:
		return EventDeployedAssetsChanged
	case TopicSymbolApprove:
		return EventApprove
	case TopicSymbolDepositToSubaccount:
		return EventDepositToSubaccount
	case TopicSymbolWithdrawFromSub:
		return EventWithdrawFromSubaccount
	case TopicSymbolWalletDeployedUpd:
		return EventWalletDeployedUpdated
	case TopicSymbolWalletNetDeplSeeded:
		return EventWalletNetDeployedSeeded
	case TopicSymbolSubaccountAdded:
		return EventSubaccountAdded
	case TopicSymbolAdminSet:
		return EventAdminSet
	case TopicSymbolOperatorSet:
		return EventOperatorSet
	}
	return ""
}

// newEvent seeds the identity columns every decoded kind shares.
func newEvent(e *events.Event, kind string) (Event, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		// Fail closed rather than substituting time.Now(): during a
		// backfill or a completeness re-derive that would stamp every
		// row with the wall clock of the replay run instead of the
		// historical ledger. Same stance as the blend / comet / defindex
		// siblings.
		return Event{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	return Event{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		ObservedAt: closedAt,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // non-negative by Soroban spec.
		Kind:       kind,
	}, nil
}

// decodeAddrTopic strkey-decodes topic[i]. Delegates the address-type
// switch (G / C / M / B / L) to scval so a CAP-67 muxed or
// liquidity-pool destination decodes rather than being dropped.
func decodeAddrTopic(e *events.Event, i int, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[i])
	if err != nil {
		return "", fmt.Errorf("%w: parse topic[%d] (%s): %w", ErrMalformedPayload, i, field, err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return "", fmt.Errorf("%w: topic[%d] (%s): %w", ErrMalformedPayload, i, field, err)
	}
	return addr, nil
}

// bodyI128Fields pulls the named i128 fields out of a Map body, in the
// order asked for.
//
// Fields are read BY NAME, never by position, per
// docs/architecture/contract-schema-evolution.md — a contract upgrade
// that appends a field must stay readable, and `deployed_assets_changed`
// puts `new_amount` FIRST on the wire despite `old` being the logically
// prior value, so positional decoding would silently swap them.
//
// The map entries never cross a function boundary on purpose: naming
// their type would pull the raw XDR package into a Soroban event
// decoder, which ADR-0013 scopes to internal/scval.
func bodyI128Fields(e *events.Event, kind string, fields ...string) ([]canonical.Amount, error) {
	sv, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s body: %w", ErrMalformedPayload, kind, err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, fmt.Errorf("%w: %s body is not a Map: %w", ErrMalformedPayload, kind, err)
	}
	out := make([]canonical.Amount, 0, len(fields))
	for _, field := range fields {
		fv, ferr := scval.MustMapField(entries, field)
		if ferr != nil {
			return nil, fmt.Errorf("%w: %s.%s: %w", ErrMalformedPayload, kind, field, ferr)
		}
		amt, aerr := scval.AsAmountFromI128(fv)
		if aerr != nil {
			return nil, fmt.Errorf("%w: %s.%s: %w", ErrMalformedPayload, kind, field, aerr)
		}
		out = append(out, amt)
	}
	return out, nil
}

// decodeFlow decodes a `deposit` or `withdraw`.
//
//	topics [Symbol, caller Address, receiver Address, owner Address]
//	body   Map{ assets: i128, shares: i128 }
//
// The (caller, receiver, owner) reading is the OpenZeppelin ERC-4626
// `Withdraw` ordering; see the package doc for the two lake events that
// prove it (one per vault, 21 ledgers apart, same holder in the middle
// slot) and for the caveat that `deposit` never exercised it.
func decodeFlow(e *events.Event, kind string) (Event, error) {
	if len(e.Topic) < 4 {
		return Event{}, fmt.Errorf("%w: %s expects 4 topics, got %d", ErrShortTopic, kind, len(e.Topic))
	}
	out, err := newEvent(e, kind)
	if err != nil {
		return Event{}, err
	}
	if out.Caller, err = decodeAddrTopic(e, 1, "caller"); err != nil {
		return Event{}, err
	}
	if out.Receiver, err = decodeAddrTopic(e, 2, "receiver"); err != nil {
		return Event{}, err
	}
	if out.Owner, err = decodeAddrTopic(e, 3, "owner"); err != nil {
		return Event{}, err
	}

	amounts, err := bodyI128Fields(e, kind, "assets", "shares")
	if err != nil {
		return Event{}, err
	}
	out.Assets, out.Shares = amounts[0], amounts[1]
	return out, nil
}

// decodeTransfer decodes a share-token `transfer`.
//
//	topics [Symbol, from Address, to Address]
//	body   i128  OR  Map{ amount: i128, to_muxed_id: Address|Void }
//
// BOTH body forms are accepted: the bare i128 is the original SEP-41
// shape and the Map is the CAP-67 / Protocol-23 extension. Every
// transfer either vault has emitted so far carries the Map form with a
// Void `to_muxed_id`, but pinning only that shape would break on any
// pre-CAP-67 replay.
//
// The bare form is tried first and the Map is the fallback, taken ONLY
// on a type mismatch — any other failure of the i128 read is a real
// error and is returned as one. (Discriminating on sv.Type directly
// would be more direct but would name raw XDR type constants inside a
// Soroban event decoder, which ADR-0013 scopes to internal/scval.)
//
// `to_muxed_id` is not stored: the muxed destination is a routing detail
// of the recipient, not a vault-economics fact, and
// internal/sources/sep41_transfers is the audit-trail surface that
// carries the full SEP-41 shape.
func decodeTransfer(e *events.Event) (Event, error) {
	if len(e.Topic) < 3 {
		return Event{}, fmt.Errorf("%w: transfer expects 3 topics, got %d", ErrShortTopic, len(e.Topic))
	}
	out, err := newEvent(e, EventTransfer)
	if err != nil {
		return Event{}, err
	}
	if out.Caller, err = decodeAddrTopic(e, 1, "from"); err != nil {
		return Event{}, err
	}
	if out.Receiver, err = decodeAddrTopic(e, 2, "to"); err != nil {
		return Event{}, err
	}

	sv, err := scval.Parse(e.Value)
	if err != nil {
		return Event{}, fmt.Errorf("%w: parse transfer body: %w", ErrMalformedPayload, err)
	}
	amt, ierr := scval.AsAmountFromI128(sv)
	if ierr == nil {
		out.Shares = amt
		return out, nil
	}
	if !errors.Is(ierr, scval.ErrScValType) {
		return Event{}, fmt.Errorf("%w: transfer.amount: %w", ErrMalformedPayload, ierr)
	}
	amounts, merr := bodyI128Fields(e, EventTransfer, "amount")
	if merr != nil {
		return Event{}, fmt.Errorf("%w: transfer body is neither an i128 nor a Map carrying `amount`: %w",
			ErrMalformedPayload, merr)
	}
	out.Shares = amounts[0]
	return out, nil
}

// decodeDeployedAssets decodes a `deployed_assets_changed`.
//
//	topics [Symbol, operator Address]
//	body   Map{ old_amount: i128, new_amount: i128 }
//
// This is the vault-level total of capital DEPLOYED into strategies,
// before and after the change. It is emitted by the operator, and in
// both vaults' history the operator address is a single account. The
// idle (undeployed) balance is NOT reported by any event, so these two
// numbers are one leg of the vault's assets and never the whole — see
// [Event.OldAmount].
func decodeDeployedAssets(e *events.Event) (Event, error) {
	if len(e.Topic) < 2 {
		return Event{}, fmt.Errorf("%w: %s expects 2 topics, got %d",
			ErrShortTopic, EventDeployedAssetsChanged, len(e.Topic))
	}
	out, err := newEvent(e, EventDeployedAssetsChanged)
	if err != nil {
		return Event{}, err
	}
	if out.Caller, err = decodeAddrTopic(e, 1, "operator"); err != nil {
		return Event{}, err
	}

	amounts, err := bodyI128Fields(e, EventDeployedAssetsChanged, "old_amount", "new_amount")
	if err != nil {
		return Event{}, err
	}
	out.OldAmount, out.NewAmount = amounts[0], amounts[1]
	return out, nil
}
