package sac_balances

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Observer is the dispatcher-facing SAC balance observer (ADR-0022).
// Implements [dispatcher.LedgerEntryChangeDecoder].
//
// Watched-contract driven: the observer's NewObserver takes a
// map of SAC contract IDs → asset_keys. Match runs:
//
//  1. Type discriminator (LedgerEntryTypeContractData)
//  2. Contract scAddress == one of the watched contracts
//  3. Key shape is `Vec(Symbol("Balance"), Address)`
//
// The operator TOML (`[supply.sac_wrappers]`) feeds NewObserver's argument.
type Observer struct {
	// wrappers maps SAC contract C-strkey → asset_key (CODE:ISSUER).
	wrappers map[string]string

	// unknownValShapeDrops counts Balance-keyed changes on a watched contract whose value is
	// neither a bare i128 nor a map-with-amount. Matches() checks only KEY shape, so a
	// pure-SEP-41 wrapper with a different storage layout is claimed on every change and fails
	// every Decode. A sustained non-zero rate means a config problem (undecodable layout), which
	// generic decode errors cannot distinguish from a blip. Read via [Observer.UnknownValShapeDrops].
	unknownValShapeDrops int
}

var (
	ErrEmptyWrapperMap = errors.New("sac_balances: cannot construct Observer with empty SAC wrapper map")
	ErrNotSACBalance   = errors.New("sac_balances: change is not a SAC balance entry")
	// ErrUnknownValShape aliases the shared scval sentinel so callers
	// (and errors.Is) keep matching the balance-value-shape error after
	// the balance decode helpers moved to internal/scval (shared with
	// the lake-seed reader in internal/storage/clickhouse).
	ErrUnknownValShape = scval.ErrUnknownBalanceValShape
)

// NewObserver constructs the SAC observer with the supplied
// contract→asset_key map. Empty input is rejected as a config
// error.
func NewObserver(wrappers map[string]string) (*Observer, error) {
	if len(wrappers) == 0 {
		return nil, ErrEmptyWrapperMap
	}
	cleaned := make(map[string]string, len(wrappers))
	for cid, ak := range wrappers {
		if cid == "" {
			return nil, errors.New("sac_balances: empty SAC contract id in wrapper map")
		}
		if ak == "" {
			return nil, fmt.Errorf("sac_balances: empty asset_key for SAC contract %s", cid)
		}
		// A SAC-wrapped classic asset's key must match the colon-form
		// supply.AssetKey the trustline / claimable / LP observers emit, so a
		// dash-form entry is not silently under-counted. The supply readers
		// resolve through the same helper, so reader and writer agree.
		key, _, err := supply.SACWrapperAssetKey(ak)
		if err != nil {
			return nil, fmt.Errorf("sac_balances: asset_key for SAC contract %s: %w", cid, err)
		}
		cleaned[cid] = key
	}
	return &Observer{wrappers: cleaned}, nil
}

func (*Observer) Name() string { return SourceName }

// Matches implements [dispatcher.LedgerEntryChangeDecoder].
func (o *Observer) Matches(change xdr.LedgerEntryChange) bool {
	cd, _, ok := contractDataFromChange(change)
	if !ok {
		return false
	}
	contractID, ok := scval.ContractIDFromScAddress(cd.Contract)
	if !ok {
		return false
	}
	if _, watched := o.wrappers[contractID]; !watched {
		return false
	}
	return scval.IsSEP41BalanceKey(cd.Key)
}

// Decode emits one Observation per matched change.
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	cd, isRemoval, ok := contractDataFromChange(ctx.Change)
	if !ok {
		return nil, ErrNotSACBalance
	}
	contractID, ok := scval.ContractIDFromScAddress(cd.Contract)
	if !ok {
		return nil, fmt.Errorf("%w: contract scAddress not a contract id", ErrNotSACBalance)
	}
	assetKey, watched := o.wrappers[contractID]
	if !watched {
		return nil, fmt.Errorf("%w: contract %s not in wrapper map", ErrNotSACBalance, contractID)
	}
	holder, err := scval.HolderFromBalanceKey(cd.Key)
	if err != nil {
		return nil, err
	}
	if isRemoval {
		return []consumer.Event{Observation{
			ContractID:     contractID,
			AssetKey:       assetKey,
			Holder:         holder,
			Ledger:         ctx.Ledger,
			ObservedAt:     ctx.ClosedAt,
			Balance:        big.NewInt(0),
			IsRemoval:      true,
			IntraLedgerSeq: ctx.IntraLedgerSeq,
		}}, nil
	}
	balance, err := scval.SEP41BalanceAmount(cd.Val)
	if err != nil {
		if errors.Is(err, scval.ErrUnknownBalanceValShape) {
			o.unknownValShapeDrops++
		}
		return nil, err
	}
	return []consumer.Event{Observation{
		ContractID:     contractID,
		AssetKey:       assetKey,
		Holder:         holder,
		Ledger:         ctx.Ledger,
		ObservedAt:     ctx.ClosedAt,
		Balance:        balance,
		IntraLedgerSeq: ctx.IntraLedgerSeq,
	}}, nil
}

// UnknownValShapeDrops is the count of Balance-keyed changes for a watched
// contract whose value shape [scval.SEP41BalanceAmount] could not decode.
// See the field doc on [Observer.unknownValShapeDrops].
func (o *Observer) UnknownValShapeDrops() int { return o.unknownValShapeDrops }

// contractDataFromChange returns the ContractDataEntry and is-removal flag for any change
// variant. Removed rows derive ContractData from the LedgerKey (Contract + Key, no Val).
//
// Both halves of the Soroban archival lifecycle arrive here. Eviction is not a change variant:
// the dispatcher turns each evicted key into a Removed change (dispatcher.walkEvictedKeys) with
// an empty TxHash and OpIndex -1, since it is ledger-scoped. An evicted balance must stop
// counting toward supply exactly as a deleted one does, and a later Restored change reverses it.
func contractDataFromChange(change xdr.LedgerEntryChange) (cd *xdr.ContractDataEntry, isRemoval bool, ok bool) {
	var entry *xdr.LedgerEntry
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry = change.Created
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry = change.Updated
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry = change.Restored
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		k := change.Removed
		if k == nil || k.Type != xdr.LedgerEntryTypeContractData || k.ContractData == nil {
			return nil, false, false
		}
		// Removed: synthesize a ContractDataEntry from the LedgerKey
		// fields (no Val available). Caller checks the isRemoval
		// flag and skips Val parsing.
		return &xdr.ContractDataEntry{
			Contract:   k.ContractData.Contract,
			Key:        k.ContractData.Key,
			Durability: k.ContractData.Durability,
		}, true, true
	default:
		return nil, false, false
	}
	if entry == nil || entry.Data.Type != xdr.LedgerEntryTypeContractData {
		return nil, false, false
	}
	return entry.Data.ContractData, false, entry.Data.ContractData != nil
}

// The SEP-41 / SAC balance decode helpers live in internal/scval (ContractIDFromScAddress,
// IsSEP41BalanceKey, HolderFromBalanceKey, SEP41BalanceAmount), shared with the lake-seed reader
// in internal/storage/clickhouse so the live observer and the seed decode the same on-wire shape.

var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
