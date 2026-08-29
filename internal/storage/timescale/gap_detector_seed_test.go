package timescale

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestGapDetectorRestartHonoursPersistedCadence pins the 2026-08-28 r1
// restart amplifier: with the per-target lastScan map starting empty,
// every aggregator start re-ran the 6h-cadence soroban_events scan
// immediately. Seeding from the persisted "gap-detector-scan" cursor's
// last_updated means a restart 1h after the last scan SKIPS the target
// on the first cycle, while a target whose cadence has elapsed (or that
// has never scanned) is still due.
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
