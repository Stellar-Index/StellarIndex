package claimable_balances

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/supply"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Observer is the dispatcher-facing ClaimableBalanceEntry observer (ADR-0022), a [dispatcher.LedgerEntryChangeDecoder] driven by the same watched-asset config as the trustlines observer; removals (claims) are resolved through the pre-image STATE change stellar-core emits just before them.
type Observer struct {
	watched map[string]struct{}

	// mu guards preImage + preImageLedger. ProcessLedger is serialized by the caller, so this is
	// uncontended; it makes a concurrent caller miss a pairing rather than race.
	mu sync.Mutex

	// preImage maps claimable_id hex → asset_key for watched claimable
	// balances whose entry body was seen earlier in THIS ledger's entry-change walk.
	//
	// A Removed change's LedgerKey carries only the BalanceId, not the asset. stellar-core always
	// emits the full pre-image as a LEDGER_ENTRY_STATE change immediately before the REMOVED for
	// the same entry (the SDK's change reader relies on the same adjacency), so the asset is
	// known by the time the removal arrives.
	//
	// Scoped to one ledger: reset when a change from a different ledger arrives, which bounds the
	// map and keeps the memo from outliving its walk.
	preImage       map[string]string
	preImageLedger uint32
}

var ErrEmptyWatchSet = errors.New("claimable_balances: cannot construct Observer with empty watched-asset list")

// NewObserver constructs an [Observer] watching the supplied asset_key list.
func NewObserver(watched []string) (*Observer, error) {
	if len(watched) == 0 {
		return nil, ErrEmptyWatchSet
	}
	watched, err := supply.CanonicalizeWatchedClassic(watched)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(watched))
	for _, k := range watched {
		if k == "" {
			return nil, errors.New("claimable_balances: empty asset_key in watched list")
		}
		set[k] = struct{}{}
	}
	return &Observer{watched: set}, nil
}

func (*Observer) Name() string { return SourceName }

// Matches implements [dispatcher.LedgerEntryChangeDecoder]: a body-carrying variant matches when the entry is a ClaimableBalance with a classic credit asset in the watched set; a Removed variant only when the pre-image memo already knows the balance, so an unattributable removal stays unmatched.
func (o *Observer) Matches(change xdr.LedgerEntryChange) bool {
	if change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		id, ok := removedClaimableID(change)
		if !ok {
			return false
		}
		return o.hasPreImage(id)
	}
	cb, ok := claimableFromChange(change)
	if !ok {
		return false
	}
	asset := cb.Asset
	if asset.Type != xdr.AssetTypeAssetTypeCreditAlphanum4 &&
		asset.Type != xdr.AssetTypeAssetTypeCreditAlphanum12 {
		return false
	}
	ak, err := assetKeyFromAsset(asset)
	if err != nil {
		return false
	}
	_, watched := o.watched[ak]
	return watched
}

// Decode implements [dispatcher.LedgerEntryChangeDecoder]. A Removed change emits a removal Observation (Balance zero, IsRemoval true) so the served aggregate stops counting a claimed balance; removal is an absorbing state, not a delta, so re-ingest rewrites the identical row. A State change carries only the pre-image: it feeds the memo and emits nothing, since that would persist a stale balance.
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		id, ok := removedClaimableID(ctx.Change)
		if !ok {
			return nil, fmt.Errorf("%w: removed change carries no claimable LedgerKey", ErrNotClaimable)
		}
		ak, known := o.lookupPreImage(ctx.Ledger, id)
		if !known {
			// hasPreImage does not apply the ledger guard lookupPreImage does, so reaching here
			// means the memo entry is from a PREVIOUS ledger; we must not guess an asset.
			//
			// Error rather than (nil, nil): a dropped removal is the only thing that stops
			// SumClaimableBalancesAtOrBefore counting a claimed balance, so it would over-report
			// supply forever with no counter moving. The error bumps the decode-error counter.
			return nil, fmt.Errorf(
				"%w: removed claimable %s has no pre-image in ledger %d's memo — "+
					"a watched balance was claimed and cannot be attributed, so its "+
					"amount will stay in the served total",
				ErrNotClaimable, id, ctx.Ledger)
		}
		return []consumer.Event{Observation{
			ClaimableID:    id,
			AssetKey:       ak,
			Ledger:         ctx.Ledger,
			ObservedAt:     ctx.ClosedAt,
			Balance:        big.NewInt(0),
			IsRemoval:      true,
			IntraLedgerSeq: ctx.IntraLedgerSeq,
		}}, nil
	}

	id, ak, balance, err := extractFromChange(ctx.Change)
	if err != nil {
		return nil, err
	}
	// Remember the asset for the removal that may follow in this ledger.
	o.rememberPreImage(ctx.Ledger, id, ak)
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryState {
		return nil, nil
	}
	return []consumer.Event{Observation{
		ClaimableID:    id,
		AssetKey:       ak,
		Ledger:         ctx.Ledger,
		ObservedAt:     ctx.ClosedAt,
		Balance:        balance,
		IntraLedgerSeq: ctx.IntraLedgerSeq,
	}}, nil
}

// rememberPreImage records claimable_id → asset_key for the current
// ledger, resetting the memo when the walk moves to a new ledger.
func (o *Observer) rememberPreImage(ledger uint32, claimableID, assetKey string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preImage == nil || o.preImageLedger != ledger {
		o.preImage = make(map[string]string)
		o.preImageLedger = ledger
	}
	o.preImage[claimableID] = assetKey
}

// lookupPreImage returns the watched asset_key recorded for claimableID earlier in THIS ledger's
// walk. A memo from a previous ledger never answers: the pre-image STATE shares a change list
// with its REMOVED, so a cross-ledger hit means the meta broke that invariant, and skipping beats
// attributing a removal from stale state.
func (o *Observer) lookupPreImage(ledger uint32, claimableID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preImageLedger != ledger {
		return "", false
	}
	ak, ok := o.preImage[claimableID]
	return ak, ok
}

// hasPreImage is the Match-time pre-filter. Matches gets no ledger context, so the ledger-scoped
// check is Decode's job: Matches is the cheap filter, Decode is authoritative.
func (o *Observer) hasPreImage(claimableID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.preImage[claimableID]
	return ok
}

// claimableFromChange returns the ClaimableBalanceEntry for a State / Created / Updated / Restored
// change, or (nil, false). Removed changes carry only a LedgerKey (see [removedClaimableID]).
func claimableFromChange(change xdr.LedgerEntryChange) (*xdr.ClaimableBalanceEntry, bool) {
	var entry *xdr.LedgerEntry
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry = change.Created
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry = change.Updated
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry = change.Restored
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		entry = change.State
	default:
		return nil, false
	}
	if entry == nil || entry.Data.Type != xdr.LedgerEntryTypeClaimableBalance {
		return nil, false
	}
	return entry.Data.ClaimableBalance, entry.Data.ClaimableBalance != nil
}

var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
