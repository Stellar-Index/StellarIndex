package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrContractEntryUnreadable marks a contract_data entry the lake holds archived or undecodable: unreadable, never absent.
var ErrContractEntryUnreadable = errors.New("clickhouse: contract entry unreadable")

// ContractInstance is one CURRENT instance entry: its executable, its storage and the ledger that last modified it, so a
// caller evaluating at an earlier ledger can tell whether this is still the state there.
type ContractInstance struct {
	WasmHash string // hex; empty for a Stellar Asset Contract
	Storage  []xdr.ScMapEntry
	Ledger   uint32
}

// ContractInstance reads a contract's instance entry by exact key (ok=false when the lake holds none), for decoders that
// must see every key the contract wrote and the build that wrote them in one read.
func (r *ExplorerReader) ContractInstance(ctx context.Context, contract string) (ContractInstance, bool, error) {
	id, err := contractIDFromStrkey(contract)
	if err != nil {
		return ContractInstance{}, false, fmt.Errorf("clickhouse: contract id %q: %w", contract, err)
	}
	keys, err := instanceKeyXDR(xdr.Hash(id))
	if err != nil {
		return ContractInstance{}, false, err
	}
	ce, err := r.readContractData(ctx, keys)
	if err != nil {
		return ContractInstance{}, false, err
	}
	for _, k := range keys {
		if ce.archived[k] {
			return ContractInstance{}, false, fmt.Errorf("%w: %s instance", ErrContractEntryUnreadable, contract)
		}
		v, found := ce.vals[k]
		if !found {
			continue
		}
		inst, isInst := v.GetInstance()
		if !isInst {
			return ContractInstance{}, false, fmt.Errorf("%w: %s instance entry holds no instance", ErrContractEntryUnreadable, contract)
		}
		out := ContractInstance{Ledger: ce.ledger[k]}
		if inst.Executable.Type == xdr.ContractExecutableTypeContractExecutableWasm && inst.Executable.WasmHash != nil {
			out.WasmHash = hex.EncodeToString(inst.Executable.WasmHash[:])
		}
		if inst.Storage != nil {
			out.Storage = *inst.Storage
		}
		return out, true, nil
	}
	return ContractInstance{}, false, nil
}

// ContractPersistentEntry reads one CURRENT persistent contract_data value by its exact ScVal key with the ledger that
// last modified it; ok=false when the lake holds no live entry, with ledger the removal ledger if it was removed.
func (r *ExplorerReader) ContractPersistentEntry(ctx context.Context, contract string, key xdr.ScVal) (val xdr.ScVal, ledger uint32, ok bool, err error) {
	id, err := contractIDFromStrkey(contract)
	if err != nil {
		return xdr.ScVal{}, 0, false, fmt.Errorf("clickhouse: contract id %q: %w", contract, err)
	}
	k, err := persistentDataKeyXDR(id, key)
	if err != nil {
		return xdr.ScVal{}, 0, false, err
	}
	ce, err := r.readContractData(ctx, []string{k})
	if err != nil {
		return xdr.ScVal{}, 0, false, err
	}
	if ce.archived[k] {
		return xdr.ScVal{}, 0, false, fmt.Errorf("%w: %s key %s", ErrContractEntryUnreadable, contract, k)
	}
	val, ok = ce.vals[k]
	return val, ce.ledger[k], ok, nil
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
