package explorer

import (
	"context"
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
