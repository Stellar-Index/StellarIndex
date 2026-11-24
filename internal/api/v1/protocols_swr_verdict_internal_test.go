package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A protocol detail served inside its TTL while its early rebuild runs on time is clean and cacheable; after a failed
// rebuild the same age is stale and no-store; a complete rebuild clears the failure. Past the TTL it is stale even with the
// rebuild on time: the inputs it froze at build must not be served clean longer than the TTL.
func TestHandleProtocolDetail_StaleFollowsTheSWRVerdict(t *testing.T) {
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: &prewarmBespokeStub{}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	key := protocolDetailCacheKey("cctp", protocolActivityWindowDays)
	if code, _ := getProtoDetail(t, ts.URL, "cctp"); code != http.StatusOK {
		t.Fatalf("cold: status %d", code)
	}
	waitProtoDetailIdle(t, srv, key)
	ageBy := protocolDetailRefreshAfter + protocolDetailRefreshTimeout/2
	age := func(failed bool) {
		srv.protoDetailMu.Lock()
		e := srv.protoDetailCache[key]
		e.at, e.refreshFailed = time.Now().Add(-ageBy), failed
		srv.protoDetailCache[key] = e
		srv.protoDetailMu.Unlock()
	}
	serve := func(name string, want bool) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/protocols/cctp") //nolint:noctx // test helper
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if noStore := resp.Header.Get("Cache-Control") == "no-store"; noStore != want {
			t.Errorf("%s: no-store = %v, want %v", name, !want, want)
		}
		waitProtoDetailIdle(t, srv, key)
	}
	serveFlag := func(name string, want bool) {
		t.Helper()
		_, env := getProtoDetail(t, ts.URL, "cctp")
		if env.Flags.Stale != want || (env.Data.Analytics.Status == protocolAnalyticsStale) != want {
			t.Errorf("%s: flags.stale = %v analytics = %q, want stale %v", name, env.Flags.Stale, env.Data.Analytics.Status, want)
		}
		waitProtoDetailIdle(t, srv, key)
	}

	age(false)
	serve("just past ttl, rebuild on time", false)
	age(false)
	serveFlag("just past ttl, rebuild on time", false)
	age(true)
	serve("previous rebuild failed, retry running", true)
	age(true)
	serveFlag("previous rebuild failed, retry running", true)

	// That serve's rebuild completed healthy, so the failure is gone.
	srv.protoDetailMu.Lock()
	failed := srv.protoDetailCache[key].refreshFailed
	srv.protoDetailMu.Unlock()
	if failed {
		t.Fatal("a complete rebuild left the failure memory set")
	}
	age(false)
	serveFlag("after a successful rebuild", false)

	ageBy = protocolDetailTTL + protocolDetailRefreshTimeout/2
	age(false)
	serveFlag("past the ttl, rebuild on time", true)
}

// A rebuild that panics marks the kept entry failed.
func TestProtoDetailRefresh_PanicIsAFailure(t *testing.T) {
	srv := New(Options{})
	key := "k"
	srv.protoDetailMu.Lock()
	srv.protoDetailInitLocked()
	srv.protoDetailCache[key] = protoDetailEntry{at: time.Now().Add(-protocolDetailTTL - time.Second)}
	done := srv.protoDetailRefreshLocked(key, func(_ context.Context) ProtocolDetailView { panic("boom") })
	srv.protoDetailMu.Unlock()
	<-done
	srv.protoDetailMu.Lock()
	defer srv.protoDetailMu.Unlock()
	if !srv.protoDetailCache[key].refreshFailed {
		t.Error("a panicked rebuild left the entry unmarked")
	}
}
