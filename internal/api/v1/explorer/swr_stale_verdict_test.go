package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Each SWR cache serves an entry just past its TTL clean while it refreshes, and flags it once a refresh has failed.
// serve returns the degraded verdict; age re-ages the entry; flightKey names the refresh to wait on.
func assertSWRStaleVerdict(t *testing.T, flight *perKeyFlight, flightKey string, justPast time.Duration,
	age func(time.Duration), setFail func(bool), serve func() bool,
) {
	t.Helper()
	age(justPast)
	if serve() {
		t.Error("an entry just past its TTL, inside its bound and with no failed refresh, was served degraded")
	}
	waitFlightIdle(t, flight, flightKey)
	setFail(true)
	age(justPast)
	serve()
	waitFlightIdle(t, flight, flightKey)
	if !serve() {
		t.Error("an entry whose last refresh failed was served clean")
	}
	waitFlightIdle(t, flight, flightKey)
}

func TestRecentContractsCached_StaleVerdict(t *testing.T) {
	h, reader := newSWRHandler()
	window := contractsWindow(30)
	if _, _, _, _, _, err := h.recentContractsCached(context.Background(), window, 10); err != nil {
		t.Fatal(err)
	}
	assertSWRStaleVerdict(t, &h.contractsDir.flight, strconv.Itoa(window), contractsDirTTL+time.Minute,
		func(d time.Duration) {
			h.contractsDir.mu.Lock()
			e := h.contractsDir.entries[window]
			e.cachedAt = time.Now().Add(-d)
			h.contractsDir.entries[window] = e
			h.contractsDir.mu.Unlock()
		},
		reader.fail.Store,
		func() bool {
			_, _, _, _, degraded, err := h.recentContractsCached(context.Background(), window, 10)
			if err != nil {
				t.Fatal(err)
			}
			return degraded
		})
}

func TestAssetHoldersCached_StaleVerdict(t *testing.T) {
	h, reader := newSWRHandler()
	const asset = "native"
	if _, _, _, _, err := h.assetHoldersCached(context.Background(), asset, 10); err != nil {
		t.Fatal(err)
	}
	age := func(d time.Duration) {
		h.assetHolders.mu.Lock()
		e := h.assetHolders.entries[asset]
		e.cachedAt = time.Now().Add(-d)
		h.assetHolders.entries[asset] = e
		h.assetHolders.mu.Unlock()
	}
	// heldServe serves while the detached refresh it kicks is held, so the verdict is on the aged entry, not a refill.
	verdict := func() servedFreshness {
		reader.holdersBlock = make(chan struct{})
		_, _, _, fr, err := h.assetHoldersCached(context.Background(), asset, 10)
		close(reader.holdersBlock)
		waitFlightIdle(t, &h.assetHolders.flight, asset)
		reader.holdersBlock = nil
		if err != nil {
			t.Fatal(err)
		}
		return fr
	}
	heldServe := func() bool { return verdict().degraded }

	age(assetHoldersTTL + time.Second)
	if fr := verdict(); fr != (servedFreshness{}) {
		t.Errorf("a board just past its TTL, with a healthy refresh running, was served %+v", fr)
	}
	age(assetHoldersTTL + min(assetHoldersTTL, assetHoldersRefreshTimeout) + time.Second)
	if fr := verdict(); !fr.stale || fr.degraded {
		t.Errorf("a board past its TTL plus one refresh's grace, with a healthy refresh running, was served %+v; want stale only", fr)
	}

	reader.fail.Store(true)
	age(assetHoldersTTL + time.Second)
	heldServe() // kicks the refresh that fails
	if !heldServe() {
		t.Error("a board whose last refresh failed was served clean")
	}
	reader.fail.Store(false)
	if !heldServe() {
		t.Error("a board whose last refresh failed was served clean before a refresh succeeded")
	}
	age(assetHoldersTTL + time.Second)
	if heldServe() {
		t.Error("after a successful refresh, a board just past its TTL was served degraded")
	}
}

func TestOpTypeStats_StaleVerdict(t *testing.T) {
	h, reader := newSWRHandler()
	h.PrewarmOpTypeStats(context.Background())
	waitFlightIdle(t, &h.opTypeStats.flight, "stats")
	age := func(d time.Duration) {
		h.opTypeStats.mu.Lock()
		h.opTypeStats.at = time.Now().Add(-d)
		h.opTypeStats.mu.Unlock()
	}
	resolve := func() bool {
		_, stale := h.resolveOpTypeStats()
		waitFlightIdle(t, &h.opTypeStats.flight, "stats")
		return stale
	}

	age(opTypeStatsTTL + time.Second)
	if resolve() {
		t.Error("a panel just past its TTL, with a healthy refresh running, was served stale")
	}
	reader.fail.Store(true)
	age(opTypeStatsTTL + min(opTypeStatsTTL, opTypeStatsRefreshTimeout) + time.Second)
	if !resolve() {
		t.Error("a panel past its TTL plus one refresh's grace was served clean")
	}
	age(opTypeStatsTTL + time.Second)
	if !resolve() {
		t.Error("a panel whose last refresh failed was served clean")
	}
}

func TestContractStats_StaleVerdict(t *testing.T) {
	h, reader := newStatsHandler(nil)
	age := func(d time.Duration) {
		h.contractStats.mu.Lock()
		h.contractStats.entry.cachedAt = time.Now().Add(-d)
		h.contractStats.mu.Unlock()
	}
	var stale, degraded bool
	h.WriteJSON = func(_ http.ResponseWriter, _ any, s bool) { stale, degraded = s, false }
	h.WriteJSONAt = func(_ http.ResponseWriter, _ any, s, d bool, _ time.Time) { stale, degraded = s, d }
	serve := func() bool {
		h.ContractStats(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/contracts/stats", nil))
		if stale != degraded {
			t.Errorf("stale = %v degraded = %v, want them together", stale, degraded)
		}
		return degraded
	}
	serve() // cold fill
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	assertSWRStaleVerdict(t, &h.contractStats.flight, contractStatsKey, contractStatsTTL+time.Minute, age, reader.fail.Store, serve)

	reader.fail.Store(false)
	serve()
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	age(contractStatsTTL + time.Minute)
	if serve() {
		t.Error("after a successful refresh: an entry just past its TTL was served degraded")
	}
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	// Hold the refresh: one that lands first is rightly served clean, as the fresh entry it is.
	reader.block = make(chan struct{})
	age(contractStatsTTL + contractStatsRefreshTimeout + time.Second)
	if !serve() {
		t.Error("an entry past its hard bound was served clean")
	}
	close(reader.block)
	waitFlightIdle(t, &h.contractStats.flight, contractStatsKey)
	if serve() {
		t.Error("the entry the held refresh landed was served degraded")
	}
}

func TestOpsDirCached_StaleVerdict(t *testing.T) {
	h, reader, _ := newOpsDirHandler()
	if _, _, _, err := h.opsDirCached(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSWRStaleVerdict(t, &h.opsDir.flight, opsDirFlightKey, opsDirTTL+2*time.Second,
		func(d time.Duration) {
			h.opsDir.mu.Lock()
			h.opsDir.entry.cachedAt = time.Now().Add(-d)
			h.opsDir.mu.Unlock()
		},
		reader.fail.Store,
		func() bool {
			_, _, degraded, err := h.opsDirCached(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			return degraded
		})
}

func TestNetworkThroughputCached_StaleVerdict(t *testing.T) {
	h, reader := newThroughputHandler()
	if _, _, _, err := h.networkThroughputCached(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	assertSWRStaleVerdict(t, &h.throughput.flight, networkThroughputFlightKey, networkThroughputTTL+time.Minute,
		func(d time.Duration) {
			h.throughput.mu.Lock()
			h.throughput.entry.cachedAt = time.Now().Add(-d)
			h.throughput.mu.Unlock()
		},
		reader.fail.Store,
		func() bool {
			_, _, degraded, err := h.networkThroughputCached(context.Background(), 30)
			if err != nil {
				t.Fatal(err)
			}
			return degraded
		})
}
