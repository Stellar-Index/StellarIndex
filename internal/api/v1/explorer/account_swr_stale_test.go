package explorer

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The two account caches serve a value just past its TTL clean while a healthy refresh runs, and flag it once the
// last refresh failed or was skipped, or once it outlives TTL plus one refresh.

type accountStateSWRReader struct {
	*capReader
	cachedAt time.Time
	pastTTL  bool
	failed   bool
	refused  bool
}

func (r *accountStateSWRReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{Exists: true, Balance: 1, CachedAt: r.cachedAt, RefreshFailed: r.failed, RefreshRefused: r.refused}, r.pastTTL, nil
}

func serveAccountStateFlags(t *testing.T, reader *accountStateSWRReader) (stale, degraded bool, reason string) {
	t.Helper()
	h := newProbeHandler(reader, nil)
	written := false
	h.WriteJSON = func(w http.ResponseWriter, _ any, s bool) {
		stale, written = s, true
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSONAt = func(w http.ResponseWriter, _ any, s, d bool, _ time.Time) {
		stale, degraded, written = s, d, true
		obs.MarkResponseFlags(w, d, s)
		reason = obs.DegradedReasonOf(w)
		w.WriteHeader(http.StatusOK)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/accounts/{g_strkey}", h.AccountState)
	req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount, nil)
	obs.HTTPMetrics(obs.CaptureRoute(mux)).ServeHTTP(httptest.NewRecorder(), req)
	if !written {
		t.Fatal("AccountState wrote no body")
	}
	return stale, degraded, reason
}

func TestAccountState_SWRStaleVerdict(t *testing.T) {
	ttl := clickhouse.AccountStateCacheTTL
	bound := ttl + min(ttl, clickhouse.AccountStateRefreshTimeout)
	for _, tc := range []struct {
		name      string
		age       time.Duration
		pastTTL   bool
		failed    bool
		refused   bool
		wantStale bool
		want      string // degraded reason; "" = not degraded
	}{
		{"just past TTL, healthy refresh in flight", ttl + 5*time.Second, true, false, false, false, ""},
		{"last refresh failed, retry in flight", ttl + 5*time.Second, true, true, false, true, obs.DegradedRefreshFailed},
		{"refresh refused, nothing refreshing", ttl + 5*time.Second, true, false, true, true, obs.DegradedRefreshRefused},
		{"refused after a recorded failure keeps the failure", ttl + 5*time.Second, true, true, true, true, obs.DegradedRefreshFailed},
		{"refused past the hard bound", bound + time.Second, true, false, true, true, obs.DegradedRefreshRefused},
		{"past the hard bound", bound + time.Second, true, false, false, true, ""},
		{"fresh after a successful fill", time.Second, false, false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale, degraded, reason := serveAccountStateFlags(t, &accountStateSWRReader{
				capReader: &capReader{probe: &deadlineProbe{}},
				cachedAt:  time.Now().Add(-tc.age), pastTTL: tc.pastTTL, failed: tc.failed, refused: tc.refused,
			})
			if stale != tc.wantStale {
				t.Errorf("flags.stale = %t, want %t", stale, tc.wantStale)
			}
			if degraded != (tc.want != "") || reason != tc.want {
				t.Errorf("degraded=%t reason=%q, want reason %q", degraded, reason, tc.want)
			}
		})
	}
}

func TestAccountsList_SWRStaleVerdict(t *testing.T) {
	ttl := clickhouse.AccountsWealthCacheTTL
	bound := ttl + min(ttl, clickhouse.AccountsWealthRefreshTimeout)
	for _, tc := range []struct {
		name   string
		age    time.Duration
		failed bool
		want   bool
	}{
		{"just past TTL, healthy refresh in flight", ttl + time.Minute, false, false},
		{"last refresh failed, retry in flight", ttl + time.Minute, true, true},
		{"past the hard bound", bound + time.Second, false, true},
		{"fresh after a successful fill", time.Minute, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := wealthTestHandler(&wealthSnapshotReader{
				capReader: &capReader{probe: &deadlineProbe{}},
				rows:      []clickhouse.AccountWealth{{AccountID: validTestAccount, Value: big.NewRat(25, 2)}},
				asOf:      time.Now().Add(-tc.age), ok: true, failed: tc.failed,
			})
			var degraded bool
			h.WriteJSONAt = func(w http.ResponseWriter, _ any, s, d bool, _ time.Time) {
				rec.status, rec.stale, degraded = http.StatusOK, s, d
				w.WriteHeader(http.StatusOK)
			}
			serveAccountsList(t, h)
			if rec.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.status)
			}
			if rec.stale != tc.want || degraded != tc.want {
				t.Errorf("flags.stale=%t degraded=%t, want both %t", rec.stale, degraded, tc.want)
			}
		})
	}
}
