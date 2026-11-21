package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// storageSupplyResult is one cached storage reading: a reading, or a refusal the lake state fixes ([storageSupplyDeclined],
// [storageSupplyUnchecked]).
type storageSupplyResult struct {
	st       clickhouse.ContractStorageSupply
	declined error
}

// CachedContractStorageSupplyReader puts a per-contract TTL cache with single-flight in front of the storage scan, which the
// supply route, every Soroban asset detail and every RWA contract row pay. Readings and declines are cached; failures never are.
type CachedContractStorageSupplyReader struct {
	upstream ContractStorageSupplyReader
	cache    *swrCache[storageSupplyResult]
}

// NewCachedContractStorageSupplyReader wraps upstream with the token-supply cache's TTL and bound.
func NewCachedContractStorageSupplyReader(upstream ContractStorageSupplyReader) *CachedContractStorageSupplyReader {
	return &CachedContractStorageSupplyReader{
		upstream: upstream,
		cache: newSWRCache[storageSupplyResult](nil, "contract_storage_supply", "api-contract-storage-supply-fill",
			tokenSupplyTTL, tokenSupplyCacheMax),
	}
}

// ContractStorageSupply serves the cached reading or decline; every return is a copy, so no caller can mutate another's amounts.
func (c *CachedContractStorageSupplyReader) ContractStorageSupply(ctx context.Context, contractID string) (clickhouse.ContractStorageSupply, error) {
	res, _, _, err := c.cache.get(ctx, "contract_storage_supply", contractID, func(ctx context.Context) (storageSupplyResult, error) {
		st, err := c.upstream.ContractStorageSupply(ctx, contractID)
		if err != nil && !storageSupplyDeclined(err) && !storageSupplyUnchecked(err) {
			return storageSupplyResult{}, err
		}
		return storageSupplyResult{st: st, declined: err}, nil
	})
	if err != nil {
		return clickhouse.ContractStorageSupply{}, err
	}
	if res.declined != nil {
		return clickhouse.ContractStorageSupply{}, res.declined
	}
	st := res.st
	st.Total, st.DeclaredTotal, st.ArchivedTotal = copyBig(st.Total), copyBig(st.DeclaredTotal), copyBig(st.ArchivedTotal)
	if st.DeclaredHolders != nil {
		h := *st.DeclaredHolders
		st.DeclaredHolders = &h
	}
	return st, nil
}
