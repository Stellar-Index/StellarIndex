package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const alcIssuer = "GATISXX6BZ6NC7IKQBY37CJD4SOZL3CYZJWXEDG6JVIY4WBS6KXJHN6Q"

// countingListingReader is a listing reader with the precise-supply seam,
// counting every store read the listing makes.
type countingListingReader struct {
	AssetsReader
	lists, supply atomic.Int32
}

func (c *countingListingReader) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	c.lists.Add(1)
	price, volume := "1.0000000000", "500000.00"
	sources := 3
	return []timescale.AssetRow{{
		AssetID: "USDC-" + alcIssuer, Code: "USDC", IssuerGStrkey: alcIssuer,
		PriceUSD: &price, Volume24hUSD: &volume, SourceCount: &sources, ObservationCount: 9,
	}}, nil
}

func (c *countingListingReader) LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error) {
	c.supply.Add(1)
	return map[string]timescale.SupplyObservation{
		"USDC-" + alcIssuer: {CirculatingSupply: "10000000000", Basis: "issuer_exclusion"},
	}, nil
}

func (c *countingListingReader) GetAssetsPriceHistory7dBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	return map[string][]timescale.AssetPricePoint{}, nil
}

type countingDirectory struct {
	batches atomic.Int32
	fail    bool
}

func (d *countingDirectory) DirectoryEntryByAddress(context.Context, string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{}, false, nil
}

func (d *countingDirectory) DirectoryEntriesByAddresses(context.Context, []string) (map[string]timescale.DirectoryEntry, error) {
	d.batches.Add(1)
	if d.fail {
		return nil, errDirectoryDown
	}
	return map[string]timescale.DirectoryEntry{}, nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func newCountingListingServer(t *testing.T, dir *countingDirectory) (*Server, *countingListingReader, *fakeClock) {
	t.Helper()
	reader := &countingListingReader{}
	srv := New(Options{Directory: dir})
	srv.assetsReader = reader
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv.assetListCache.now = clock.now
	return srv, reader, clock
}

type alcResponse struct {
	cache string
	stale bool
	body  string
}

func getAssetList(t *testing.T, srv *Server, target string) alcResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	var env struct {
		Data  []AssetDetail `json:"data"`
		Flags Flags         `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].MarketCapUSD == nil {
		t.Fatalf("instrument: want the one row with its supply-derived market cap, got %+v", env.Data)
	}
	return alcResponse{cache: rec.Header().Get("X-Stellarindex-Cache"), stale: env.Flags.Stale, body: rec.Body.String()}
}

// A warm listing page is replayed whole: no supply read, no directory
// read, no row read. Those per-request overlays are what held the
// endpoint's p95 above the SLA while every other probed route sat at 20ms.
func TestAssetList_WarmPageMakesNoStoreReads(t *testing.T) {
	dir := &countingDirectory{}
	srv, reader, _ := newCountingListingServer(t, dir)

	first := getAssetList(t, srv, "/v1/assets?limit=100")
	if reader.lists.Load() != 1 || reader.supply.Load() != 1 || dir.batches.Load() != 1 {
		t.Fatalf("instrument: cold build read lists=%d supply=%d directory=%d, want 1 each",
			reader.lists.Load(), reader.supply.Load(), dir.batches.Load())
	}

	second := getAssetList(t, srv, "/v1/assets?limit=100")
	if n := reader.lists.Load() + reader.supply.Load() + dir.batches.Load(); n != 3 {
		t.Errorf("warm request made %d store reads, want 0 (lists=%d supply=%d directory=%d)",
			n-3, reader.lists.Load()-1, reader.supply.Load()-1, dir.batches.Load()-1)
	}
	if second.cache != "HIT" || second.body != first.body {
		t.Errorf("warm request cache=%q, body identical=%v; want a byte-identical HIT", second.cache, second.body == first.body)
	}

	// A different query is a different page.
	getAssetList(t, srv, "/v1/assets?limit=100&include=sparkline7d")
	if reader.lists.Load() != 2 {
		t.Errorf("a different query reused another page's slot (lists=%d, want 2)", reader.lists.Load())
	}
}

// Past the TTL the page is served at once, flagged stale, and exactly one
// background rebuild replaces it; past the max age it is rebuilt inline.
func TestAssetList_StaleReplayIsFlaggedAndRefreshedOnce(t *testing.T) {
	srv, reader, clock := newCountingListingServer(t, &countingDirectory{})

	if got := getAssetList(t, srv, "/v1/assets?limit=100"); got.stale {
		t.Fatal("cold build flagged stale")
	}
	clock.advance(assetListCacheTTL + time.Second)

	got := getAssetList(t, srv, "/v1/assets?limit=100")
	if !got.stale || got.cache != "HIT" {
		t.Errorf("replay past the TTL: cache=%q stale=%v, want a stale-flagged HIT", got.cache, got.stale)
	}
	getAssetList(t, srv, "/v1/assets?limit=100")

	deadline := time.Now().Add(5 * time.Second)
	for {
		fresh := getAssetList(t, srv, "/v1/assets?limit=100")
		if !fresh.stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never replaced the stale page")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := reader.lists.Load(); n != 2 {
		t.Errorf("rows read %d times, want 2 (cold build + one refresh for every stale replay)", n)
	}

	clock.advance(assetListCacheMaxAge + time.Second)
	if got := getAssetList(t, srv, "/v1/assets?limit=100"); got.stale || got.cache == "HIT" {
		t.Errorf("past max age: cache=%q stale=%v, want an inline rebuild", got.cache, got.stale)
	}
	if n := reader.lists.Load(); n != 3 {
		t.Errorf("rows read %d times past max age, want 3", n)
	}
}

// A page shaped by a failed read is answered but never replayed: it would
// keep withholding prices after the directory came back.
func TestAssetList_DegradedPageIsNotCached(t *testing.T) {
	dir := &countingDirectory{fail: true}
	srv, _, _ := newCountingListingServer(t, dir)

	for range 2 {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets?limit=100", nil))
		if rec.Code != http.StatusOK || rec.Header().Get("X-Stellarindex-Cache") == "HIT" {
			t.Fatalf("degraded page: status=%d cache=%q, want an uncached 200", rec.Code, rec.Header().Get("X-Stellarindex-Cache"))
		}
	}
	if n := dir.batches.Load(); n != 2 {
		t.Errorf("directory read %d times, want 2 — the degraded page was replayed", n)
	}
}

func TestAssetListCacheKey_CanonicalQueryOrder(t *testing.T) {
	a := httptest.NewRequest(http.MethodGet, "/v1/assets?limit=100&q=usd", nil)
	b := httptest.NewRequest(http.MethodGet, "/v1/assets?q=usd&limit=100", nil)
	c := httptest.NewRequest(http.MethodGet, "/v1/assets?q=usd&limit=100&include=sparkline7d", nil)
	if assetListCacheKey(a) != assetListCacheKey(b) {
		t.Error("parameter order split one page into two slots")
	}
	if assetListCacheKey(b) == assetListCacheKey(c) {
		t.Error("include= did not separate the slot")
	}
}

func TestAssetListCache_BoundedEntries(t *testing.T) {
	c := newAssetListResponseCache(assetListCacheTTL, assetListCacheMaxAge)
	for i := range assetListCacheMaxEntries + 10 {
		if err := c.put(string(rune('a'+i%26))+time.Duration(i).String(), Envelope{Data: []AssetDetail{}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.entries); n > assetListCacheMaxEntries {
		t.Errorf("cache holds %d entries, cap is %d", n, assetListCacheMaxEntries)
	}
}
