package clickhouse

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// clickhouseGoroutineFloor is a "did this test find anything?" floor, not an exact count: the
// account_state_cache, ttl_liveness_cache and accounts_wealth_cache refresh goroutines existed when the guard
// was written.
const clickhouseGoroutineFloor = 3

// TestClickhouseGoroutinesRecover requires every `go` statement in a non-test file here to defer
// worker.Recover or worker.Report, since cache-refresh paths take attacker-chosen accounts and a panic is
// reachable.
func TestClickhouseGoroutinesRecover(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanner := guardscan.NewScanner(guardscan.Config{Guards: []string{"worker.Recover", "worker.Report"}})
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		sites, err := scanner.ScanFile(f)
		if err != nil {
			t.Fatalf("scan %s: %v", f, err)
		}
		for _, s := range sites {
			checked++
			if s.Kind == guardscan.KindUnresolved || !s.Recovers {
				t.Errorf("go %s at %s:%d does not defer worker.Recover/worker.Report — a panic "+
					"there kills the whole process, not just the one cached read", s.Target, f, s.Line)
			}
		}
	}
	if checked < clickhouseGoroutineFloor {
		t.Errorf("discovered %d goroutine(s) in internal/storage/clickhouse, want at least %d — "+
			"the scan has drifted from the code", checked, clickhouseGoroutineFloor)
	}
}

// The explorer's ClickHouse-side stale-while-revalidate refreshers run detached goroutines
// kicked from request paths on attacker-chosen keys. An unrecovered panic in ANY goroutine kills
// the WHOLE process, so each must recover.
//
// Recovery alone is not enough: a contained panic that does not ALSO release what the goroutine
// owned turns a crash into a permanent wedge. So every case asserts about the panicking refresh:
//
//  1. the process survives (a bare `go func` would have taken the test binary down);
//  2. the waiter is RELEASED with an honest failure, not left blocked and not told "success,
//     here is an empty answer"; and
//  3. the cache accepts a NEW flight afterwards, i.e. the single-flight marker was cleared, so
//     the panic costs one refresh and not the rest of the process's life.
//
// It also pins the panic as VISIBLE: stellarindex_worker_panics_total is the only signal that a
// background worker died, so a recover that does not move it is a silent dead worker.

// The account-state and wealth refreshers already release their flight from a defer; these two pin the guard
// itself and that the release still happens once the panic is contained (a zero-value ExplorerReader has a
// nil conn).
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
		[]string{"1"})

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
