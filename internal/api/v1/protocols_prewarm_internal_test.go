package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin that no user request pays for a cold protocol-analytics build, built detail views stale-serve (never
// blank), and analytics degradation is explicit on the wire (analytics.status), not zeros.

// prewarmActivityStub is a minimal healthy lake reader: a readable tip
// and empty (but successful) fills — enough for buildProtocolDetail to
// stamp analytics.status "ok".
type prewarmActivityStub struct{}

func (prewarmActivityStub) LakeTipLedger(context.Context) (uint32, error) {
	return 10_000_000, nil
}

func (prewarmActivityStub) ProtocolEventBreakdown(context.Context, []string, uint32) ([]clickhouse.ProtocolEventTypeCount, error) {
	return nil, nil
}

func (prewarmActivityStub) ProtocolDailyActivity(context.Context, []string, uint32) ([]clickhouse.ProtocolDailyPoint, error) {
	return nil, nil
}

func (prewarmActivityStub) ProtocolContractActivity(context.Context, []string, uint32) ([]clickhouse.ProtocolContractActivity, error) {
	return nil, nil
}

// prewarmBespokeStub records which (source, window) builds ran and
// returns a one-KPI block so Bespoke is present on every view.
type prewarmBespokeStub struct {
	mu    sync.Mutex
	calls map[string][]int
}

func (s *prewarmBespokeStub) BuildProtocolBespoke(_ context.Context, source, category string, windowDays int) (*timescale.BespokeBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string][]int{}
	}
	s.calls[source] = append(s.calls[source], windowDays)
	return &timescale.BespokeBlock{
		Category: category,
		KPIs:     []timescale.BespokeKPI{{Label: "probe", Value: "1"}},
	}, nil
}

func (s *prewarmBespokeStub) windows(source string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.calls[source]...)
}

// waitProtoDetailIdle waits for a detached detail refresh for key to
// finish (its flight entry disappears once the rebuild goroutine ends).
func waitProtoDetailIdle(t *testing.T, s *Server, key string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.protoDetailMu.Lock()
		_, up := s.protoDetailFlight[key]
		s.protoDetailMu.Unlock()
		if !up {
			return
		}
		select {
		case <-deadline:
			t.Fatal("detached protocol-detail refresh never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type protoDetailEnvelope struct {
	Data  ProtocolDetailView `json:"data"`
	Flags Flags              `json:"flags"`
}

func getProtoDetail(t *testing.T, base, name string) (int, protoDetailEnvelope) {
	t.Helper()
	resp, err := http.Get(base + "/v1/protocols/" + name) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env protoDetailEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, env
}

// TestHandleProtocolDetail_StaleServeNeverBlanks verifies SWR on the detail route: a view past its TTL is served
// immediately with flags.stale and analytics.status "stale", bespoke block intact, while one detached rebuild runs;
// the next request is fresh.
func TestHandleProtocolDetail_StaleServeNeverBlanks(t *testing.T) {
	stub := &prewarmBespokeStub{}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Cold: the detached build fills the cache; served fresh.
	code, env := getProtoDetail(t, ts.URL, "cctp")
	if code != http.StatusOK {
		t.Fatalf("cold status = %d", code)
	}
	if env.Flags.Stale || env.Data.Analytics == nil || env.Data.Analytics.Status != protocolAnalyticsOK {
		t.Fatalf("cold: flags.stale=%v analytics=%+v, want fresh ok", env.Flags.Stale, env.Data.Analytics)
	}
	if env.Data.Bespoke == nil {
		t.Fatal("cold: bespoke absent")
	}

	// Age the entry past the TTL.
	key := protocolDetailCacheKey("cctp", protocolActivityWindowDays)
	srv.protoDetailMu.Lock()
	e, ok := srv.protoDetailCache[key]
	if !ok {
		srv.protoDetailMu.Unlock()
		t.Fatal("entry missing after cold build")
	}
	e.at = e.at.Add(-protocolDetailTTL - time.Minute)
	srv.protoDetailCache[key] = e
	srv.protoDetailMu.Unlock()

	// Stale: served instantly with honest markers, bespoke intact.
	code, env = getProtoDetail(t, ts.URL, "cctp")
	if code != http.StatusOK {
		t.Fatalf("stale status = %d", code)
	}
	if !env.Flags.Stale {
		t.Error("stale serve: flags.stale = false, want true")
	}
	if env.Data.Analytics == nil || env.Data.Analytics.Status != protocolAnalyticsStale {
		t.Errorf("stale serve: analytics = %+v, want status %q", env.Data.Analytics, protocolAnalyticsStale)
	}
	if env.Data.Bespoke == nil {
		t.Error("stale serve blanked the bespoke block — must serve the previous build")
	}

	// The kicked detached rebuild replaces the entry; next hit is fresh.
	waitProtoDetailIdle(t, srv, key)
	code, env = getProtoDetail(t, ts.URL, "cctp")
	if code != http.StatusOK || env.Flags.Stale {
		t.Fatalf("post-refresh: status=%d stale=%v, want fresh 200", code, env.Flags.Stale)
	}
	if env.Data.Analytics == nil || env.Data.Analytics.Status != protocolAnalyticsOK {
		t.Errorf("post-refresh analytics = %+v, want ok", env.Data.Analytics)
	}
}

func TestHandleProtocolDetail_StaleServeIsNoStore(t *testing.T) {
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: &prewarmBespokeStub{}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	cacheControl := func() string {
		resp, err := http.Get(ts.URL + "/v1/protocols/cctp") //nolint:noctx // test helper
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		return resp.Header.Get("Cache-Control")
	}

	if got := cacheControl(); got != "public, max-age=60" {
		t.Errorf("fresh build: Cache-Control = %q, want the handler's public band", got)
	}
	key := protocolDetailCacheKey("cctp", protocolActivityWindowDays)
	srv.protoDetailMu.Lock()
	e, ok := srv.protoDetailCache[key]
	if !ok {
		srv.protoDetailMu.Unlock()
		t.Fatal("entry missing after cold build")
	}
	e.at = e.at.Add(-protocolDetailTTL - time.Minute)
	srv.protoDetailCache[key] = e
	srv.protoDetailMu.Unlock()

	if got := cacheControl(); got != "no-store" {
		t.Errorf("stale serve: Cache-Control = %q, want no-store", got)
	}
	waitProtoDetailIdle(t, srv, key)
}

// TestHandleProtocolDetail_StaleBespokeSetsEnvelopeFlag pins that analytics.status "stale"
// always travels with flags.stale, even when the DETAIL entry is fresh: the age lives in the
// bespoke block, and a client must not read a last-good suite as live.
func TestHandleProtocolDetail_StaleBespokeSetsEnvelopeFlag(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})
	meta, _ := protocolByName("cctp")
	key := protocolBespokeCacheKey(meta.Name, meta.Category, protocolActivityWindowDays)
	ageBespokeEntry(t, srv, stub, key, &timescale.BespokeBlock{
		Category: meta.Category,
		KPIs:     []timescale.BespokeKPI{{Label: "probe", Value: "1"}},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code, env := getProtoDetail(t, ts.URL, "cctp")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if env.Data.Bespoke == nil {
		t.Fatal("stale bespoke block dropped — the suite must still render")
	}
	if env.Data.Analytics == nil || env.Data.Analytics.Status != protocolAnalyticsStale {
		t.Errorf("analytics = %+v, want status %q", env.Data.Analytics, protocolAnalyticsStale)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false with analytics.status=stale — the envelope must agree")
	}
	close(stub.block)
	waitBespokeIdle(t, srv, key)
}
