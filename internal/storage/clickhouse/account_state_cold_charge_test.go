package clickhouse

import (
	"context"
	"errors"
	"fmt"
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
