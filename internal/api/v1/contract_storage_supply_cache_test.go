package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

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

// scriptedStorageSupply answers its first read; later reads fail while fail is set and block while hold is open.
type scriptedStorageSupply struct {
	calls atomic.Int32
	fail  atomic.Bool
	hold  chan struct{}
	out   clickhouse.ContractStorageSupply
}

func (s *scriptedStorageSupply) ContractStorageSupply(_ context.Context, id string) (clickhouse.ContractStorageSupply, error) {
	if s.calls.Add(1) > 1 {
		if s.hold != nil {
			<-s.hold
		}
		if s.fail.Load() {
			return clickhouse.ContractStorageSupply{}, errors.New("storage scan exceeded its budget")
		}
	}
	out := s.out
	out.ContractID = id
	return out, nil
}

// filledStorageSupplyCache is a cache holding one reading for key that has sat idle well past TTL and one refresh budget.
func filledStorageSupplyCache(t *testing.T, up *scriptedStorageSupply, key string) *CachedContractStorageSupplyReader {
	t.Helper()
	c := NewCachedContractStorageSupplyReader(up)
	if _, err := c.ContractStorageSupply(context.Background(), key); err != nil {
		t.Fatalf("fill: %v", err)
	}
	c.ExpireForTest(key, tokenSupplyTTL+cacheFillBudget+time.Minute)
	return c
}

func readStorageSupplyStale(t *testing.T, c *CachedContractStorageSupplyReader, key string) bool {
	t.Helper()
	ctx, rep := withReadReport(context.Background())
	st, err := c.ContractStorageSupply(ctx, key)
	if err != nil || st.Total.Int64() != 500 {
		t.Fatalf("read: total=%v err=%v, want 500", st.Total, err)
	}
	return rep.stale.Load()
}

// Staleness is the refresh outcome, not the entry's age: an idle key whose refresh succeeds was never wrong.
func TestCachedContractStorageSupply_StaleIsAFailedOrOverdueRefreshNotAge(t *testing.T) {
	reading := clickhouse.ContractStorageSupply{Total: big.NewInt(500), BalanceEntries: 3}

	t.Run("idle key, refresh succeeds: not stale", func(t *testing.T) {
		c := filledStorageSupplyCache(t, &scriptedStorageSupply{out: reading}, supplyContractID)
		if readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("the first read after idling is stale, though no refresh has failed")
		}
		c.AwaitRefreshForTest(supplyContractID)
		if readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("stale after a refresh that succeeded")
		}
	})

	t.Run("refresh failed: stale until one succeeds", func(t *testing.T) {
		up := &scriptedStorageSupply{out: reading}
		up.fail.Store(true)
		c := NewCachedContractStorageSupplyReader(up)
		if _, err := c.ContractStorageSupply(context.Background(), supplyContractID); err != nil {
			t.Fatalf("fill: %v", err)
		}
		c.ExpireForTest(supplyContractID, tokenSupplyTTL+time.Second)
		readStorageSupplyStale(t, c, supplyContractID)
		c.AwaitRefreshForTest(supplyContractID)
		if !readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("not stale after its refresh failed: a last-good reading is served as current")
		}
		up.fail.Store(false)
		c.AwaitRefreshForTest(supplyContractID) // may have read fail before the Store; the next one cannot
		readStorageSupplyStale(t, c, supplyContractID)
		c.AwaitRefreshForTest(supplyContractID)
		if readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("still stale after a refresh succeeded")
		}
	})

	t.Run("refresh in flight: stale only past its budget", func(t *testing.T) {
		up := &scriptedStorageSupply{out: reading, hold: make(chan struct{})}
		defer close(up.hold)
		c := NewCachedContractStorageSupplyReader(up)
		if _, err := c.ContractStorageSupply(context.Background(), supplyContractID); err != nil {
			t.Fatalf("fill: %v", err)
		}
		c.ExpireForTest(supplyContractID, tokenSupplyTTL+time.Second)
		if readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("stale while its refresh is inside its budget")
		}
		c.BackdateRefreshForTest(supplyContractID, cacheFillBudget+time.Second)
		if !readStorageSupplyStale(t, c, supplyContractID) {
			t.Error("not stale while its refresh has run past its budget")
		}
	})
}

// GET /v1/assets/{id}/supply carries the refresh outcome: a failed refresh is served stale, an idle healthy key is not.
func TestSupplyStorageReadingStaleOnlyAfterAFailedRefresh(t *testing.T) {
	for _, fail := range []bool{false, true} {
		up := &scriptedStorageSupply{out: dealStorageSupply()}
		up.fail.Store(fail)
		c := NewCachedContractStorageSupplyReader(up)
		if _, err := c.ContractStorageSupply(context.Background(), storageDealContractID); err != nil {
			t.Fatalf("fill: %v", err)
		}
		if fail {
			// Barely expired, so only the refresh outcome, not the age, can flag it.
			c.ExpireForTest(storageDealContractID, tokenSupplyTTL+time.Second)
			if _, err := c.ContractStorageSupply(context.Background(), storageDealContractID); err != nil {
				t.Fatalf("refresh trigger: %v", err)
			}
			c.AwaitRefreshForTest(storageDealContractID)
		} else {
			// Long idle, and this request is the first since: it starts the refresh.
			c.ExpireForTest(storageDealContractID, StorageSupplyIdleAge)
		}

		rec := serveSupplyWithStorage(t, zeroFlows(), c, storageDealContractID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
		}
		got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
		if got.TotalSupply != storageDealTotal {
			t.Fatalf("total_supply = %q, want the cached storage level %s", got.TotalSupply, storageDealTotal)
		}
		if flags.Stale != fail {
			t.Errorf("refresh failed=%v: flags.stale = %v, want %v", fail, flags.Stale, fail)
		}
	}
}
