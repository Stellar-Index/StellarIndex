package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestCachedContractStorageSupply_CachesReadingsAndDeclinesNeverFailures(t *testing.T) {
	ctx := context.Background()

	reading := &fakeStorageSupply{out: clickhouse.ContractStorageSupply{Total: big.NewInt(500), BalanceEntries: 3}}
	c := NewCachedContractStorageSupplyReader(reading)
	first, err := c.ContractStorageSupply(ctx, supplyContractID)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	first.Total.SetInt64(1) // a caller's mutation must not reach the cached entry
	second, err := c.ContractStorageSupply(ctx, supplyContractID)
	if err != nil || second.Total.Int64() != 500 || reading.calls != 1 {
		t.Errorf("second read: total=%v err=%v upstream calls=%d, want 500, nil, 1", second.Total, err, reading.calls)
	}

	declined := &fakeStorageSupply{err: clickhouse.ErrStorageSupplyIsStellarAsset}
	c = NewCachedContractStorageSupplyReader(declined)
	for range 2 {
		if _, err := c.ContractStorageSupply(ctx, supplyContractID); !errors.Is(err, clickhouse.ErrStorageSupplyIsStellarAsset) {
			t.Fatalf("decline: err = %v, want ErrStorageSupplyIsStellarAsset", err)
		}
	}
	if declined.calls != 1 {
		t.Errorf("decline: upstream calls = %d, want 1 (a decline is cached)", declined.calls)
	}

	failing := &fakeStorageSupply{err: errors.New("clickhouse down")}
	c = NewCachedContractStorageSupplyReader(failing)
	for range 2 {
		if _, err := c.ContractStorageSupply(ctx, supplyContractID); err == nil {
			t.Fatal("failure: err = nil, want the read error")
		}
	}
	if failing.calls != 2 {
		t.Errorf("failure: upstream calls = %d, want 2 (a failure is never cached)", failing.calls)
	}
}
