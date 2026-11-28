package clickhouse

import (
	"context"
	"testing"
	"time"
)

const swrTestAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func expiredStateReader(gateSlots int) *ExplorerReader {
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(gateSlots),
	}
	r.stateCache.put(swrTestAccount, AccountState{Exists: true, Balance: 7}, time.Now().Add(-AccountStateCacheTTL-time.Second))
	return r
}

func waitStateFlight(t *testing.T, r *ExplorerReader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fl, owner := r.stateFlight.begin(swrTestAccount)
		if owner {
			r.stateFlight.end(swrTestAccount, fl)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("account-state refresh flight never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An expired entry whose refresh is healthy and in flight reports no failure; the API layer serves it clean.
func TestAccountStateCached_ExpiredWithHealthyRefreshReportsNoFailure(t *testing.T) {
	t.Parallel()
	r := expiredStateReader(1)
	fl, _ := r.stateFlight.begin(swrTestAccount) // a refresh already running
	defer r.stateFlight.end(swrTestAccount, fl)

	st, pastTTL, err := r.AccountStateCached(context.Background(), swrTestAccount)
	if err != nil || !pastTTL || !st.Exists {
		t.Fatalf("got (%+v, pastTTL=%t, %v), want the expired entry served", st, pastTTL, err)
	}
	if st.RefreshFailed {
		t.Error("a healthy in-flight refresh was reported as failed")
	}
	if st.CachedAt.IsZero() || time.Since(st.CachedAt) <= AccountStateCacheTTL {
		t.Errorf("CachedAt = %v, want the expired entry's fill time", st.CachedAt)
	}
}

// A refresh the gate refused is not a failed refresh: only that serve is flagged, the entry's outcome is untouched, and the next
// serve with a free slot refreshes.
func TestAccountStateCached_RefusedRefreshIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, priorFailed := range []bool{false, true} {
		r := expiredStateReader(1)
		if priorFailed {
			r.stateCache.markRefreshFailed(swrTestAccount)
		}
		if !r.refreshGate.TryAcquire() {
			t.Fatal("could not saturate the gate")
		}
		for range 2 { // refused on every request: never served clean
			st, pastTTL, err := r.AccountStateCached(context.Background(), swrTestAccount)
			if err != nil || !pastTTL {
				t.Fatalf("got (pastTTL=%t, %v), want the expired entry served", pastTTL, err)
			}
			if !st.RefreshRefused {
				t.Error("the serve whose refresh was refused did not report the refusal")
			}
			if st.RefreshFailed != priorFailed {
				t.Errorf("RefreshFailed = %t after a refusal, want the earlier outcome %t", st.RefreshFailed, priorFailed)
			}
			if again, _, _ := r.stateCache.get(swrTestAccount); again.RefreshFailed != priorFailed || again.RefreshRefused {
				t.Errorf("the refusal leaked onto the entry: failed=%t refused=%t", again.RefreshFailed, again.RefreshRefused)
			}
		}
		r.refreshGate.Release()
		st, _, err := r.AccountStateCached(context.Background(), swrTestAccount)
		if err != nil || st.RefreshRefused {
			t.Fatalf("next serve with a free slot: refused=%t err=%v, want it to try again", st.RefreshRefused, err)
		}
		waitStateFlight(t, r) // the refresh ran (nil conn panics: a real failure), proving a slot was taken
		if after, _, _ := r.stateCache.get(swrTestAccount); !after.RefreshFailed {
			t.Error("the retried refresh did not run")
		}
		r.stateCache.put(swrTestAccount, AccountState{Exists: true}, time.Now())
		if after, _, _ := r.stateCache.get(swrTestAccount); after.RefreshFailed {
			t.Error("a complete successful fill did not clear the failure memory")
		}
	}
}

// A refresh that panics (nil conn) sets the failure memory; only a successful fill clears it.
func TestAccountStateRefresh_PanicSetsFailureUntilFill(t *testing.T) {
	r := expiredStateReader(1)
	if _, _, err := r.AccountStateCached(context.Background(), swrTestAccount); err != nil {
		t.Fatal(err)
	}
	waitStateFlight(t, r)
	if st, _, _ := r.stateCache.get(swrTestAccount); !st.RefreshFailed {
		t.Fatal("a panicked refresh left the entry reporting no failure")
	}
	r.stateCache.put(swrTestAccount, AccountState{Exists: true}, time.Now())
	if st, _, _ := r.stateCache.get(swrTestAccount); st.RefreshFailed {
		t.Error("a complete successful fill did not clear the failure memory")
	}
}

func waitWealthFlight(t *testing.T, r *ExplorerReader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, owner := r.wealthCache.beginFlight()
		if owner {
			r.wealthCache.endFlight(ch)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("wealth refresh flight never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The wealth snapshot: a healthy in-flight refresh reports no failure, a panicked one (nil conn) sets it,
// a failed prewarm sets it, and only a successful fill clears it.
func TestAccountsWealthCache_FailureMemory(t *testing.T) {
	r := &ExplorerReader{wealthCache: newAccountsWealthCache()}
	r.wealthCache.put(AccountWealthSnapshot{Basis: WealthBasisUSD, AsOf: time.Now().Add(-AccountsWealthCacheTTL - time.Minute)})
	in := []string{"USD:" + swrTestAccount}

	ch, _ := r.wealthCache.beginFlight() // a refresh already running
	snap, ok := r.AccountsByWealthCached(context.Background(), in, []string{"1"}, 10)
	r.wealthCache.endFlight(ch)
	if !ok || snap.RefreshFailed {
		t.Fatalf("healthy in-flight refresh: ok=%t RefreshFailed=%t, want served with no failure", ok, snap.RefreshFailed)
	}

	r.AccountsByWealthCached(context.Background(), in, []string{"1"}, 10) // owns the refresh, which panics
	waitWealthFlight(t, r)
	if snap, _ := r.wealthCache.get(); !snap.RefreshFailed {
		t.Fatal("a panicked refresh left the snapshot reporting no failure")
	}

	r.wealthCache.put(AccountWealthSnapshot{Basis: WealthBasisUSD, AsOf: time.Now()})
	if snap, _ := r.wealthCache.get(); snap.RefreshFailed {
		t.Fatal("a complete successful fill did not clear the failure memory")
	}

	func() {
		defer func() { _ = recover() }() // nil conn: the prewarm's scan panics like the refresh's
		_ = r.PrewarmAccountsByWealth(context.Background(), in, []string{"1"})
	}()
	if snap, _ := r.wealthCache.get(); !snap.RefreshFailed {
		t.Error("a failed prewarm left the snapshot reporting no failure")
	}
}
