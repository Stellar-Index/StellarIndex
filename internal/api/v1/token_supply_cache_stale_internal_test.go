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

// gatedTokenSupply parks every read while held; release decides whether the parked read fails.
type gatedTokenSupply struct {
	mu   sync.Mutex
	gate chan struct{}
	fail atomic.Bool
	mint atomic.Int64
}

func (g *gatedTokenSupply) hold() {
	g.mu.Lock()
	g.gate = make(chan struct{})
	g.mu.Unlock()
}

func (g *gatedTokenSupply) release(fail bool, mint int64) {
	g.fail.Store(fail)
	g.mint.Store(mint)
	g.mu.Lock()
	close(g.gate)
	g.gate = nil
	g.mu.Unlock()
}

func (g *gatedTokenSupply) read(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if g.fail.Load() {
		return clickhouse.TokenSupply{}, errors.New("lake down")
	}
	m := g.mint.Load()
	return clickhouse.TokenSupply{ContractID: id, Total: big.NewInt(m), Mint: big.NewInt(m), Burn: new(big.Int), Clawback: new(big.Int)}, nil
}

// A caller that stops waiting on a refresh is served the expired reading: clean while that refresh is healthy and inside
// TTL+min(TTL, fill timeout), stale once a refresh has failed or the reading is past that bound.
func TestTokenSupplyCache_TimedOutWaitIsStaleOnlyWhenLateOrFailed(t *testing.T) {
	clk := &swrTestClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	up := &gatedTokenSupply{}
	up.mint.Store(100)
	c := newTokenSupplyCache(nil, up.read)
	c.now = clk.now

	read := func(step string, wantTotal int64, wantStale bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		ctx, rep := withReadReport(ctx)
		sup, err := c.TokenSupply(ctx, "CTOKEN")
		if err != nil || sup.Total.Int64() != wantTotal {
			t.Fatalf("%s: got %v, %v; want %d", step, sup.Total, err, wantTotal)
		}
		if rep.stale.Load() != wantStale {
			t.Errorf("%s: reported stale=%v, want %v", step, rep.stale.Load(), wantStale)
		}
	}
	// settle waits for the parked fill to finish, joining it or running a fresh one with the same outcome.
	settle := func() {
		t.Helper()
		_, _ = c.TokenSupply(context.Background(), "CTOKEN")
	}

	read("cold fill", 100, false)

	clk.add(tokenSupplyTTL + time.Second)
	up.hold()
	read("past TTL, healthy refresh in flight", 100, false)
	up.release(true, 100)
	settle()

	up.hold()
	read("previous refresh failed, retry in flight", 100, true)
	up.release(false, 200)
	settle()

	read("fresh after a successful refresh", 200, false)

	clk.add(tokenSupplyTTL + tokenSupplyFillTimeout + time.Second)
	up.hold()
	read("past the hard bound, refresh in flight", 200, true)
	up.release(false, 200)
	settle()
}
