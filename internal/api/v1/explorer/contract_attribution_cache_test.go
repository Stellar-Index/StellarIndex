package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingContractsReader struct {
	calls atomic.Int32
	fail  atomic.Bool
	panc  atomic.Bool
	idx   map[string]string
}

func (r *countingContractsReader) ProtocolContractIndex(ctx context.Context) (map[string]string, error) {
	r.calls.Add(1)
	if r.panc.Load() {
		panic("registry reader bug")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.fail.Load() {
		return nil, errors.New("registry unavailable")
	}
	return r.idx, nil
}

func newAttributionHandler(r *countingContractsReader) *Handler {
	return &Handler{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProtocolContracts: r,
	}
}

// The registry map is read once per TTL, not once per request, however
// many concurrent requests ask for it.
func TestContractAttribution_ReadsRegistryOncePerTTL(t *testing.T) {
	r := &countingContractsReader{idx: map[string]string{"CBLEND": "blend"}}
	h := newAttributionHandler(r)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := h.contractAttribution(context.Background())["CBLEND"]; got != "blend" {
				t.Errorf("attribution = %q, want blend", got)
			}
		}()
	}
	wg.Wait()
	for range 10 {
		h.contractAttribution(context.Background())
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("ProtocolContractIndex calls = %d, want 1", n)
	}
}

// Past the TTL the map is re-read; a failed re-read serves the last good map
// rather than dropping every attribution.
func TestContractAttribution_RefreshesAfterTTLAndServesLastGoodOnError(t *testing.T) {
	r := &countingContractsReader{idx: map[string]string{"CBLEND": "blend"}}
	h := newAttributionHandler(r)
	h.contractAttribution(context.Background())

	h.attribution.loadedAt = time.Now().Add(-2 * contractAttributionTTL)
	r.fail.Store(true)
	if got := h.contractAttribution(context.Background())["CBLEND"]; got != "blend" {
		t.Fatalf("attribution after failed refresh = %q, want last good blend", got)
	}
	if n := r.calls.Load(); n != 2 {
		t.Fatalf("ProtocolContractIndex calls = %d, want 2 (expired entry re-read)", n)
	}

	r.fail.Store(false)
	r.idx = map[string]string{"CBLEND": "blend", "CSWAP": "soroswap"}
	if got := h.contractAttribution(context.Background())["CSWAP"]; got != "soroswap" {
		t.Fatalf("attribution after recovery = %q, want soroswap", got)
	}
}

// A panicking registry read degrades to the last good map (or none) instead
// of crashing the process or blocking a caller that has no deadline.
func TestContractAttribution_PanickingReadDegrades(t *testing.T) {
	r := &countingContractsReader{}
	r.panc.Store(true)
	h := newAttributionHandler(r)
	if got := h.contractAttribution(context.Background()); len(got) != 0 {
		t.Fatalf("attribution on cold panic = %v, want empty", got)
	}

	r.panc.Store(false)
	r.idx = map[string]string{"CBLEND": "blend"}
	h.contractAttribution(context.Background())
	h.attribution.loadedAt = time.Now().Add(-2 * contractAttributionTTL)
	r.panc.Store(true)
	if got := h.contractAttribution(context.Background())["CBLEND"]; got != "blend" {
		t.Fatalf("attribution after panicking refresh = %q, want last good blend", got)
	}
}

// A request that has already gone away must not fail the shared refill.
func TestContractAttribution_CancelledCallerStillFills(t *testing.T) {
	r := &countingContractsReader{idx: map[string]string{"CBLEND": "blend"}}
	h := newAttributionHandler(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.contractAttribution(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if idx, fresh := h.attribution.get(); fresh && idx["CBLEND"] == "blend" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled caller left the cache empty")
		}
		time.Sleep(time.Millisecond)
	}
}

// A "ch:" code-history entry has a 24h TTL re-stamped daily, so under global-oldest-cachedAt eviction it is always
// oldest beside the 1-5 minute "pos:"/"act:" classes. Eviction must prefer an expired entry over any unexpired one.
func TestContractDetailCache_PutProtectsUnexpiredCodeHistory(t *testing.T) {
	c := &contractDetailCache{entries: make(map[string]contractDetailEntry, contractDetailCacheMax)}
	now := time.Now()

	const chKey = "ch:CONTRACTX"
	// Computed a while ago but well within its 24h TTL — still hot.
	c.entries[chKey] = contractDetailEntry{v: "code-history", cachedAt: now.Add(-20 * time.Hour)}

	// Fill the rest with "pos:" churn entries younger than the code-history entry but past their own 1-minute TTL, as
	// ordinary distinct-G-address explorer traffic produces.
	for i := 0; i < contractDetailCacheMax-1; i++ {
		key := positionsCacheKey + "G" + strconv.Itoa(i)
		c.entries[key] = contractDetailEntry{
			v:        i,
			cachedAt: now.Add(-time.Duration(2+i%5) * time.Minute),
		}
	}

	// Cache is now at capacity; one more insert forces an eviction.
	c.put(positionsCacheKey+"GNEW", "new")

	if _, ok := c.entries[chKey]; !ok {
		t.Fatalf("code-history entry %s was evicted ahead of an already-expired churn entry", chKey)
	}
}

// A stream of distinct account-activity keys (cheap for any minted G-address) must stay inside its own bound and leave a
// full set of unexpired contract entries untouched.
func TestContractDetailCache_ActivityKeysCannotEvictContractEntries(t *testing.T) {
	c := &contractDetailCache{}
	for i := range contractDetailCacheMax {
		c.put("ev:C"+strconv.Itoa(i), i)
	}
	for i := range contractDetailCacheMax + 1 {
		c.put(accountActivityCacheKey+"G"+strconv.Itoa(i), i)
	}
	for i := range contractDetailCacheMax {
		if _, ok, _ := c.get("ev:C" + strconv.Itoa(i)); !ok {
			t.Fatalf("contract entry ev:C%d evicted by account-activity keys", i)
		}
	}
	if got := len(c.activity); got != contractDetailCacheMax {
		t.Fatalf("account-activity map holds %d entries, want its bound %d", got, contractDetailCacheMax)
	}
}
