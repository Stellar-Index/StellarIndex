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
		{"stale price only", AssetDetail{priceStale: true}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := detailDegradedReason(&tc.d, tc.home, tc.dead); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
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
