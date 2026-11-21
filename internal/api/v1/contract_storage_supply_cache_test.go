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

// ageStorageSupplyEntry backdates key's fill time, as a refresh that keeps failing leaves it.
func ageStorageSupplyEntry(t *testing.T, c *CachedContractStorageSupplyReader, key string, age time.Duration) {
	t.Helper()
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	e, ok := c.cache.entries[key]
	if !ok || e.at.IsZero() {
		t.Fatalf("no filled entry for %s", key)
	}
	e.at = time.Now().Add(-age)
}

// A reading kept alive by failing refreshes is not current: past its TTL and one refresh budget it is reported stale.
func TestCachedContractStorageSupply_OverdueReadingIsReportedStale(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		wantStale bool
	}{
		{"fresh", 0, false},
		{"just expired: its refresh is in flight", tokenSupplyTTL + time.Second, false},
		{"overdue: refreshes have kept failing", tokenSupplyTTL + cacheFillBudget + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCachedContractStorageSupplyReader(&blockingStorageSupply{
				out: clickhouse.ContractStorageSupply{Total: big.NewInt(500), BalanceEntries: 3},
			})
			if _, err := c.ContractStorageSupply(context.Background(), supplyContractID); err != nil {
				t.Fatalf("fill: %v", err)
			}
			if tc.age > 0 {
				ageStorageSupplyEntry(t, c, supplyContractID, tc.age)
			}
			ctx, rep := withReadReport(context.Background())
			st, err := c.ContractStorageSupply(ctx, supplyContractID)
			if err != nil || st.Total.Int64() != 500 {
				t.Fatalf("read: total=%v err=%v, want the cached 500", st.Total, err)
			}
			if got := rep.stale.Load(); got != tc.wantStale {
				t.Errorf("reported stale = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

// blockingStorageSupply answers its first read and fails every later one, as a scan that outgrew its budget does.
type blockingStorageSupply struct {
	calls atomic.Int32
	out   clickhouse.ContractStorageSupply
}

func (b *blockingStorageSupply) ContractStorageSupply(_ context.Context, id string) (clickhouse.ContractStorageSupply, error) {
	if b.calls.Add(1) > 1 {
		return clickhouse.ContractStorageSupply{}, errors.New("storage scan exceeded its budget")
	}
	out := b.out
	out.ContractID = id
	return out, nil
}

// The overdue reading reaches the wire: GET /v1/assets/{id}/supply serves it flagged stale, not as a current floor.
func TestSupplyOverdueStorageReadingIsServedStale(t *testing.T) {
	c := NewCachedContractStorageSupplyReader(&blockingStorageSupply{out: dealStorageSupply()})
	if _, err := c.ContractStorageSupply(context.Background(), storageDealContractID); err != nil {
		t.Fatalf("fill: %v", err)
	}
	ageStorageSupplyEntry(t, c, storageDealContractID, tokenSupplyTTL+cacheFillBudget+time.Minute)

	rec := serveSupplyWithStorage(t, zeroFlows(), c, storageDealContractID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
	}
	got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
	if got.TotalSupply != storageDealTotal {
		t.Fatalf("total_supply = %q, want the cached storage level %s", got.TotalSupply, storageDealTotal)
	}
	if !flags.Stale {
		t.Error("flags.stale = false on a storage reading its refreshes have not replaced for longer than TTL + refresh budget")
	}
}
