package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the task-#13 contract (2026-07-31): no user request
// ever pays for a cold protocol-analytics build, previously-built detail
// views stale-serve (never blank), and analytics degradation is explicit
// on the wire (analytics.status) instead of masquerading as zeros.

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

// TestPrewarmProtocolDetails_SweepWarmsEveryProtocolWindow verifies one
// full sweep builds EVERY registry protocol × every ?days= window into
// the detail cache (each entry healthy: analytics.status "ok", bespoke
// present) and that every rebuild observed the paired
// stellarindex_protocol_detail_refresh metrics (the wave-88…91
// counter+histogram pattern, asserted via obstest because per-label
// histogram children aren't Collectors).
func TestPrewarmProtocolDetails_SweepWarmsEveryProtocolWindow(t *testing.T) {
	oldPause := protocolDetailPrewarmPause
	protocolDetailPrewarmPause = 0
	defer func() { protocolDetailPrewarmPause = oldPause }()

	stub := &prewarmBespokeStub{}
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: stub})

	before := obstest.HistogramSampleCount(t, obs.ProtocolDetailRefreshDurationSeconds, "outcome", "ok")
	srv.PrewarmProtocolDetails(context.Background())

	wantKeys := len(protocolRegistry) * len(protocolBespokeWindows)
	srv.protoDetailMu.Lock()
	gotKeys := len(srv.protoDetailCache)
	for key, e := range srv.protoDetailCache {
		if e.view.Analytics == nil || e.view.Analytics.Status != protocolAnalyticsOK {
			t.Errorf("entry %q analytics = %+v, want status %q", key, e.view.Analytics, protocolAnalyticsOK)
		}
		if e.view.Bespoke == nil {
			t.Errorf("entry %q bespoke absent, want the stub block", key)
		}
	}
	srv.protoDetailMu.Unlock()
	if gotKeys != wantKeys {
		t.Fatalf("sweep cached %d keys, want %d (registry %d × windows %d)",
			gotKeys, wantKeys, len(protocolRegistry), len(protocolBespokeWindows))
	}
	for _, meta := range protocolRegistry {
		if got := stub.windows(meta.Name); len(got) != len(protocolBespokeWindows) {
			t.Errorf("%s bespoke built for windows %v, want all of %v", meta.Name, got, protocolBespokeWindows)
		}
	}
	// >= not ==: detached refreshes from other tests in this package may
	// still be draining and observing concurrently.
	after := obstest.HistogramSampleCount(t, obs.ProtocolDetailRefreshDurationSeconds, "outcome", "ok")
	if after-before < uint64(wantKeys) {
		t.Errorf("refresh histogram advanced %d, want >= %d", after-before, wantKeys)
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

// TestHandleProtocolDetail_StaleServeNeverBlanks verifies the SWR
// contract on the detail route: a previously-built view past its TTL is
// served IMMEDIATELY with flags.stale + analytics.status "stale" — the
// bespoke block never blanks — while one detached rebuild runs, after
// which the next request is fresh again.
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

// TestBuildProtocolDetail_DegradationIsExplicit verifies a build whose
// analytics components are missing stamps analytics.status
// "unavailable" — absence/zeros must be distinguishable from real zeros
// on the wire — and that stale-serving such an entry keeps the worse
// state (unavailable, not stale).
func TestBuildProtocolDetail_DegradationIsExplicit(t *testing.T) {
	srv := New(Options{}) // no analytics readers wired at all
	meta, ok := protocolByName("cctp")
	if !ok {
		t.Fatal("cctp missing from registry")
	}
	view := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if view.Analytics == nil || view.Analytics.Status != protocolAnalyticsUnavailable {
		t.Fatalf("analytics = %+v, want status %q", view.Analytics, protocolAnalyticsUnavailable)
	}
	if view.Analytics.AsOf == "" {
		t.Error("analytics.as_of empty, want build timestamp")
	}
	staled := staleProtocolDetail(view)
	if staled.Analytics.Status != protocolAnalyticsUnavailable {
		t.Errorf("stale overlay rewrote status %q → %q; unavailable must win over stale",
			protocolAnalyticsUnavailable, staled.Analytics.Status)
	}
}
