package clickhouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrContractEntryUnreadable marks a contract_data entry the lake holds archived or undecodable: unreadable, never absent.
var ErrContractEntryUnreadable = errors.New("clickhouse: contract entry unreadable")

// ContractInstanceStorage reads a contract's CURRENT instance storage by exact key (ok=false when the lake holds no
// instance entry), for decoders that must see every key the contract wrote.
func (r *ExplorerReader) ContractInstanceStorage(ctx context.Context, contract string) (storage []xdr.ScMapEntry, ok bool, err error) {
	id, err := contractIDFromStrkey(contract)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: contract id %q: %w", contract, err)
	}
	keys, err := instanceKeyXDR(xdr.Hash(id))
	if err != nil {
		return nil, false, err
	}
	ce, err := r.readContractData(ctx, keys)
	if err != nil {
		return nil, false, err
	}
	for _, k := range keys {
		if ce.archived[k] {
			return nil, false, fmt.Errorf("%w: %s instance", ErrContractEntryUnreadable, contract)
		}
		v, found := ce.vals[k]
		if !found {
			continue
		}
		inst, isInst := v.GetInstance()
		if !isInst {
			return nil, false, fmt.Errorf("%w: %s instance entry holds no instance", ErrContractEntryUnreadable, contract)
		}
		if inst.Storage == nil {
			return nil, true, nil
		}
		return *inst.Storage, true, nil
	}
	return nil, false, nil
}

// ContractPersistentEntry reads one CURRENT persistent contract_data value by its exact ScVal key; ok=false when the
// lake holds no live entry under it.
func (r *ExplorerReader) ContractPersistentEntry(ctx context.Context, contract string, key xdr.ScVal) (val xdr.ScVal, ok bool, err error) {
	id, err := contractIDFromStrkey(contract)
	if err != nil {
		return xdr.ScVal{}, false, fmt.Errorf("clickhouse: contract id %q: %w", contract, err)
	}
	k, err := persistentDataKeyXDR(id, key)
	if err != nil {
		return xdr.ScVal{}, false, err
	}
	ce, err := r.readContractData(ctx, []string{k})
	if err != nil {
		return xdr.ScVal{}, false, err
	}
	if ce.archived[k] {
		return xdr.ScVal{}, false, fmt.Errorf("%w: %s key %s", ErrContractEntryUnreadable, contract, k)
	}
	val, ok = ce.vals[k]
	return val, ok, nil
}

// persistentDataKeyXDR builds the base64 LedgerKey of a persistent contract_data entry with an arbitrary ScVal key.
func persistentDataKeyXDR(contract xdr.ContractId, key xdr.ScVal) (string, error) {
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	return xdr.MarshalBase64(lk)
}
