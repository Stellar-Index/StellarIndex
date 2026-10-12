package upshift

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify returns the event kind for a topic[0] this protocol emits, or "", by byte-equality against pre-encoded Symbols. It
// enumerates EVERY symbol either vault has emitted, so a recognized-but-undecoded event counts as expected-zero in the ADR-0033
// re-derive; an unlisted symbol is a schema change we do not claim. Topic-only routing: the identity gate is [Decoder.Matches] (ADR-0035).
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
	case TopicSymbolVaultPaused:
		return EventVaultPaused
	case TopicSymbolVaultUnpaused:
		return EventVaultUnpaused
	}
	return ""
}

// newEvent seeds the identity columns every decoded kind shares.
func newEvent(e *events.Event, kind string) (Event, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		// Fail closed rather than substitute time.Now(): in a backfill or re-derive that
		// would stamp every row with the replay's wall clock instead of the ledger time.
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

// bodyI128Fields pulls the named i128 fields out of a Map body, in the order asked for. Fields are read BY NAME, never position
// (docs/architecture/ingest-pipeline.md#contract-schema-evolution): `deployed_assets_changed` puts `new_amount` FIRST on the
// wire. Map entries never cross a function boundary: ADR-0013 scopes the raw XDR package to internal/scval.
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

// decodeFlow decodes a `deposit` or `withdraw`: topics [Symbol, caller, receiver, owner], body Map{ assets: i128, shares: i128 }.
// The (caller, receiver, owner) reading is the OpenZeppelin ERC-4626 `Withdraw` ordering; the package doc has the lake evidence.
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

// decodeTransfer decodes a share-token `transfer`: topics [Symbol, from, to], body i128 OR Map{ amount: i128, to_muxed_id }. BOTH
// forms are accepted (bare SEP-41 i128 and CAP-67 Map; pinning the Map would break pre-CAP-67 replay): the bare form is tried
// first, the Map only on a type mismatch, and any other i128 read failure is a real error. `to_muxed_id` is not stored.
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

// decodeDeployedAssets decodes a `deployed_assets_changed`: topics [Symbol, operator], body Map{ old_amount, new_amount: i128 }.
// This is the vault-level capital DEPLOYED into strategies; the idle balance is in no event, so it is one leg of the vault's
// assets, never the whole (see [Event.OldAmount]).
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
