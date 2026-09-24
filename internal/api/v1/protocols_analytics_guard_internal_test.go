package v1

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the 2026-07-31 audit fixes on the protocol-detail
// analytics path: a degraded FAST-failing rebuild must never displace a
// previously-good cached entry; the daily-pre-aggregation probe must not
// latch a transient error for the process lifetime; and the three
// analytics fills must share one tip read + one fast-vs-raw decision.

// TestProtoDetailRefresh_FastFailKeepsGoodEntry: a rebuild that fails
// FAST (store down → enrich errors in ms, rctx.Err() == nil) must keep
// the previously-good entry — before the fix the rctx.Err()-only guard
// let one prewarm sweep during a ClickHouse outage blank every protocol
// page while stamping the blanks fresh.
func TestProtoDetailRefresh_FastFailKeepsGoodEntry(t *testing.T) {
	srv := New(Options{})
	key := protocolDetailCacheKey("cctp", protocolActivityWindowDays)
	good := ProtocolDetailView{
		Bespoke:   &ProtocolBespoke{Category: "bridge", KPIs: []BespokeKPI{{Label: "probe", Value: "1"}}},
		Analytics: &ProtocolAnalyticsStatus{Status: protocolAnalyticsOK, AsOf: "2026-07-31T00:00:00Z"},
	}
	degradedBuild := func(context.Context) ProtocolDetailView {
		// Fast failure: returns immediately with an analytics-empty view.
		return ProtocolDetailView{Analytics: &ProtocolAnalyticsStatus{Status: protocolAnalyticsUnavailable}}
	}

	srv.protoDetailMu.Lock()
	srv.protoDetailInitLocked()
	srv.protoDetailCache[key] = protoDetailEntry{view: good, at: time.Now().Add(-protocolDetailTTL - time.Minute)}
	done := srv.protoDetailRefreshLocked(key, degradedBuild)
	srv.protoDetailMu.Unlock()
	<-done

	srv.protoDetailMu.Lock()
	e, has := srv.protoDetailCache[key]
	srv.protoDetailMu.Unlock()
	if !has || e.view.Analytics == nil || e.view.Analytics.Status != protocolAnalyticsOK || e.view.Bespoke == nil {
		t.Fatalf("degraded fast-fail rebuild displaced the good entry: %+v", e.view.Analytics)
	}

	// The SWR read path serves the kept entry STALE (it kicks another
	// degraded rebuild, which again must not displace it).
	view, stale, ok := srv.cachedProtocolDetail(context.Background(), key, degradedBuild)
	if !ok || !stale {
		t.Fatalf("cachedProtocolDetail = stale %v ok %v, want stale serve of the kept entry", stale, ok)
	}
	if view.Analytics == nil || view.Analytics.Status != protocolAnalyticsOK || view.Bespoke == nil {
		t.Fatalf("stale serve returned %+v, want the previous good view", view.Analytics)
	}
	waitProtoDetailIdle(t, srv, key)

	// A degraded build may still populate a stone-cold key (registry-only
	// beats 503) …
	coldKey := protocolDetailCacheKey("cctp", 7)
	srv.protoDetailMu.Lock()
	done = srv.protoDetailRefreshLocked(coldKey, degradedBuild)
	srv.protoDetailMu.Unlock()
	<-done
	srv.protoDetailMu.Lock()
	_, has = srv.protoDetailCache[coldKey]
	srv.protoDetailMu.Unlock()
	if !has {
		t.Fatal("degraded build did not populate a cold key")
	}

	// … and a HEALTHY rebuild replaces the kept entry again.
	srv.protoDetailMu.Lock()
	done = srv.protoDetailRefreshLocked(key, func(context.Context) ProtocolDetailView {
		return ProtocolDetailView{Analytics: &ProtocolAnalyticsStatus{Status: protocolAnalyticsOK, AsOf: "2026-07-31T01:00:00Z"}}
	})
	srv.protoDetailMu.Unlock()
	<-done
	srv.protoDetailMu.Lock()
	e = srv.protoDetailCache[key]
	srv.protoDetailMu.Unlock()
	if e.view.Analytics == nil || e.view.Analytics.AsOf != "2026-07-31T01:00:00Z" {
		t.Fatalf("healthy rebuild did not replace the entry: %+v", e.view.Analytics)
	}
}

// probeAnswer scripts one DailyActivityAvailable response.
type probeAnswer struct{ avail, definitive bool }

// scriptedFastStub is a lake reader whose fast-availability probe follows
// a script, recording probe/tip/fast-read call counts.
type scriptedFastStub struct {
	prewarmActivityStub

	mu           sync.Mutex
	answers      []probeAnswer
	probeCalls   int
	tipCalls     int
	fastSeries   int
	fastBreak    int
	fastContract int
	rawSeries    int
	rawBreak     int
	seriesPoint  []clickhouse.ProtocolDailyPoint
}

func (s *scriptedFastStub) DailyActivityAvailable(context.Context) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.probeCalls
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	s.probeCalls++
	return s.answers[i].avail, s.answers[i].definitive
}

func (s *scriptedFastStub) LakeTipLedger(context.Context) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tipCalls++
	return 10_000_000, nil
}

func (s *scriptedFastStub) ProtocolDailyActivityFast(context.Context, []string, time.Time) ([]clickhouse.ProtocolDailyPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fastSeries++
	return s.seriesPoint, nil
}

func (s *scriptedFastStub) ProtocolEventBreakdownFast(context.Context, []string, time.Time) ([]clickhouse.ProtocolEventTypeCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fastBreak++
	return nil, nil
}

func (s *scriptedFastStub) ProtocolContractActivityFast(context.Context, []string, time.Time) ([]clickhouse.ProtocolContractActivity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fastContract++
	return nil, nil
}

func (s *scriptedFastStub) ProtocolDailyActivity(context.Context, []string, uint32) ([]clickhouse.ProtocolDailyPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawSeries++
	return nil, nil
}

func (s *scriptedFastStub) ProtocolEventBreakdown(context.Context, []string, uint32) ([]clickhouse.ProtocolEventTypeCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawBreak++
	return nil, nil
}

// TestFastActivity_TransientProbeErrorIsNotLatched: a transient error on
// the FIRST availability probe (definitive=false) must degrade only that
// call — the next call re-probes and can settle on the fast path. Before
// the fix a sync.Once latched the first answer forever (raw 12B-row
// scans for the process lifetime after one ClickHouse blip).
func TestFastActivity_TransientProbeErrorIsNotLatched(t *testing.T) {
	stub := &scriptedFastStub{answers: []probeAnswer{{false, false}, {true, true}}}
	srv := New(Options{ProtocolActivity: stub})
	ctx := context.Background()

	if got := srv.fastActivity(ctx); got != nil {
		t.Fatal("transient probe failure must degrade this call to raw (nil)")
	}
	if got := srv.fastActivity(ctx); got == nil {
		t.Fatal("recovered probe must enable the fast path — the transient answer was latched")
	}
	if got := srv.fastActivity(ctx); got == nil {
		t.Fatal("settled fast path lost")
	}
	stub.mu.Lock()
	probes := stub.probeCalls
	stub.mu.Unlock()
	if probes != 2 {
		t.Fatalf("probe calls = %d, want 2 (one transient, one definitive; the settled answer is cached)", probes)
	}
}

// TestFastActivity_DefinitiveAbsenceIsCached: a definitive "table
// missing" answer settles false without re-probing every call.
func TestFastActivity_DefinitiveAbsenceIsCached(t *testing.T) {
	stub := &scriptedFastStub{answers: []probeAnswer{{false, true}, {true, true}}}
	srv := New(Options{ProtocolActivity: stub})
	ctx := context.Background()
	if srv.fastActivity(ctx) != nil || srv.fastActivity(ctx) != nil {
		t.Fatal("definitive absence must pin the raw path")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1 (definitive answer cached)", stub.probeCalls)
	}
}

// erroringCountReader is a contracts reader whose roster reads always
// succeed (empty) but whose count-without-enumeration path always fails —
// isolates detailContractCount's error from the roster build.
type erroringCountReader struct{ err error }

func (erroringCountReader) ListProtocolContracts(context.Context, string) ([]timescale.ProtocolContract, error) {
	return nil, nil
}

func (erroringCountReader) ListSourceContractsFromProjection(context.Context, string) ([]string, error) {
	return nil, nil
}

func (erroringCountReader) ProtocolContractIndex(context.Context) (map[string]string, error) {
	return nil, nil
}

func (r erroringCountReader) CountSourceContracts(context.Context, string) (int64, bool, error) {
	return 0, false, r.err
}

// erroringStatsReader is a ProtocolStatsReader that always fails.
type erroringStatsReader struct{ err error }

func (r erroringStatsReader) CountRecentEventsBySource(context.Context) (map[string]int64, error) {
	return nil, r.err
}

// TestBuildProtocolDetail_ContractCountErrorDegradesStatus: a failed
// contract-count read must flip analytics.status to "unavailable" even
// when the lake analytics and bespoke block both build healthy — before
// the fix detailContractCount degraded to len(roster) silently and never
// fed the failure into status, so the page read "ok" while serving a
// wrong contract_count.
func TestBuildProtocolDetail_ContractCountErrorDegradesStatus(t *testing.T) {
	meta, ok := protocolByName("cctp")
	if !ok {
		t.Fatal("cctp missing from registry")
	}
	srv := New(Options{
		ProtocolActivity:  prewarmActivityStub{},
		ProtocolBespoke:   &bespokeStub{},
		ProtocolContracts: erroringCountReader{err: errors.New("count read failed")},
	})
	v := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if v.Analytics == nil || v.Analytics.Status != protocolAnalyticsUnavailable {
		t.Fatalf("status = %+v, want %q: a failed contract count must not read as a healthy build",
			v.Analytics, protocolAnalyticsUnavailable)
	}
}

// erroringRosterReader is a contracts reader whose roster ENUMERATION always
// fails and has no count-without-enumeration path wired (no
// CountSourceContracts, so it does not satisfy protocolContractCounter) —
// the common shape for a source with no counter (every source except
// sorocredit). Isolates a bare roster-read failure from the count-reader
// failure erroringCountReader exercises above.
type erroringRosterReader struct{ err error }

func (r erroringRosterReader) ListProtocolContracts(context.Context, string) ([]timescale.ProtocolContract, error) {
	return nil, r.err
}

func (erroringRosterReader) ListSourceContractsFromProjection(context.Context, string) ([]string, error) {
	return nil, nil
}

func (erroringRosterReader) ProtocolContractIndex(context.Context) (map[string]string, error) {
	return nil, nil
}

// TestBuildProtocolDetail_RosterReadErrorDegradesStatus is the
// CA2-A06-harden-3 regression guard: a roster read failure degrades to the
// same shape as a genuinely empty roster (protocolRoster swallowed it to
// []ProtocolContractView{}), so buildProtocolDetail must not stamp the page
// "ok" on it — a failed read is not a true empty, and an "ok" status
// displaces a previously healthy cached entry (protoDetailRefreshLocked).
func TestBuildProtocolDetail_RosterReadErrorDegradesStatus(t *testing.T) {
	meta, ok := protocolByName("cctp")
	if !ok {
		t.Fatal("cctp missing from registry")
	}
	srv := New(Options{
		ProtocolActivity:  prewarmActivityStub{},
		ProtocolBespoke:   &bespokeStub{},
		ProtocolContracts: erroringRosterReader{err: errors.New("roster read failed")},
	})
	v := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if v.Analytics == nil || v.Analytics.Status != protocolAnalyticsUnavailable {
		t.Fatalf("status = %+v, want %q: a failed roster read must not read as a healthy build",
			v.Analytics, protocolAnalyticsUnavailable)
	}
}

// TestBuildProtocolDetail_Events24hErrorDegradesStatus: a failed
// events_24h read must flip analytics.status to "unavailable" even when
// the lake analytics and bespoke block both build healthy — before the
// fix protocolEvents24h degraded to a nil map silently (every protocol
// serves 0) and never fed the failure into status.
func TestBuildProtocolDetail_Events24hErrorDegradesStatus(t *testing.T) {
	meta, ok := protocolByName("cctp")
	if !ok {
		t.Fatal("cctp missing from registry")
	}
	srv := New(Options{
		ProtocolActivity: prewarmActivityStub{},
		ProtocolBespoke:  &bespokeStub{},
		ProtocolStats:    erroringStatsReader{err: errors.New("stats read failed")},
	})
	v := srv.buildProtocolDetail(context.Background(), meta, protocolActivityWindowDays)
	if v.Analytics == nil || v.Analytics.Status != protocolAnalyticsUnavailable {
		t.Fatalf("status = %+v, want %q: a failed events_24h read must not read as a healthy build",
			v.Analytics, protocolAnalyticsUnavailable)
	}
}

// tipErrActivityStub fails every tip read.
type tipErrActivityStub struct{ prewarmActivityStub }

func (tipErrActivityStub) LakeTipLedger(context.Context) (uint32, error) {
	return 0, errors.New("lake unreachable")
}

// TestEnrichProtocolAnalytics_TipErrorDegradesHonestly: a failed lake-tip
// read must degrade the analytics (status "unavailable"), never serve a
// silently-mislabeled window (tip==0 used to collapse the fast path's
// cutoff to yesterday while still claiming the 90d window).
func TestEnrichProtocolAnalytics_TipErrorDegradesHonestly(t *testing.T) {
	srv := New(Options{ProtocolActivity: tipErrActivityStub{}})
	meta, ok := protocolByName("soroswap") // factories ⇒ non-empty analytics scope
	if !ok {
		t.Fatal("soroswap missing from registry")
	}
	view := ProtocolDetailView{ProtocolView: ProtocolView{Name: meta.Name}}
	if srv.enrichProtocolAnalytics(context.Background(), meta, &view) {
		t.Fatal("enrich reported healthy despite an unreadable tip")
	}
	if view.ActivityWindowDays != 0 || view.ActivitySeries != nil {
		t.Fatalf("degraded enrich still labeled a window: days=%d series=%v",
			view.ActivityWindowDays, view.ActivitySeries)
	}
}

// TestEnrichProtocolAnalytics_SharedPlanSingleTipRead: the three
// concurrent fills share ONE tip read and ONE fast-vs-raw decision — no
// fill may independently re-read the tip (discarding its error) or land
// on a different source than its siblings.
func TestEnrichProtocolAnalytics_SharedPlanSingleTipRead(t *testing.T) {
	stub := &scriptedFastStub{
		answers:     []probeAnswer{{true, true}},
		seriesPoint: []clickhouse.ProtocolDailyPoint{{Date: "2026-07-30", Events: 3}},
	}
	srv := New(Options{ProtocolActivity: stub})
	meta, ok := protocolByName("soroswap")
	if !ok {
		t.Fatal("soroswap missing from registry")
	}
	view := ProtocolDetailView{ProtocolView: ProtocolView{Name: meta.Name}}
	if !srv.enrichProtocolAnalytics(context.Background(), meta, &view) {
		t.Fatal("enrich degraded with a healthy stub")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tipCalls != 1 {
		t.Errorf("tip reads = %d, want exactly 1 (shared plan)", stub.tipCalls)
	}
	if stub.fastSeries != 1 || stub.fastBreak != 1 {
		t.Errorf("fast series/breakdown = %d/%d, want 1/1 (both fills on the fast source)", stub.fastSeries, stub.fastBreak)
	}
	// Per-contract activity must also serve from the fast pre-aggregation —
	// the raw `contract_events FINAL` path is the 2 GiB-limit kill that made
	// /v1/protocols/{name} return the certified-lake "unavailable" verdict.
	if stub.fastContract != 1 {
		t.Errorf("fast contract-activity = %d, want 1 (must use the fast source, not the raw FINAL scan)", stub.fastContract)
	}
	if stub.rawSeries != 0 || stub.rawBreak != 0 {
		t.Errorf("raw series/breakdown = %d/%d, want 0/0 when the fast path serves", stub.rawSeries, stub.rawBreak)
	}
}

// closeTimeActivityStub models a synthetic, deliberately NON-theoretical
// ledger close cadence (6s, i.e. 14,400/day — distinct from the old code's
// hardcoded 17,280/day) so protocolWindowFloor's close_time boundary
// produces a DIFFERENT, independently-computable answer than the old
// ledger-count arithmetic (CA2-A06-correct-1).
type closeTimeActivityStub struct {
	prewarmActivityStub
	tip uint32
}

var (
	protocolWindowGenesis = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	protocolWindowCadence = 6 * time.Second
)

func protocolWindowCloseTime(seq uint32) time.Time {
	return protocolWindowGenesis.Add(protocolWindowCadence * time.Duration(seq))
}

func (s closeTimeActivityStub) LakeTipLedger(context.Context) (uint32, error) {
	return s.tip, nil
}

func (s closeTimeActivityStub) LedgerBySeq(_ context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error) {
	if seq == 0 || seq > s.tip {
		return clickhouse.LedgerHeader{}, false, nil
	}
	return clickhouse.LedgerHeader{Seq: seq, CloseTime: protocolWindowCloseTime(seq)}, true, nil
}

// TestProtocolWindowFloor_UsesCloseTimeNotLedgerCount is the
// CA2-A06-correct-1 regression guard: the raw analytics readers' window
// cutoff must be derived from the tip's close_time minus
// protocolActivityWindowDays days, not a ledger-count multiple of the
// theoretical 17,280/day cadence — which on a real chain overshoots the
// requested number of days.
func TestProtocolWindowFloor_UsesCloseTimeNotLedgerCount(t *testing.T) {
	const tip = uint32(10_000_000)
	srv := New(Options{ProtocolActivity: closeTimeActivityStub{tip: tip}})

	since, sinceDay := srv.protocolWindowFloor(context.Background(), tip)

	// Independently derived from the model, not from protocolWindowFloor's
	// own arithmetic: 90 days at the stub's 6s cadence is exactly
	// 90*86400/6 = 1,296,000 ledgers, an exact boundary (no rounding).
	wantSince := tip - 1_296_000
	if since != wantSince {
		t.Fatalf("sinceLedger = %d, want %d (must derive from close_time, not the theoretical ledgers/day constant)",
			since, wantSince)
	}
	wantDay := protocolWindowCloseTime(tip).AddDate(0, 0, -protocolActivityWindowDays)
	if !sinceDay.Equal(wantDay) {
		t.Fatalf("sinceDay = %v, want %v (fast-path cutoff must share the same close_time boundary)", sinceDay, wantDay)
	}
}
