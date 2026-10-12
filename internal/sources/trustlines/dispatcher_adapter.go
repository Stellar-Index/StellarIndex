package trustlines

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/supply"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Observer is the dispatcher-facing TrustlineEntry observer (ADR-0022), a [dispatcher.LedgerEntryChangeDecoder] firing only on trustlines whose asset_key is in the operator-configured watched set; native and pool-share variants are skipped in Match.
type Observer struct {
	// watched maps asset_key (CODE:ISSUER) → struct{}{}. Map
	// lookup is O(1) per change; the watched set is bounded by
	// operator config, typically single-digit assets at v1.
	watched map[string]struct{}
}

// ErrEmptyWatchSet is returned by [NewObserver] for an empty list: an observer with nothing to watch is a configuration error; operators who don't want it should not register it.
var ErrEmptyWatchSet = errors.New("trustlines: cannot construct Observer with empty watched-asset list")

// NewObserver constructs an [Observer] watching the asset_key list; keys are deduplicated and empty strings rejected as a configuration error.
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
			return nil, errors.New("trustlines: empty asset_key in watched list")
		}
		set[k] = struct{}{}
	}
	return &Observer{watched: set}, nil
}

// Name implements [dispatcher.LedgerEntryChangeDecoder].
func (*Observer) Name() string { return SourceName }

// Matches implements [dispatcher.LedgerEntryChangeDecoder]: true when the change touches a TrustlineEntry for a classic credit (alphanum4/12) whose asset_key is watched; bounded work, no balance parsing.
func (o *Observer) Matches(change xdr.LedgerEntryChange) bool {
	if !changeIsTrustline(change) {
		return false
	}
	asset, ok := trustlineAssetFromChange(change)
	if !ok {
		return false
	}
	if !isClassicCreditAsset(asset.Type) {
		return false
	}
	ak, err := assetKeyFromTrustLineAsset(asset)
	if err != nil {
		return false
	}
	_, watched := o.watched[ak]
	return watched
}

// Decode implements [dispatcher.LedgerEntryChangeDecoder], emitting exactly one [Observation] per matched change.
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	accountID, assetKey, balance, isRemoval, err := extractTrustlineFromChange(ctx.Change)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{Observation{
		AccountID:      accountID,
		AssetKey:       assetKey,
		Ledger:         ctx.Ledger,
		ObservedAt:     ctx.ClosedAt,
		Balance:        balance,
		IsRemoval:      isRemoval,
		IntraLedgerSeq: ctx.IntraLedgerSeq,
	}}, nil
}

// changeIsTrustline reports whether the change's data type is
// Trustline, regardless of the change variant (Created /
// Updated / Restored / Removed). Cheap pre-filter.
func changeIsTrustline(change xdr.LedgerEntryChange) bool {
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return change.Created != nil && change.Created.Data.Type == xdr.LedgerEntryTypeTrustline
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return change.Updated != nil && change.Updated.Data.Type == xdr.LedgerEntryTypeTrustline
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return change.Restored != nil && change.Restored.Data.Type == xdr.LedgerEntryTypeTrustline
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		return change.Removed != nil && change.Removed.Type == xdr.LedgerEntryTypeTrustline
	}
	return false
}

// trustlineAssetFromChange extracts the TrustLineAsset from a
// change already known to touch a Trustline entry.
func trustlineAssetFromChange(change xdr.LedgerEntryChange) (xdr.TrustLineAsset, bool) {
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return change.Created.Data.TrustLine.Asset, true
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return change.Updated.Data.TrustLine.Asset, true
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return change.Restored.Data.TrustLine.Asset, true
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		return change.Removed.TrustLine.Asset, true
	}
	return xdr.TrustLineAsset{}, false
}

func isClassicCreditAsset(t xdr.AssetType) bool {
	return t == xdr.AssetTypeAssetTypeCreditAlphanum4 ||
		t == xdr.AssetTypeAssetTypeCreditAlphanum12
}

// Compile-time check.
var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
