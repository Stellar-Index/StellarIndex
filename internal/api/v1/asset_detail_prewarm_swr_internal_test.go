package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// prewarmClockUpstream is a catalogue whose native-row read takes refreshTook of fake time and fails while fail is set.
type prewarmClockUpstream struct {
	*fakeAssetsUpstream
	now  *atomic.Int64
	fail atomic.Bool
}

const refreshTook = 300 * time.Millisecond

func (u *prewarmClockUpstream) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	u.now.Add(int64(refreshTook))
	if u.fail.Load() {
		return timescale.AssetRow{}, errors.New("down")
	}
	return timescale.AssetRow{AssetID: "native"}, nil
}

// newPrewarmTestServer serves /v1/assets/native over an AssetCatalogueTTL catalogue on a fake clock.
func newPrewarmTestServer(t *testing.T) (*Server, *CachedAssetsReader, *prewarmClockUpstream, *atomic.Int64) {
	t.Helper()
	now := &atomic.Int64{}
	now.Store(time.Now().UnixNano())
	up := &prewarmClockUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, now: now}
	c := NewCachedAssetsReader(up, AssetCatalogueTTL)
	c.single.now = func() time.Time { return time.Unix(0, now.Load()) }
	s := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AssetsReader: c})
	return s, c, up, now
}

// settleSWRFlights waits for every refresh in flight on c's single-value cache, so the next tick starts from settled entries.
func settleSWRFlights(c *CachedAssetsReader) {
	c.single.mu.Lock()
	var flights []chan struct{}
	for _, e := range c.single.entries {
		if e.flight != nil {
			flights = append(flights, e.flight)
		}
	}
	c.single.mu.Unlock()
	for _, f := range flights {
		<-f
	}
}

// prewarmRender runs one self-prewarm render of /v1/assets/native and returns the detail-cache entry it left.
func prewarmRender(t *testing.T, s *Server) *assetDetailEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("User-Agent", "stellarindex-prewarm/1")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	entry, _ := s.assetDetailCache.get(assetDetailCacheKey(mustAsset(t, "native"), false))
	return entry
}

// A key read only by the 60s self-prewarm is found past TTL+min(TTL, budget) at some ticks; the prewarm has no latency
// constraint, so it refreshes before rendering and neither serves nor caches a stale-flagged detail.
func TestAssetDetail_PrewarmCadenceNeverRendersStale(t *testing.T) {
	s, c, _, now := newPrewarmTestServer(t)
	start := now.Load()
	for minute := 0; minute <= 12; minute++ {
		// The ticker fires on its own schedule, so a refresh's duration shifts the entry's age at the next ticks.
		now.Store(start + int64(minute)*int64(time.Minute))
		entry := prewarmRender(t, s)
		settleSWRFlights(c)
		if entry == nil {
			t.Fatalf("minute %d: a healthy prewarm render was not cached", minute)
		}
		if entry.detail.extensionStale || entry.stale || entry.degraded {
			t.Fatalf("minute %d: prewarm cached a flagged render (extensionStale=%v stale=%v degraded=%v)",
				minute, entry.detail.extensionStale, entry.stale, entry.degraded)
		}
	}
}

// A prewarm whose refresh fails still renders the last-good value flagged: waiting never hides a failure.
func TestAssetDetail_PrewarmFailedRefreshIsFlagged(t *testing.T) {
	s, c, up, now := newPrewarmTestServer(t)
	prewarmRender(t, s)
	settleSWRFlights(c)
	up.fail.Store(true)
	now.Add(int64(AssetCatalogueTTL + time.Second))
	entry := prewarmRender(t, s)
	if entry == nil {
		t.Fatal("failed refresh: no cached entry")
	}
	if !entry.detail.extensionStale || !entry.stale {
		t.Errorf("failed refresh: extensionStale=%v stale=%v, want both", entry.detail.extensionStale, entry.stale)
	}
}

// Outside the prewarm the read stays stale-while-revalidate: a value past its bound is served at once, flagged.
func TestAssetDetail_OverdueValueStaysFlaggedForClients(t *testing.T) {
	s, c, _, now := newPrewarmTestServer(t)
	native := mustAsset(t, "native")
	var d AssetDetail
	s.applyAssetExtensionFields(context.Background(), &d, native)
	settleSWRFlights(c)
	now.Add(int64(AssetCatalogueTTL + cacheFillBudget + time.Second))
	s.applyAssetExtensionFields(context.Background(), &d, native)
	if !d.extensionStale {
		t.Error("a client read past the bound was served unflagged")
	}
	settleSWRFlights(c)
	var p AssetDetail
	now.Add(int64(AssetCatalogueTTL + cacheFillBudget + time.Second))
	s.applyAssetExtensionFields(withAwaitRefresh(context.Background()), &p, native)
	if p.extensionStale {
		t.Error("a prewarm read past the bound was not refreshed before it was served")
	}
}
