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

// Observer is the dispatcher-facing ClaimableBalanceEntry
// observer per ADR-0022 PR 3/5. Implements
// [dispatcher.LedgerEntryChangeDecoder].
//
// Watched-asset driven via the same operator config the
// trustlines observer (#304) consumes. Removed-variant changes
// (a claim) are resolved through the pre-image STATE change that
// stellar-core emits immediately before them — see package doc.
type Observer struct {
	watched map[string]struct{}

	// mu guards preImage + preImageLedger. [dispatcher.Dispatcher]
	// documents that ProcessLedger must be serialized by the caller,
	// so this lock is uncontended in production — it is here so a
	// future concurrent caller degrades to "pairing may miss a
	// removal" rather than to a data race.
	mu sync.Mutex

	// preImage maps claimable_id hex → asset_key for watched claimable
	// balances whose entry body was seen earlier in THIS ledger's
	// entry-change walk (a STATE / Created / Updated / Restored change).
	//
	// It exists because the LedgerKey on a Removed change carries only
	// the BalanceId — not the asset — so a claim cannot be attributed
	// to a watched asset from the Removed change alone. stellar-core
	// always emits the full pre-image as a LEDGER_ENTRY_STATE change
	// immediately before the LEDGER_ENTRY_REMOVED for the same entry
	// (LedgerTxn::getChanges; the SDK's
	// ingest.GetChangesFromLedgerEntryChanges relies on the same
	// adjacency), so the asset is always in hand by the time the
	// removal arrives.
	//
	// Scoped to one ledger: reset whenever a change from a different
	// ledger arrives, which bounds the map by the count of watched-asset
	// claimable changes in a single ledger (single/double digits) and
	// keeps the memo from outliving the walk it belongs to.
	preImage       map[string]string
	preImageLedger uint32
}

var ErrEmptyWatchSet = errors.New("claimable_balances: cannot construct Observer with empty watched-asset list")

// NewObserver constructs an [Observer] watching the supplied
// asset_key list.
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

// Matches implements [dispatcher.LedgerEntryChangeDecoder].
//
// For body-carrying variants (State / Created / Updated / Restored)
// it returns true when:
//
//  1. The change touches a ClaimableBalance entry, AND
//  2. The claimable's asset is a classic credit, AND
//  3. The asset_key is in the watched set.
//
// For Removed variants (a claim) it returns true only when this
// ledger's pre-image memo already knows the balance — i.e. the
// paired STATE change identified it as a watched classic asset.
// A removal we cannot attribute to a watched asset is left
// unmatched, exactly as before.
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

// Decode implements [dispatcher.LedgerEntryChangeDecoder].
//
// A Removed change emits a removal Observation (Balance zero,
// IsRemoval true) so the served aggregate stops counting a claimed
// balance — same shape the trustlines observer uses. The removal is
// an absorbing STATE, not a delta, so re-ingesting a ledger rewrites
// the identical row rather than double-subtracting.
//
// A State change carries only the PRE-image; it feeds the memo and
// emits nothing (emitting would persist a stale balance).
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		id, ok := removedClaimableID(ctx.Change)
		if !ok {
			return nil, fmt.Errorf("%w: removed change carries no claimable LedgerKey", ErrNotClaimable)
		}
		ak, known := o.lookupPreImage(ctx.Ledger, id)
		if !known {
			// Unwatched asset, or the pre-image STATE never reached us.
			// Nothing attributable — skip rather than guess an asset.
			return nil, nil
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

// lookupPreImage returns the watched asset_key recorded for
// `claimableID` earlier in THIS ledger's walk. A memo left over from
// a previous ledger never answers: stellar-core emits the pre-image
// STATE in the same change list as the REMOVED it belongs to (the SDK's
// reader asserts that adjacency outright), so a cross-ledger hit would
// mean the meta broke that invariant — in which case skipping is the
// honest answer rather than attributing a removal from stale state.
func (o *Observer) lookupPreImage(ledger uint32, claimableID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preImageLedger != ledger {
		return "", false
	}
	ak, ok := o.preImage[claimableID]
	return ak, ok
}

// hasPreImage is the Match-time pre-filter: does the memo know this
// claimable at all? Matches gets no [dispatcher.LedgerEntryChangeContext],
// so the ledger-scoped check is Decode's job — per the decoder contract,
// Matches is the cheap filter and Decode is authoritative.
func (o *Observer) hasPreImage(claimableID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.preImage[claimableID]
	return ok
}

// claimableFromChange returns the ClaimableBalanceEntry pointer
// for a State / Created / Updated / Restored change, or
// (nil, false) otherwise. Removed-variant changes return
// (nil, false) — they carry only a LedgerKey, not the entry body
// (see [removedClaimableID]).
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
