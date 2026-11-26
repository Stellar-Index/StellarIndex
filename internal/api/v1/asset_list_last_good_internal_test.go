package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flakyListingReader is countingListingReader whose listing read fails while fail is set.
type flakyListingReader struct {
	*countingListingReader
	fail atomic.Bool
}

func (f *flakyListingReader) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if f.fail.Load() {
		return nil, errors.New("listing read failed")
	}
	return f.countingListingReader.ListAssetsExt(ctx, opts)
}

type listingAnswer struct {
	stale               bool
	cache, cacheControl string
	reason              string
	counted             float64
}

// getListingAnswer serves target and reads its stale flag, headers and the one degraded reason it counted, if any.
func getListingAnswer(t *testing.T, srv *Server, target string) listingAnswer {
	t.Helper()
	before := map[string]float64{}
	for _, r := range allDegradedReasons {
		before[r] = testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues("/v1/assets", r))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	var env struct {
		Flags Flags `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := listingAnswer{
		stale: env.Flags.Stale, cache: rec.Header().Get("X-Stellarindex-Cache"), cacheControl: rec.Header().Get("Cache-Control"),
	}
	for _, r := range allDegradedReasons {
		if n := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues("/v1/assets", r)) - before[r]; n > 0 {
			a.reason, a.counted = r, a.counted+n
		}
	}
	return a
}

// newLastGoodListing serves target once over a real catalogue rows cache, then ages those rows past TTL plus budget, applies
// state to them and drops the built pages, so the next ask is an unwarmed page key over old rows.
func newLastGoodListing(t *testing.T, dir *countingDirectory, target string, state func(*assetsCacheEntry, *flakyListingReader)) *Server {
	t.Helper()
	srv, reader, _ := newCountingListingServer(t, dir)
	flaky := &flakyListingReader{countingListingReader: reader}
	catalogue := NewCachedAssetsReader(flaky, AssetCatalogueTTL)
	srv.AssetsReader = catalogue
	getAssetList(t, srv, target)
	waitAssetListSettled(t, srv)
	catalogue.mu.Lock()
	for _, e := range catalogue.entries {
		e.at = time.Now().Add(-10 * time.Minute)
		state(e, flaky)
	}
	catalogue.mu.Unlock()
	srv.assetListCache.mu.Lock()
	clear(srv.assetListCache.entries)
	srv.assetListCache.mu.Unlock()
	return srv
}

var lastGoodTargets = []string{"/v1/assets?limit=25", "/v1/assets?asset_class=all&limit=25"}

// Rows past their bound with no refresh running are stale: the ask that finds them starts the refresh, so nothing has
// failed or overrun, and the page is neither degraded nor no-store.
func TestAssetList_IdleOldRowsAreStaleNotDegraded(t *testing.T) {
	for _, target := range lastGoodTargets {
		t.Run(target, func(t *testing.T) {
			srv := newLastGoodListing(t, &countingDirectory{}, target, func(*assetsCacheEntry, *flakyListingReader) {})
			got := getListingAnswer(t, srv, target)
			if !got.stale || got.counted != 0 || got.cacheControl == "no-store" {
				t.Errorf("idle old rows: stale=%v counted=%v (%q) cache-control=%q, want stale, uncounted, cacheable",
					got.stale, got.counted, got.reason, got.cacheControl)
			}
		})
	}
}

func TestAssetList_LastGoodRowsAreDegradedWithTheirReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(*assetsCacheEntry, *flakyListingReader)
		want  string
	}{
		{"refresh failed", func(e *assetsCacheEntry, f *flakyListingReader) {
			e.refreshFailed = true
			f.fail.Store(true) // the retry this ask starts fails too
		}, obs.DegradedRefreshFailed},
		{"refresh overrun", func(e *assetsCacheEntry, _ *flakyListingReader) {
			e.flight, e.flightAt = make(chan struct{}), time.Now().Add(-assetsRefreshBudget-time.Second)
		}, obs.DegradedRefreshOverrun},
	} {
		for _, target := range lastGoodTargets {
			t.Run(tc.name+" "+target, func(t *testing.T) {
				srv := newLastGoodListing(t, &countingDirectory{}, target, tc.state)
				got := getListingAnswer(t, srv, target)
				if !got.stale || got.reason != tc.want || got.counted != 1 || got.cacheControl != "no-store" {
					t.Errorf("stale=%v reason=%q counted=%v cache-control=%q, want stale, %q once, no-store",
						got.stale, got.reason, got.counted, got.cacheControl, tc.want)
				}
			})
		}
	}
}

// The idle-old-rows page carries the header a stale-only replay of a cached page carries.
func TestAssetList_IdleOldRowsHeaderMatchesStaleReplay(t *testing.T) {
	const target = "/v1/assets?limit=25"
	srv, _, clock := newCountingListingServer(t, &countingDirectory{})
	getAssetList(t, srv, target)
	clock.advance(assetListCacheTTL + min(assetListCacheTTL, cacheFillBudget) + time.Second)
	replay := getListingAnswer(t, srv, target)
	waitAssetListSettled(t, srv)
	if !replay.stale || replay.cache != "HIT" {
		t.Fatalf("replay: cache=%q stale=%v, want a stale-flagged HIT", replay.cache, replay.stale)
	}
	idle := getListingAnswer(t, newLastGoodListing(t, &countingDirectory{}, target, func(*assetsCacheEntry, *flakyListingReader) {}), target)
	if idle.cacheControl != replay.cacheControl {
		t.Errorf("idle old rows cache-control=%q, stale replay %q", idle.cacheControl, replay.cacheControl)
	}
}

// Every other degraded exit of the listing names its cause, on the classic and the unified page alike.
func TestAssetList_DegradedExitsNameTheirReason(t *testing.T) {
	for _, target := range lastGoodTargets {
		t.Run("unmeasured "+target, func(t *testing.T) {
			srv, _, _ := newCountingListingServer(t, &countingDirectory{})
			srv.Substance = &unmeasuredGate{}
			if got := getListingAnswer(t, srv, target); got.reason != obs.DegradedPartialRead || got.counted != 1 {
				t.Errorf("reason=%q counted=%v, want %q once", got.reason, got.counted, obs.DegradedPartialRead)
			}
		})
		t.Run("directory unchecked "+target, func(t *testing.T) {
			srv, _, _ := newCountingListingServer(t, &countingDirectory{fail: true})
			if got := getListingAnswer(t, srv, target); got.reason != obs.DegradedCheckUnread || got.counted != 1 {
				t.Errorf("reason=%q counted=%v, want %q once", got.reason, got.counted, obs.DegradedCheckUnread)
			}
		})
	}
}
