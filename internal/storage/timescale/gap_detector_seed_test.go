package timescale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestGapDetectorRestartHonoursPersistedCadence pins that lastScan is seeded from the persisted "gap-
// detector-scan" cursor: a restart 1h after a scan skips a 6h-cadence target, while an elapsed or never-
// scanned one is due.
func TestGapDetectorRestartHonoursPersistedCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 18, 23, 0, 0, time.UTC)
	heavy := GapDetectorTarget{Source: "seed-heavy", Table: "seed_heavy_events", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour}
	elapsed := GapDetectorTarget{Source: "seed-elapsed", Table: "seed_elapsed_events", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour}
	light := GapDetectorTarget{Source: "seed-light", Table: "seed_light_events", LedgerColumn: "ledger"}
	never := GapDetectorTarget{Source: "seed-never", Table: "seed_never_events", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour}
	targets := []GapDetectorTarget{heavy, elapsed, light, never}

	cursors := map[string]Cursor{
		targetKey(heavy):   {Source: gapDetectorHighWaterSource, Sub: targetKey(heavy), LastLedger: 62_500_000, UpdatedAt: now.Add(-1 * time.Hour)},
		targetKey(elapsed): {Source: gapDetectorHighWaterSource, Sub: targetKey(elapsed), LastLedger: 62_500_000, UpdatedAt: now.Add(-7 * time.Hour)},
		targetKey(light):   {Source: gapDetectorHighWaterSource, Sub: targetKey(light), LastLedger: 62_500_000, UpdatedAt: now.Add(-45 * time.Minute)},
	}
	getCursor := func(_ context.Context, source, sub string) (Cursor, error) {
		if source != gapDetectorHighWaterSource {
			t.Errorf("seed read cursor source %q; want %q", source, gapDetectorHighWaterSource)
		}
		c, ok := cursors[sub]
		if !ok {
			return Cursor{}, ErrNotFound
		}
		return c, nil
	}
	snapshots := []SourceCoverage{
		{Source: heavy.Source, Table: heavy.Table, DistinctLedgers: 4_200, MaxGapLedgers: 1_234, GapCount: 2, LastUpdated: now.Add(-1 * time.Hour)},
	}

	lastScan := seedGapDetectorState(context.Background(), targets, getCursor, snapshots, slog.Default(), now)

	if got, ok := lastScan[targetKey(heavy)]; !ok || !got.Equal(now.Add(-1*time.Hour)) {
		t.Errorf("heavy lastScan = %v (present=%v); want cursor last_updated %v", got, ok, now.Add(-1*time.Hour))
	}
	if _, ok := lastScan[targetKey(never)]; ok {
		t.Errorf("never-scanned target must have no lastScan entry")
	}

	due := dueGapDetectorTargets(targets, lastScan, now)
	got := make(map[string]bool, len(due))
	for _, d := range due {
		got[d.Source] = true
	}
	if got[heavy.Source] {
		t.Errorf("first cycle after restart scanned %s although only 1h of its 6h cadence elapsed — the 2026-08-28 restart storm", heavy.Source)
	}
	if !got[elapsed.Source] {
		t.Errorf("%s (7h since last scan, 6h cadence) must be due", elapsed.Source)
	}
	if !got[light.Source] {
		t.Errorf("%s (45m since last scan, 30m default cadence) must be due", light.Source)
	}
	if !got[never.Source] {
		t.Errorf("%s (no cursor) must be due on the first cycle", never.Source)
	}

	// Liveness + last-known gauges are re-emitted for the skipped target
	// so the _silent and gap_detected alerts keep working across the
	// restart.
	if v := testutil.ToFloat64(obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(heavy.Source, heavy.Table)); v != float64(now.Add(-1*time.Hour).Unix()) {
		t.Errorf("last_success_unix for skipped target = %v; want persisted %d", v, now.Add(-1*time.Hour).Unix())
	}
	if v := testutil.ToFloat64(obs.IngestGapMaxSize.WithLabelValues(heavy.Source, heavy.Table)); v != 1_234 {
		t.Errorf("gap_max_size for skipped target = %v; want snapshot 1234", v)
	}
	if v := testutil.ToFloat64(obs.IngestGapCount.WithLabelValues(heavy.Source, heavy.Table)); v != 2 {
		t.Errorf("gap_count for skipped target = %v; want snapshot 2", v)
	}
	if v := testutil.ToFloat64(obs.IngestSourceDistinctLedgers.WithLabelValues(heavy.Source, heavy.Table)); v != 4_200 {
		t.Errorf("distinct_ledgers for skipped target = %v; want snapshot 4200", v)
	}
	// Once the cadence elapses the seeded target becomes due.
	if later := dueGapDetectorTargets([]GapDetectorTarget{heavy}, lastScan, now.Add(5*time.Hour+1*time.Minute)); len(later) != 1 {
		t.Errorf("heavy target must be due once 6h have elapsed since the persisted scan; got %d due", len(later))
	}
}

// TestGapDetectorSeedFailsOpen: a cursor read error or a future-dated
// cursor must never SUPPRESS a scan beyond one cadence — the seed fails
// open to "scan now" (error) or clamps to now (skew).
func TestGapDetectorSeedFailsOpen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 18, 23, 0, 0, time.UTC)
	broken := GapDetectorTarget{Source: "seed-broken", Table: "seed_broken_events", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour}
	skewed := GapDetectorTarget{Source: "seed-skewed", Table: "seed_skewed_events", LedgerColumn: "ledger", ScanCadence: 6 * time.Hour}
	getCursor := func(_ context.Context, _, sub string) (Cursor, error) {
		switch sub {
		case targetKey(broken):
			return Cursor{}, errors.New("connection refused")
		case targetKey(skewed):
			return Cursor{UpdatedAt: now.Add(48 * time.Hour)}, nil
		}
		return Cursor{}, ErrNotFound
	}
	lastScan := seedGapDetectorState(context.Background(), []GapDetectorTarget{broken, skewed}, getCursor, nil, slog.Default(), now)
	if _, ok := lastScan[targetKey(broken)]; ok {
		t.Errorf("cursor read error must leave the target unseeded (scan on first cycle)")
	}
	if got := lastScan[targetKey(skewed)]; !got.Equal(now) {
		t.Errorf("future-dated cursor must clamp to now; got %v", got)
	}
	due := dueGapDetectorTargets([]GapDetectorTarget{broken, skewed}, lastScan, now.Add(6*time.Hour))
	if len(due) != 2 {
		t.Errorf("both targets must be due once one cadence has elapsed from boot; got %d", len(due))
	}
}

// gatherGapDetectorRunsTotal collects the runs_total counter through a private registry and returns
// value-by-label-set. It never calls WithLabelValues, which would create the series under test, so an absent
// series is reported absent.
func gatherGapDetectorRunsTotal(t *testing.T) map[[3]string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(obs.IngestGapDetectorRunsTotal)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := make(map[[3]string]float64)
	for _, mf := range families {
		if mf.GetName() != "stellarindex_ingest_gap_detector_runs_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var key [3]string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "source":
					key[0] = lp.GetValue()
				case "table":
					key[1] = lp.GetValue()
				case "outcome":
					key[2] = lp.GetValue()
				}
			}
			got[key] = m.GetCounter().GetValue()
		}
	}
	return got
}

// TestGapDetectorPreregistersRunsTotalSeries pins the stellarindex_ingest_gap_detector_silent
// false-fire: a detector that has run zero scans (the seeded schedule deferred them all) must
// still expose {outcome="ok"} and {outcome="error"} at exactly 0 for every configured target,
// or absent_over_time(runs_total) reads "no scan due yet" as "detector dead".
//
// preregFixtureSeq numbers the fixture targets so each invocation gets fresh label sets. The
// test requires the series to be ABSENT before preregisterGapDetectorSeries runs, so its
// presence afterwards is attributable to that call and not to the process-global vec; fixed
// names would trip that guard on `go test -count=2`. Numbering keeps the guard strict.
var preregFixtureSeq atomic.Uint64

func TestGapDetectorPreregistersRunsTotalSeries(t *testing.T) {
	t.Parallel()
	n := preregFixtureSeq.Add(1)
	targets := []GapDetectorTarget{
		{Source: fmt.Sprintf("prereg-a-%d", n), Table: fmt.Sprintf("prereg_a_events_%d", n), LedgerColumn: "ledger"},
		{Source: fmt.Sprintf("prereg-b-%d", n), Table: fmt.Sprintf("prereg_b_trades_%d", n), LedgerColumn: "ledger", ScanCadence: 6 * time.Hour},
	}

	before := gatherGapDetectorRunsTotal(t)
	for _, tg := range targets {
		for _, outcome := range []string{"ok", "error"} {
			if _, present := before[[3]string{tg.Source, tg.Table, outcome}]; present {
				t.Fatalf("test fixture %s/%s outcome=%s already present before pre-registration; pick unique names", tg.Source, tg.Table, outcome)
			}
		}
	}

	preregisterGapDetectorSeries(targets)

	after := gatherGapDetectorRunsTotal(t)
	for _, tg := range targets {
		for _, outcome := range []string{"ok", "error"} {
			key := [3]string{tg.Source, tg.Table, outcome}
			v, present := after[key]
			if !present {
				t.Errorf("runs_total{source=%q,table=%q,outcome=%q} absent after pre-registration; a restarted detector with no scan due reads as dead", tg.Source, tg.Table, outcome)
				continue
			}
			if v != 0 {
				t.Errorf("runs_total{source=%q,table=%q,outcome=%q} = %v; want exactly 0 (no scan has run)", tg.Source, tg.Table, outcome, v)
			}
		}
	}
}

// TestGapDetectorPreregisterCoversDefaultTargets guards the production
// wiring: every DefaultGapDetectorTargets entry gets both outcomes, so a
// newly added target cannot regress to emit-on-first-scan.
func TestGapDetectorPreregisterCoversDefaultTargets(t *testing.T) {
	t.Parallel()
	preregisterGapDetectorSeries(DefaultGapDetectorTargets)
	got := gatherGapDetectorRunsTotal(t)
	for _, tg := range DefaultGapDetectorTargets {
		for _, outcome := range []string{"ok", "error"} {
			if _, present := got[[3]string{tg.Source, tg.Table, outcome}]; !present {
				t.Errorf("default target %s/%s outcome=%s not pre-registered", tg.Source, tg.Table, outcome)
			}
		}
	}
}

// TestGapDetectorStatementTimeoutWithinGoBudget: the PG statement_timeout set on gap-detector scans must not
// exceed the Go per-target timeout, or Go abandons a query PG keeps running and orphan scans stack up each
// cycle.
func TestGapDetectorStatementTimeoutWithinGoBudget(t *testing.T) {
	t.Parallel()

	pgTimeout := time.Duration(gapDetectorStatementTimeoutMS) * time.Millisecond
	if pgTimeout <= 0 {
		t.Fatalf("gapDetectorStatementTimeoutMS = %d; must be positive (0 disables the PG-side backstop entirely)", gapDetectorStatementTimeoutMS)
	}
	if pgTimeout > gapDetectorPerTargetTimeout {
		t.Fatalf("gapDetectorStatementTimeoutMS = %v exceeds the Go per-target timeout %v — an over-budget scan outlives its context as an orphaned PG backend",
			pgTimeout, gapDetectorPerTargetTimeout)
	}
	// The ops/verify constant is the wrong tool here by construction:
	// it is sized for trusted multi-hour jobs whose caller context is
	// the real bound. Guard against someone "simplifying" back to it.
	if opsTimeout := time.Duration(opsVerifyStatementTimeoutMS) * time.Millisecond; opsTimeout <= gapDetectorPerTargetTimeout {
		t.Logf("note: opsVerifyStatementTimeoutMS (%v) is now within the detector budget; this guard is moot", opsTimeout)
	} else if gapDetectorStatementTimeoutMS == opsVerifyStatementTimeoutMS {
		t.Fatalf("gap detector must not share opsVerifyStatementTimeoutMS (%v) — that is the 2026-08-28 inversion", opsTimeout)
	}
}

// TestGapVerdictTrustworthy pins that a zero-gap result for a target whose density comes from a
// DistinctLedgerCountSQL override (soroban-events) is not trusted as clean without positive census evidence,
// since a TRUNCATE leaves no row to bracket a gap.
func TestGapVerdictTrustworthy(t *testing.T) {
	cases := []struct {
		name              string
		hasCensusOverride bool
		distinctErr       error
		distinct          int64
		want              bool
	}{
		{"no override: always trusted", false, nil, 0, true},
		{"no override: trusted even with distinct=0", false, nil, 0, true},
		{"census override, positive census: trusted", true, nil, 5, true},
		{"census override, zero census: NOT trusted (the finding)", true, nil, 0, false},
		{"census override, count-distinct errored: NOT trusted", true, errors.New("boom"), 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gapVerdictTrustworthy(tc.hasCensusOverride, tc.distinctErr, tc.distinct); got != tc.want {
				t.Errorf("gapVerdictTrustworthy(hasCensusOverride=%v, err=%v, distinct=%d) = %v, want %v",
					tc.hasCensusOverride, tc.distinctErr, tc.distinct, got, tc.want)
			}
		})
	}
}
