package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// TestAccountStateCached_ColdChargeRefusedClaimsNothing: a cold account read whose request cannot pay the cold-fill surcharge
// returns the refusal before claiming a gate slot or leaving a flight, so no scan starts.
func TestAccountStateCached_ColdChargeRefusedClaimsNothing(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
	}
	var charged int
	ctx := ratelimit.WithColdFillCharge(context.Background(), func(n int) error {
		charged += n
		return fmt.Errorf("budget spent: %w", ratelimit.ErrColdFillRefused)
	})
	_, _, err := r.AccountStateCached(ctx, "GCOLD")
	if !errors.Is(err, ratelimit.ErrColdFillRefused) {
		t.Fatalf("err = %v, want ErrColdFillRefused", err)
	}
	if charged != ratelimit.ColdFillCost {
		t.Fatalf("charged %d, want ColdFillCost (%d)", charged, ratelimit.ColdFillCost)
	}
	r.refreshGate.mu.Lock()
	held, parked := r.refreshGate.classes["account_state"], r.refreshGate.waiting["account_state"]
	r.refreshGate.mu.Unlock()
	if held != 0 || parked != 0 || len(r.refreshGate.sem) != 0 {
		t.Fatalf("gate after refusal: held=%d parked=%d sem=%d, want all 0", held, parked, len(r.refreshGate.sem))
	}
	r.stateFlight.mu.Lock()
	inFlight := len(r.stateFlight.inGoing)
	r.stateFlight.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("%d flights left after refusal, want 0", inFlight)
	}
}

// joinSignalCtx closes joined on the first Done call: on the cold path only a request waiting on a flight selects on ctx.Done.
type joinSignalCtx struct {
	context.Context
	once   sync.Once
	joined chan struct{}
}

func (c *joinSignalCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

// TestAccountStateCached_JoinerOfRefusedOwnerRetriesAsOwner: a request that joined a flight whose owner was then refused its charge
// retries once as owner, paying its own charge, instead of answering ErrRefreshSaturated (a 503) for a scan that never ran.
func TestAccountStateCached_JoinerOfRefusedOwnerRetriesAsOwner(t *testing.T) {
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
	}
	const account = "GREFUSEDOWNER"
	release := make(chan struct{})
	ownerCtx := ratelimit.WithColdFillCharge(context.Background(), func(int) error {
		<-release // hold the flight until the joiner is waiting on it
		return fmt.Errorf("budget spent: %w", ratelimit.ErrColdFillRefused)
	})
	var charged atomic.Int64
	joiner := &joinSignalCtx{Context: ratelimit.WithColdFillCharge(context.Background(), func(n int) error {
		charged.Add(int64(n))
		return nil
	}), joined: make(chan struct{})}

	ownerErr := make(chan error, 1)
	go func() {
		_, _, err := r.AccountStateCached(ownerCtx, account)
		ownerErr <- err
	}()
	for {
		r.stateFlight.mu.Lock()
		up := len(r.stateFlight.inGoing) == 1
		r.stateFlight.mu.Unlock()
		if up {
			break
		}
		time.Sleep(time.Millisecond)
	}
	joinErr := make(chan error, 1)
	go func() {
		_, _, err := r.AccountStateCached(joiner, account)
		joinErr <- err
	}()
	<-joiner.joined
	close(release)
	if err := <-ownerErr; !errors.Is(err, ratelimit.ErrColdFillRefused) {
		t.Fatalf("owner: err = %v, want ErrColdFillRefused", err)
	}
	// The joiner's own scan has no lake here (nil conn), so it fails; what matters is that it ran, paid for, not shed.
	if err := <-joinErr; errors.Is(err, ErrRefreshSaturated) {
		t.Fatalf("joiner of a refused owner: err = %v, want its own fill attempted", err)
	}
	if got := charged.Load(); got != ratelimit.ColdFillCost {
		t.Fatalf("joiner charged %d, want ColdFillCost (%d) for the fill it then owned", got, ratelimit.ColdFillCost)
	}
}

// TestAccountStateCached_WarmReadNotCharged: an entry in cache, fresh or stale, is served without a cold-fill charge.
func TestAccountStateCached_WarmReadNotCharged(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
	}
	ctx := ratelimit.WithColdFillCharge(context.Background(), func(int) error {
		t.Error("a warm read was charged a cold fill")
		return nil
	})
	r.stateCache.put("GWARM", AccountState{Exists: true}, time.Now())
	if _, _, err := r.AccountStateCached(ctx, "GWARM"); err != nil {
		t.Fatalf("warm read: %v", err)
	}
}
