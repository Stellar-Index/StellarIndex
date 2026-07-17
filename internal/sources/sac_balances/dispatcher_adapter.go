package sac_balances

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Observer is the dispatcher-facing SAC balance observer per
// ADR-0022 PR 4/5. Implements
// [dispatcher.LedgerEntryChangeDecoder].
//
// Watched-contract driven: the observer's NewObserver takes a
// map of SAC contract IDs → asset_keys. Match runs:
//
//  1. Type discriminator (LedgerEntryTypeContractData)
//  2. Contract scAddress == one of the watched contracts
//  3. Key shape is `Vec(Symbol("Balance"), Address)`
//
// PR 5/5 wires the operator TOML
// (`[supply.sac_wrappers]`) into NewObserver's argument.
type Observer struct {
	// wrappers maps SAC contract C-strkey → asset_key (CODE:ISSUER).
	wrappers map[string]string
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
		cleaned[cid] = ak
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

// contractDataFromChange returns the ContractDataEntry +
// is-removal flag for any change variant. Removed-variant rows
// derive ContractData from the LedgerKey (which carries Contract
// + Key but no Val).
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

// The SEP-41 / SAC balance decode helpers this observer used to define
// locally — contractID-from-scAddress, the Balance(Address) key
// predicate, the holder extractor, and the i128-or-map value decoder —
// now live in internal/scval (scval.ContractIDFromScAddress,
// IsSEP41BalanceKey, HolderFromBalanceKey, SEP41BalanceAmount). They are
// shared verbatim with the lake-seed reader in internal/storage/
// clickhouse so the live observer and the seed recognise + decode
// exactly the same on-wire shape.

var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
