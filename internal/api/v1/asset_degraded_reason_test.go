package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// issuedAssetReader is rtLean with the issuer set, which the directory lookup is keyed on.
type issuedAssetReader struct{ rtLean }

func (issuedAssetReader) GetAsset(_ context.Context, a canonical.Asset) (AssetDetail, error) {
	iss := a.Issuer
	return AssetDetail{AssetID: a.String(), Code: a.Code, Type: string(a.Type), Issuer: &iss}, nil
}

func degradedReasonOfGET(t *testing.T, h http.Handler, route, path string) (reason string, total float64) {
	t.Helper()
	before := map[string]float64{}
	for _, r := range allDegradedReasons {
		before[r] = testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	t.Logf("GET %s = %d cache-control=%q", path, rec.Code, rec.Header().Get("Cache-Control"))
	for _, r := range allDegradedReasons {
		if n := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r)) - before[r]; n > 0 {
			reason, total = r, total+n
		}
	}
	return reason, total
}

// A failed issuer-directory read withholds the page's pricing; the listing and the detail both name it.
func TestAssetRoutes_directoryUnreadIsCheckUnread(t *testing.T) {
	s := newRTServer(t, false, nil)
	s.Directory = failingDirectory{}
	detail := New(Options{Assets: issuedAssetReader{}, Directory: failingDirectory{}}).Handler()
	for _, tc := range []struct {
		route, path string
		h           http.Handler
	}{
		{"/v1/assets", "/v1/assets?limit=2", s.h},
		{"/v1/assets/{asset_id}", "/v1/assets/" + rtZED, detail},
	} {
		t.Run(tc.route, func(t *testing.T) {
			got, total := degradedReasonOfGET(t, tc.h, tc.route, tc.path)
			if got != obs.DegradedCheckUnread || total != 1 {
				t.Errorf("reason = %q (counted %v), want %q once", got, total, obs.DegradedCheckUnread)
			}
		})
	}
}

func TestAssetRoutes_healthyRecordsNoReason(t *testing.T) {
	s := New(Options{Assets: rtLean{}, Directory: fixedDirectory{}})
	h := s.Handler()
	for _, tc := range []struct{ route, path string }{
		{"/v1/assets", "/v1/assets?limit=2"},
		{"/v1/assets/{asset_id}", "/v1/assets/" + rtFOO},
	} {
		if got, total := degradedReasonOfGET(t, h, tc.route, tc.path); total != 0 {
			t.Errorf("%s healthy response counted %q", tc.route, got)
		}
	}
}

// Each cause of a degraded detail maps to one reason, and the highest-precedence cause wins.
func TestDetailDegradedReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    AssetDetail
		home bool
		dead bool
		want string
	}{
		{"extension deadline", AssetDetail{extensionUnread: true}, false, false, obs.DegradedSectionDropped},
		{"issuer directory", AssetDetail{issuerDirectoryUnchecked: true}, false, false, obs.DegradedCheckUnread},
		{"directory beats extension", AssetDetail{issuerDirectoryUnchecked: true, extensionUnread: true}, false, false, obs.DegradedCheckUnread},
		{"extension beats f2", AssetDetail{extensionUnread: true, f2ReadFailed: true}, false, false, obs.DegradedSectionDropped},
		{"f2", AssetDetail{f2ReadFailed: true}, false, false, obs.DegradedPartialRead},
		{"sep1", AssetDetail{sep1Unread: true}, false, false, obs.DegradedPartialRead},
		{"home domain", AssetDetail{}, true, false, obs.DegradedPartialRead},
		{"dead context", AssetDetail{}, false, true, obs.DegradedRequestCanceled},
		{"stale price", AssetDetail{priceStale: true}, false, false, obs.DegradedSourceStale},
		{"stale supply", AssetDetail{SupplyStale: true}, false, false, obs.DegradedSourceStale},
		{"aged catalogue value is not a cause", AssetDetail{extensionStale: true}, false, false, ""},
		{"aged catalogue value under stale supply", AssetDetail{extensionStale: true, SupplyStale: true}, false, false, obs.DegradedSourceStale},
		{"failed catalogue refresh", AssetDetail{extensionStale: true, extensionLastGood: obs.DegradedRefreshFailed}, false, false, obs.DegradedRefreshFailed},
		{"overrun beats source_stale", AssetDetail{extensionLastGood: obs.DegradedRefreshOverrun, SupplyStale: true}, false, false, obs.DegradedRefreshOverrun},
		{"decimals unresolved", AssetDetail{DecimalsUnresolved: true}, false, false, obs.DegradedPartialRead},
		{"all three plus directory", AssetDetail{extensionStale: true, SupplyStale: true, issuerDirectoryUnchecked: true}, false, false, obs.DegradedCheckUnread},
		{"nothing", AssetDetail{}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := detailDegradedReason(&tc.d, tc.home, tc.dead); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// A stored entry served by the real handler counts its stored reason, so a last-good catalogue value is named on a hit.
func TestAssetDetailHit_replaysStoredReason(t *testing.T) {
	s := New(Options{Assets: issuedAssetReader{}})
	parsed := mustParseAsset(rtZED)
	d := AssetDetail{AssetID: rtZED, Code: "ZED", extensionStale: true, SupplyStale: true}
	reason := detailDegradedReason(&d, false, false)
	s.assetDetailCache.put(assetDetailCacheKey(parsed, false), newReasonedAssetDetailEntry(d, []byte("{}\n"), Flags{Stale: true, Degraded: true}, reason))
	if got, total := degradedReasonOfGET(t, s.Handler(), "/v1/assets/{asset_id}", "/v1/assets/"+rtZED); got != obs.DegradedSourceStale || total != 1 {
		t.Errorf("reason = %q (counted %v), want %q once", got, total, obs.DegradedSourceStale)
	}
	d = AssetDetail{AssetID: rtZED, Code: "ZED", extensionStale: true, extensionLastGood: obs.DegradedRefreshOverrun}
	s.assetDetailCache.put(assetDetailCacheKey(parsed, false), newReasonedAssetDetailEntry(d, []byte("{}\n"), Flags{Stale: true, Degraded: true}, detailDegradedReason(&d, false, false)))
	if got, total := degradedReasonOfGET(t, s.Handler(), "/v1/assets/{asset_id}", "/v1/assets/"+rtZED); got != obs.DegradedRefreshOverrun || total != 1 {
		t.Errorf("reason = %q (counted %v), want %q once", got, total, obs.DegradedRefreshOverrun)
	}
}

// A cached degraded entry replays its reason, and the extension deadline is counted section_dropped end to end.
func TestCachedDetailReplaysItsReason(t *testing.T) {
	d := AssetDetail{AssetID: rtFOO, extensionUnread: true, issuerDirectoryUnchecked: false}
	entry := newAssetDetailEntry(d, []byte("{}\n"), Flags{Stale: true, Degraded: true})
	entry.reason = detailDegradedReason(&d, false, false)
	got := serveDegraded(t, "/t/detail-replay", func(_ context.Context, w http.ResponseWriter) bool {
		writeCachedAssetDetail(w, entry)
		return false
	})
	if got != obs.DegradedSectionDropped {
		t.Errorf("replayed reason = %q, want %q", got, obs.DegradedSectionDropped)
	}
}
