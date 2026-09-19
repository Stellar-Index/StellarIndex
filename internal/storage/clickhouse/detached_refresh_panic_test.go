package clickhouse

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// K012 — the explorer's ClickHouse-side stale-while-revalidate refreshers
// run detached goroutines inside the stellarindex-api process, kicked from
// request paths on attacker-chosen keys. An unrecovered panic in ANY
// goroutine terminates the WHOLE process, so each of them has to recover.
//
// Recovery alone is not the fix, though, and these tests exist to pin the
// second half: a contained panic that does not ALSO release the thing the
// goroutine owned converts a process crash into a permanent wedge, which is
// worse — the crash at least ends with a fresh process. So every case below
// asserts three things about the panicking refresh:
//
//  1. the process survives (the test binary reaching its assertions at all
//     is that assertion — a bare `go func` would have taken it down);
//  2. the waiter is RELEASED with an honest failure, not left blocked and
//     not told "success, here is an empty answer"; and
//  3. the cache accepts a NEW flight afterwards, i.e. the single-flight
//     marker was cleared, so the panic costs one refresh and not the rest
//     of the process's life.
//
// It also pins the panic as VISIBLE: stellarindex_worker_panics_total is the
// only signal that a background worker died, so a recover that does not move
// it is a silent dead worker wearing the costume of a fix.

func TestTTLLivenessCache_PanickingRefreshReleasesFlightAndSurvives(t *testing.T) {
	const workerName = "explorer-ttl-liveness-refresh"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	var calls atomic.Int32
	c := newTTLLivenessCache(func(_ context.Context, keys []string) (map[string]TTLLiveness, error) {
		if calls.Add(1) == 1 {
			panic("ttl prefix scan blew up")
		}
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLArchived
		}
		return out, nil
	})

	// Cold resolve: kicks the detached recompute and waits on its flight.
	// A bare recover (contain, never release) leaves this blocked on a done
	// channel nobody will close, so it would come back DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.resolve(ctx, []string{"k-one"})
	if err == nil {
		t.Fatalf("panicking refresh reported success with verdicts %v — a panicked "+
			"scan must not be served as an authoritative empty verdict set", got)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter was never released (%v): the refresh goroutine contained the "+
			"panic without ending the flight, which wedges coldFill forever — release "+
			"from a defer", err)
	}
	if !errors.Is(err, errTTLRefreshPanicked) {
		t.Fatalf("cold resolve after a panicking refresh = %v, want errTTLRefreshPanicked", err)
	}

	// The single-flight marker must be clear, or kickRefresh keeps handing
	// out the same dead flight for the life of the process.
	c.mu.Lock()
	stuck := c.flight
	c.mu.Unlock()
	if stuck != nil {
		t.Fatalf("c.flight still points at the panicked flight — every later caller "+
			"joins a flight that will never run again (%p)", stuck)
	}

	// A fresh flight must be startable and must serve real verdicts.
	got, err = c.resolve(ctx, []string{"k-one"})
	if err != nil {
		t.Fatalf("resolve after the panicked refresh: %v", err)
	}
	if got["k-one"] != TTLArchived {
		t.Fatalf("verdict after recovery = %v, want TTLArchived", got["k-one"])
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("compute ran %d times, want 2 (the panicking one and its replacement)", n)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a recover "+
			"that does not move the counter turns a loud crash into a silent dead worker",
			workerName, after, before+1)
	}
}

// The account-state and wealth refreshers already release their flight from
// a defer, so what these two pin is the guard itself — plus the fact that
// the release still happens once the panic is contained rather than fatal.
// The panic source is the real one a zero-value reader produces:
// ExplorerReader.conn is nil, so the scan's first r.conn.Query dereferences
// a nil interface.
func TestAccountStateRefresh_PanicReleasesFlightAndSurvives(t *testing.T) {
	const workerName = "explorer-account-state-refresh"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	const account = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(1),
	}

	fl, saturated := r.refreshAccountState(account)
	if saturated {
		t.Fatal("refresh reported saturation; the gate has a free slot in this test")
	}
	select {
	case <-fl.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the account-state flight never ended: the refresh goroutine contained " +
			"its panic without releasing the waiter, so every reader of this account " +
			"blocks until the process dies — release from a defer")
	}

	// The per-key flight must be gone, so the next request re-kicks, and the
	// gate slot must be back or the whole class is saturated for good.
	if fl2, owner := r.stateFlight.begin(account); !owner {
		t.Error("the panicked flight is still registered; the next caller joins a dead flight")
	} else {
		r.stateFlight.end(account, fl2)
	}
	if !r.refreshGate.TryAcquireClass("account_state") {
		t.Error("the panicking goroutine never released its refresh-gate slot")
	} else {
		r.refreshGate.ReleaseClass("account_state")
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v",
			workerName, after, before+1)
	}
}

func TestAccountsWealthRefresh_PanicReleasesFlightAndSurvives(t *testing.T) {
	const workerName = "explorer-accounts-wealth-refresh"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	r := &ExplorerReader{wealthCache: newAccountsWealthCache()}
	r.refreshAccountsWealth(
		[]string{"USD:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		[]float64{1})

	// endFlight must have run, so a second refresh can own a new flight.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, owner := r.wealthCache.beginFlight()
		if owner {
			r.wealthCache.endFlight(ch)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wealth flight never ended: the refresh goroutine contained its " +
				"panic without releasing it, so /v1/accounts never re-scans for the life " +
				"of the process")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v",
			workerName, after, before+1)
	}
}
