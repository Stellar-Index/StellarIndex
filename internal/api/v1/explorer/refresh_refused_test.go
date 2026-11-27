package explorer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A refresh the gate refused is not a failed refresh: the entry keeps its real last-refresh outcome, the serve that was refused is
// stale + degraded with reason refresh_refused (the markets cache's verdict), and the next request with a free slot refreshes.

// saturateClass takes every slot class may hold and returns the release.
func saturateClass(t *testing.T, gate *clickhouse.RefreshGate, class string) func() {
	t.Helper()
	held := 0
	for gate.TryAcquireClass(class) {
		held++
		if held > 1000 {
			t.Fatal("gate never saturates")
		}
	}
	if held == 0 {
		t.Fatal("gate refused its first slot")
	}
	return func() {
		for ; held > 0; held-- {
			gate.ReleaseClass(class)
		}
	}
}

// refusedWant is the verdict of a refused serve: a recorded failure keeps naming itself, otherwise the refusal is the reason.
func refusedWant(priorFailed bool) servedFreshness {
	if priorFailed {
		return servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshFailed}
	}
	return servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshRefused}
}

func TestContractDetailCached_RefusalKeepsRefreshOutcome(t *testing.T) {
	const key = "ev:C1"
	for _, priorFailed := range []bool{false, true} {
		h, _ := newSWRHandler()
		h.contractDetail.put(key, "old")
		ageContractDetail(h, key, contractDetailTTL+time.Minute) // inside the stale bound: only the refresh outcome can flag it
		if priorFailed {
			h.contractDetail.markRefreshFailed(key)
		}
		release := saturateClass(t, h.detachedGate(), detachedClassForKey(key))
		ran := 0
		compute := func(context.Context) (any, error) { ran++; return "new", nil }

		v, _, fr, err := h.contractDetailCached(context.Background(), key, compute)
		if err != nil || v != "old" || fr != refusedWant(priorFailed) {
			t.Fatalf("priorFailed=%v refused serve: v=%v %+v err=%v, want old with %+v", priorFailed, v, fr, err, refusedWant(priorFailed))
		}
		if e, _, _ := h.contractDetail.get(key); e.refreshFailed != priorFailed {
			t.Fatalf("priorFailed=%v: refusal set refreshFailed=%v, want it unchanged", priorFailed, e.refreshFailed)
		}
		release()

		// The next request has a slot: it refreshes, and its own serve carries only the entry's real outcome.
		_, _, fr, _ = h.contractDetailCached(context.Background(), key, compute)
		if !priorFailed && fr.degraded {
			t.Errorf("serve after the gate freed, no failure recorded: %+v, want not degraded", fr)
		}
		waitFlightIdle(t, &h.contractDetail.flight, key)
		v, _, fr, _ = h.contractDetailCached(context.Background(), key, compute)
		if ran != 1 || v != "new" || fr != (servedFreshness{}) {
			t.Errorf("priorFailed=%v after refresh: ran=%d v=%v %+v, want one refresh serving new, fresh", priorFailed, ran, v, fr)
		}
	}
}

// A key refused on every serve (always third in its class) is never served clean: each refused serve is degraded, and past the
// stale bound its age is reported too.
func TestContractDetailCached_RefusedEveryServeNeverClean(t *testing.T) {
	const key = "ix:C1"
	h, _ := newSWRHandler()
	h.contractDetail.put(key, "old")
	ageContractDetail(h, key, detailStaleAfterForKey(key)+time.Hour)
	defer saturateClass(t, h.detachedGate(), detachedClassForKey(key))()
	for i := range 3 {
		_, _, fr, err := h.contractDetailCached(context.Background(), key, func(context.Context) (any, error) { return "new", nil })
		if err != nil || fr != refusedWant(false) {
			t.Fatalf("refused serve %d: %+v err=%v, want %+v", i, fr, err, refusedWant(false))
		}
	}
}

func TestAssetHoldersCached_RefusalKeepsRefreshOutcome(t *testing.T) {
	const asset = "native"
	for _, priorFailed := range []bool{false, true} {
		h, reader := newSWRHandler()
		h.assetHolders.put(asset, assetHoldersEntry{
			holders: []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 1}},
			total:   1, cachedAt: time.Now().Add(-assetHoldersTTL - time.Second),
		})
		if priorFailed {
			h.assetHolders.markRefreshFailed(asset)
		}
		release := saturateClass(t, h.detachedGate(), "asset_holders")

		_, total, _, fr, err := h.assetHoldersCached(context.Background(), asset, 10)
		if err != nil || total != 1 || fr != refusedWant(priorFailed) {
			t.Fatalf("priorFailed=%v refused serve: total=%d %+v err=%v, want %+v", priorFailed, total, fr, err, refusedWant(priorFailed))
		}
		if e, _, _ := h.assetHolders.get(asset); e.refreshFailed != priorFailed {
			t.Fatalf("priorFailed=%v: refusal set refreshFailed=%v, want it unchanged", priorFailed, e.refreshFailed)
		}
		release()

		reader.holdersBlock = make(chan struct{})
		_, _, _, fr, _ = h.assetHoldersCached(context.Background(), asset, 10)
		close(reader.holdersBlock)
		if !priorFailed && fr.degraded {
			t.Errorf("serve after the gate freed, no failure recorded: %+v, want not degraded", fr)
		}
		waitFlightIdle(t, &h.assetHolders.flight, asset)
		if _, ok, fresh := h.assetHolders.get(asset); !ok || !fresh || reader.holdersCalls.Load() != 1 {
			t.Errorf("priorFailed=%v after refresh: ok=%v fresh=%v scans=%d, want one scan and a fresh entry",
				priorFailed, ok, fresh, reader.holdersCalls.Load())
		}
	}
}

func TestRecentContractsCached_RefusalKeepsRefreshOutcome(t *testing.T) {
	for _, priorFailed := range []bool{false, true} {
		h, reader := newSWRHandler()
		h.contractsDir.put(30, contractsDirEntry{
			rows:  []clickhouse.ContractDirectoryRow{{ContractID: validTestContract, Events: 1}},
			since: 100, cachedAt: time.Now().Add(-contractsDirTTL - time.Second),
		})
		if priorFailed {
			h.contractsDir.markRefreshFailed(30)
		}
		release := saturateClass(t, h.detachedGate(), "contracts_dir")

		rows, _, _, _, fr, err := h.recentContractsCached(context.Background(), 30, 10)
		if err != nil || len(rows) != 1 || fr != refusedWant(priorFailed) {
			t.Fatalf("priorFailed=%v refused serve: rows=%d %+v err=%v, want %+v", priorFailed, len(rows), fr, err, refusedWant(priorFailed))
		}
		if e, _, _ := h.contractsDir.get(30); e.refreshFailed != priorFailed {
			t.Fatalf("priorFailed=%v: refusal set refreshFailed=%v, want it unchanged", priorFailed, e.refreshFailed)
		}
		release()

		reader.contractsBlock = make(chan struct{})
		_, _, _, _, fr, _ = h.recentContractsCached(context.Background(), 30, 10)
		close(reader.contractsBlock)
		if !priorFailed && fr.degraded {
			t.Errorf("serve after the gate freed, no failure recorded: %+v, want not degraded", fr)
		}
		waitFlightIdle(t, &h.contractsDir.flight, "30")
		if _, ok, fresh := h.contractsDir.get(30); !ok || !fresh || reader.contractsCalls.Load() != 1 {
			t.Errorf("priorFailed=%v after refresh: ok=%v fresh=%v runs=%d, want one run and a fresh entry",
				priorFailed, ok, fresh, reader.contractsCalls.Load())
		}
	}
}

// The single-entry caches on the same gate: a refused refresh leaves refreshFailed false and the serve names the refusal.
func TestSingleEntryCaches_RefusalIsNotAFailure(t *testing.T) {
	t.Run("operations directory", func(t *testing.T) {
		h, _ := newSWRHandler()
		h.opsDir.put(OperationsView{})
		h.opsDir.mu.Lock()
		h.opsDir.entry.cachedAt = time.Now().Add(-opsDirTTL - time.Second)
		h.opsDir.mu.Unlock()
		defer saturateClass(t, h.detachedGate(), "ops_directory")()
		_, _, fr, err := h.opsDirCached(context.Background())
		if err != nil || fr != refusedWant(false) {
			t.Fatalf("refused serve: %+v err=%v, want %+v", fr, err, refusedWant(false))
		}
		if e, _, _ := h.opsDir.get(); e.refreshFailed {
			t.Error("refusal recorded as a failed refresh")
		}
	})
	t.Run("network throughput", func(t *testing.T) {
		h, _ := newSWRHandler()
		h.throughput.put(nil)
		h.throughput.mu.Lock()
		h.throughput.entry.cachedAt = time.Now().Add(-networkThroughputTTL - time.Second)
		h.throughput.mu.Unlock()
		defer saturateClass(t, h.detachedGate(), "network_throughput")()
		_, _, stale, err := h.networkThroughputCached(context.Background(), 7)
		if err != nil || !stale {
			t.Fatalf("refused serve: stale=%v err=%v, want stale", stale, err)
		}
		if e, _, _ := h.throughput.get(); e.refreshFailed {
			t.Error("refusal recorded as a failed refresh")
		}
	})
}

func TestKeyFlight_RefusedOnlyWhenEndedByARefusal(t *testing.T) {
	var f perKeyFlight
	fl, _ := f.begin("k")
	if fl.refused() {
		t.Error("a running flight reported refused")
	}
	f.end("k", fl, errRefreshSaturated)
	if !fl.refused() {
		t.Error("a flight ended errRefreshSaturated not reported refused")
	}
	for _, err := range []error{nil, errors.New("read failed"), errRefreshPanicked} {
		fl, _ := f.begin("k")
		f.end("k", fl, err)
		if fl.refused() {
			t.Errorf("flight ended %v reported refused", err)
		}
	}
}
