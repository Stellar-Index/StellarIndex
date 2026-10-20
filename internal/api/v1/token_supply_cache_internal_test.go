package v1

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The wrapper must keep every capability the handlers type-assert off Server.TokenSupply.
var (
	_ TokenSupplyReader          = (*TokenSupplyCache)(nil)
	_ classicLakeSupplyReader    = (*TokenSupplyCache)(nil)
	_ rwaSupplyFlowHistoryReader = (*TokenSupplyCache)(nil)
)

type scriptedTokenSupply struct {
	calls atomic.Int32
	mint  int64
	err   error
	gate  chan struct{}
}

func (s *scriptedTokenSupply) read(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.err != nil {
		return clickhouse.TokenSupply{}, s.err
	}
	return clickhouse.TokenSupply{ContractID: id, Total: big.NewInt(s.mint), Mint: big.NewInt(s.mint), Burn: new(big.Int), Clawback: new(big.Int), FlowCount: 1}, nil
}

func TestTokenSupplyCache_RefillsAfterTTLAndKeepsLastGoodOnError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	up := &scriptedTokenSupply{mint: 100}
	c := newTokenSupplyCache(nil, up.read)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		sup, err := c.TokenSupply(ctx, "CTOKEN")
		if err != nil || sup.Total.Int64() != 100 {
			t.Fatalf("got %v, %v; want 100", sup.Total, err)
		}
	}
	if n := up.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d within TTL, want 1", n)
	}

	now = now.Add(tokenSupplyTTL)
	up.mint, up.err = 200, errors.New("lake down")
	sup, err := c.TokenSupply(ctx, "CTOKEN")
	if err != nil || sup.Total.Int64() != 100 {
		t.Fatalf("failed refill: got %v, %v; want last-good 100", sup.Total, err)
	}
	if _, err := c.TokenSupply(ctx, "CNEVERSEEN"); err == nil {
		t.Fatal("failed fill with no prior entry must return the error")
	}

	up.err = nil
	if sup, _ := c.TokenSupply(ctx, "CTOKEN"); sup.Total.Int64() != 200 {
		t.Fatalf("after TTL got %v, want refreshed 200", sup.Total)
	}
}

func TestTokenSupplyCache_ReturnsCopies(t *testing.T) {
	t.Parallel()
	c := newTokenSupplyCache(nil, (&scriptedTokenSupply{mint: 7}).read)
	ctx := context.Background()

	a, _ := c.TokenSupply(ctx, "CTOKEN")
	a.Total.SetInt64(0)
	a.Mint.Add(a.Mint, big.NewInt(1))
	b, _ := c.TokenSupply(ctx, "CTOKEN")
	if b.Total.Int64() != 7 || b.Mint.Int64() != 7 {
		t.Fatalf("a caller's mutation leaked into the cache: total=%v mint=%v", b.Total, b.Mint)
	}
}

func TestTokenSupplyCache_ConcurrentMissesShareOneFill(t *testing.T) {
	t.Parallel()
	up := &scriptedTokenSupply{mint: 5, gate: make(chan struct{})}
	c := newTokenSupplyCache(nil, up.read)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sup, err := c.TokenSupply(context.Background(), "CTOKEN"); err != nil || sup.Total.Int64() != 5 {
				t.Errorf("got %v, %v", sup.Total, err)
			}
		}()
	}
	for up.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	close(up.gate)
	wg.Wait()
	if n := up.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d for 8 concurrent misses, want 1", n)
	}
}

func TestTokenSupplyCache_EvictsAtCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newTokenSupplyCache(nil, (&scriptedTokenSupply{mint: 1}).read)
	c.now = func() time.Time { return now }
	for i := range tokenSupplyCacheMax {
		c.entries[string(rune(i))] = tokenSupplyEntry{at: now}
	}
	if _, err := c.TokenSupply(context.Background(), "CNEW"); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) > tokenSupplyCacheMax {
		t.Fatalf("entries = %d, cap %d", len(c.entries), tokenSupplyCacheMax)
	}
	if _, ok := c.entries["CNEW"]; !ok {
		t.Fatal("the new entry must survive eviction")
	}
}
