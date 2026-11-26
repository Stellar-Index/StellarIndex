package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// gatedNativeUpstream counts native-row reads and, while gate is set, holds each until it is closed.
type gatedNativeUpstream struct {
	*prewarmClockUpstream
	calls   atomic.Int64
	gate    atomic.Pointer[chan struct{}]
	entered chan struct{}
}

func (u *gatedNativeUpstream) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	u.calls.Add(1)
	if g := u.gate.Load(); g != nil {
		select {
		case u.entered <- struct{}{}:
		default:
		}
		<-*g
	}
	return u.prewarmClockUpstream.GetNativeAssetRow(ctx)
}

func (u *gatedNativeUpstream) hold() chan struct{} {
	g := make(chan struct{})
	u.gate.Store(&g)
	return g
}

// newDetailTTLServer is newPrewarmTestServer with the detail cache on the same fake clock and the native row gated; the first
// client GET has filled every catalogue key and the detail cache.
func newDetailTTLServer(t *testing.T) (*Server, *CachedAssetsReader, *gatedNativeUpstream, *atomic.Int64) {
	t.Helper()
	now := &atomic.Int64{}
	now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	up := &gatedNativeUpstream{
		prewarmClockUpstream: &prewarmClockUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, now: now},
		entered:              make(chan struct{}, 1),
	}
	c := NewCachedAssetsReader(up, AssetCatalogueTTL)
	c.single.now = clock
	s := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AssetsReader: c})
	s.assetDetailCache.now = clock
	if res := clientDetailGET(t, s, context.Background()); res.flags.Stale || res.noStore {
		t.Fatalf("cold GET: flags=%+v, want a clean build", res.flags)
	}
	settleSWRFlights(c)
	return s, c, up, now
}

type detailGETResult struct {
	hit     bool
	noStore bool
	flags   Flags
	body    string
}

// storedDetailEntry is the detail-cache entry for native, expired or not.
func storedDetailEntry(t *testing.T, s *Server) *assetDetailEntry {
	t.Helper()
	s.assetDetailCache.mu.RLock()
	defer s.assetDetailCache.mu.RUnlock()
	return s.assetDetailCache.entries[assetDetailCacheKey(mustAsset(t, "native"), false)]
}

// clientDetailGET serves one client GET; hit means it replayed the stored entry (every 200 here carries the HIT header).
func clientDetailGET(t *testing.T, s *Server, ctx context.Context) detailGETResult {
	t.Helper()
	before := storedDetailEntry(t, s)
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil).WithContext(ctx)
	req.RemoteAddr = "203.0.113.7:40000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	hit := before != nil && storedDetailEntry(t, s) == before
	if ctx.Err() != nil {
		return detailGETResult{}
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Flags Flags `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detailGETResult{
		hit:     hit,
		noStore: rec.Header().Get("Cache-Control") == "no-store",
		flags:   env.Flags,
		body:    rec.Body.String(),
	}
}

// pastStaleBound ages every catalogue value past ttl+min(ttl, budget), and the clean detail entry past its TTL.
func pastStaleBound(now *atomic.Int64) {
	now.Add(int64(AssetCatalogueTTL + cacheFillBudget + time.Second))
}

// A degraded body lives assetDetailDegradedTTL, not the clean TTL: within it a repeat replays it, after it the refreshed
// values rebuild a clean answer.
func TestAssetDetail_DegradedEntryYieldsToRefreshedAnswer(t *testing.T) {
	s, c, up, now := newDetailTTLServer(t)
	gate := up.hold()
	pastStaleBound(now)

	start := time.Now()
	first := clientDetailGET(t, s, context.Background())
	if waited := time.Since(start); waited > assetDetailAwaitBound+2*time.Second {
		t.Errorf("slow refresh held the request %v, want about %v", waited, assetDetailAwaitBound)
	}
	if !first.flags.Stale || !first.noStore || first.hit {
		t.Fatalf("first GET: flags=%+v no-store=%v hit=%v, want a stale no-store build", first.flags, first.noStore, first.hit)
	}

	now.Add(int64(assetDetailDegradedTTL - time.Second))
	burst := clientDetailGET(t, s, context.Background())
	if !burst.hit || burst.body != first.body || !burst.noStore {
		t.Errorf("repeat within %v: hit=%v same body=%v no-store=%v, want a no-store HIT of the stale body",
			assetDetailDegradedTTL, burst.hit, burst.body == first.body, burst.noStore)
	}

	close(gate)
	settleSWRFlights(c)
	now.Add(int64(time.Second + time.Millisecond))
	after := clientDetailGET(t, s, context.Background())
	if after.hit || after.flags.Stale || after.noStore {
		t.Errorf("after the refresh and %v: hit=%v flags=%+v no-store=%v, want a clean rebuild",
			assetDetailDegradedTTL, after.hit, after.flags, after.noStore)
	}
}

// A refresh that fails leaves the next rebuild flagged: the short life never hides a failure.
func TestAssetDetail_FailedRefreshStaysStaleAfterDegradedTTL(t *testing.T) {
	s, _, up, now := newDetailTTLServer(t)
	up.fail.Store(true)
	pastStaleBound(now)
	if first := clientDetailGET(t, s, context.Background()); !first.flags.Stale {
		t.Fatalf("failed refresh: first GET flags=%+v, want stale", first.flags)
	}
	now.Add(int64(assetDetailDegradedTTL + time.Second))
	next := clientDetailGET(t, s, context.Background())
	if next.hit || !next.flags.Stale || !next.noStore {
		t.Errorf("failed refresh, after %v: hit=%v flags=%+v no-store=%v, want a stale no-store rebuild",
			assetDetailDegradedTTL, next.hit, next.flags, next.noStore)
	}
}

// A clean entry keeps the full TTL.
func TestAssetDetail_CleanEntryKeepsFullTTL(t *testing.T) {
	s, _, _, now := newDetailTTLServer(t)
	now.Add(int64(s.assetDetailCache.ttl - time.Second))
	if res := clientDetailGET(t, s, context.Background()); !res.hit || res.flags.Stale {
		t.Errorf("clean entry inside its TTL: hit=%v flags=%+v, want a clean HIT", res.hit, res.flags)
	}
}

// A refresh that lands inside the bound makes the first ask after the stale bound complete.
func TestAssetDetail_FastRefreshAnswersCleanOnFirstAsk(t *testing.T) {
	s, _, _, now := newDetailTTLServer(t)
	pastStaleBound(now)
	if res := clientDetailGET(t, s, context.Background()); res.hit || res.flags.Stale || res.noStore {
		t.Errorf("first ask with a fast refresh: hit=%v flags=%+v no-store=%v, want clean", res.hit, res.flags, res.noStore)
	}
}

// Concurrent stale asks share one refresh per key, and a request that gives up does not cancel it.
func TestAssetDetail_StaleBurstSharesOneRefresh(t *testing.T) {
	s, c, up, now := newDetailTTLServer(t)
	gate := up.hold()
	pastStaleBound(now)
	before := up.calls.Load()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		clientDetailGET(t, s, ctx)
	}()
	<-up.entered
	cancel()
	<-done

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := clientDetailGET(t, s, context.Background()); !res.flags.Stale {
				t.Errorf("held refresh: flags=%+v, want stale", res.flags)
			}
		}()
	}
	wg.Wait()
	if got := up.calls.Load() - before; got != 1 {
		t.Errorf("native row read %d times by 51 stale asks, want 1", got)
	}

	close(gate)
	settleSWRFlights(c)
	now.Add(int64(assetDetailDegradedTTL + time.Second))
	if res := clientDetailGET(t, s, context.Background()); res.hit || res.flags.Stale {
		t.Errorf("after the cancelled ask's refresh landed: hit=%v flags=%+v, want clean", res.hit, res.flags)
	}
}
