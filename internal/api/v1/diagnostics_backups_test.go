package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackupMetrics answers queryVector from a canned expr → samples
// table; an expr missing from the table returns an empty vector
// (series absent), and exprs listed in fail return an error.
type fakeBackupMetrics struct {
	samples map[string][]promSample
	fail    map[string]bool
}

func (f *fakeBackupMetrics) queryVector(_ context.Context, expr string) ([]promSample, error) {
	if f.fail[expr] {
		return nil, errors.New("prometheus down")
	}
	return f.samples[expr], nil
}

func sample(labels map[string]any, v float64) promSample {
	return promSample{Labels: labels, Value: []any{float64(0), fmt.Sprintf("%g", v)}}
}

func d(dur time.Duration) *time.Duration { return &dur }

// TestFreshnessVerdict pins the SLO judgement: an age past the SLO is
// "stale", within it "ok", and no data is "unknown" — never a fresh
// zero (the web-status-1 class).
//
// The future-dated rows are the #311 regression: a stamp past
// backupClockSkewTolerance must read "unknown" carrying its RAW
// negative age, NOT "ok" with a clamped 0 — a forward-skewed host
// clock or a corrupt future-dated backup label would otherwise paint
// an arbitrarily stale backup green on the public status page.
// Ordinary sub-tolerance skew still floors to 0 and keeps its verdict:
// a stamp a few seconds ahead bounds the true age at a few seconds.
func TestFreshnessVerdict(t *testing.T) {
	cases := []struct {
		name   string
		age    *time.Duration
		slo    time.Duration
		status string
		ageSec *int64
	}{
		{"absent is unknown", nil, backupSLOFull, freshnessUnknown, nil},
		{"within SLO is ok", d(7 * 24 * time.Hour), backupSLOFull, freshnessOK, i64(7 * 24 * 3600)},
		{"exactly SLO is ok", d(backupSLODiff), backupSLODiff, freshnessOK, i64(36 * 3600)},
		{"past SLO is stale", d(backupSLODiff + time.Second), backupSLODiff, freshnessStale, i64(36*3600 + 1)},
		{"9d full is stale", d(9 * 24 * time.Hour), backupSLOFull, freshnessStale, i64(9 * 24 * 3600)},
		{"16m WAL is stale", d(16 * time.Minute), backupSLOWAL, freshnessStale, i64(16 * 60)},
		{"skew inside tolerance floors to 0 and stays ok", d(-30 * time.Second), backupSLOWAL, freshnessOK, i64(0)},
		// The two rows either side of backupClockSkewTolerance are
		// spelled as literals on purpose: they pin the tolerance's VALUE
		// (1 min), not merely its relationship to itself.
		{"exactly the skew tolerance is still ok", d(-60 * time.Second), backupSLOWAL, freshnessOK, i64(0)},
		{"one second past the tolerance is unknown, raw age", d(-61 * time.Second), backupSLOWAL, freshnessUnknown, i64(-61)},
		{"an hour in the future is unknown, raw age", d(-time.Hour), backupSLOWAL, freshnessUnknown, i64(-3600)},
		{"9d in the future never reads fresh", d(-9 * 24 * time.Hour), backupSLOOffsite, freshnessUnknown, i64(-9 * 24 * 3600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := freshnessVerdict(tc.age, tc.slo)
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q", got.Status, tc.status)
			}
			if got.SLOSeconds != int64(tc.slo/time.Second) {
				t.Errorf("slo_seconds = %d, want %d", got.SLOSeconds, int64(tc.slo/time.Second))
			}
			switch {
			case tc.ageSec == nil && got.AgeSeconds != nil:
				t.Errorf("age_seconds = %d, want nil", *got.AgeSeconds)
			case tc.ageSec != nil && (got.AgeSeconds == nil || *got.AgeSeconds != *tc.ageSec):
				t.Errorf("age_seconds = %v, want %d", got.AgeSeconds, *tc.ageSec)
			}
		})
	}
}

// TestOverallFreshness: stale outranks unknown outranks ok — a panel
// that can't see a source is degraded, a breach is red.
func TestOverallFreshness(t *testing.T) {
	ok := FreshnessVerdict{Status: freshnessOK}
	unk := FreshnessVerdict{Status: freshnessUnknown}
	stale := FreshnessVerdict{Status: freshnessStale}
	if got := overallFreshness(ok, ok); got != freshnessOK {
		t.Errorf("all ok → %q, want ok", got)
	}
	if got := overallFreshness(ok, unk, ok); got != freshnessUnknown {
		t.Errorf("one unknown → %q, want unknown", got)
	}
	if got := overallFreshness(unk, stale, ok); got != freshnessStale {
		t.Errorf("stale+unknown → %q, want stale", got)
	}
	if got := overallFreshness(); got != freshnessOK {
		t.Errorf("no items → %q, want ok", got)
	}
}

func TestParseBackupLabelTime(t *testing.T) {
	full, ok := parseBackupLabelTime("20260823-020001F")
	if !ok || !full.Equal(time.Date(2026, 8, 23, 2, 0, 1, 0, time.UTC)) {
		t.Errorf("full label → %v %v", full, ok)
	}
	diff, ok := parseBackupLabelTime("20260823-020001F_20260827-020003D")
	if !ok || !diff.Equal(time.Date(2026, 8, 27, 2, 0, 3, 0, time.UTC)) {
		t.Errorf("diff label → %v %v (want the LAST segment)", diff, ok)
	}
	if _, ok := parseBackupLabelTime("garbage"); ok {
		t.Error("garbage parsed")
	}
	if _, ok := parseBackupLabelTime(""); ok {
		t.Error("empty parsed")
	}
}

// TestBuildBackupsSnapshot_Projection feeds a fully-populated exporter
// shape and pins every wire field the status panel reads: timestamps
// derived from the exporter's since-last ages, the newest full's size
// + repo, per-repo newest backup from the label, drill result, and the
// per-item verdicts.
func TestBuildBackupsSnapshot_Projection(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	stanza := "stellarindex"
	src := &fakeBackupMetrics{samples: map[string][]promSample{
		promBackupSinceLast: {
			sample(map[string]any{"stanza": stanza, "backup_type": "full"}, 5*24*3600),
			sample(map[string]any{"stanza": stanza, "backup_type": "diff"}, 10*3600),
			sample(map[string]any{"stanza": stanza, "backup_type": "incr"}, 10*3600),
			// The aggregate pseudo-stanza must be filtered by the PromQL,
			// but even if it leaked it must not win: give it a fresher age.
			sample(map[string]any{"stanza": "all-stanzas", "backup_type": "full"}, 1),
		},
		promBackupInfo: {
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "full", "backup_name": "20260817-020001F"}, 1),
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "full", "backup_name": "20260824-020001F"}, 1),
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "diff", "backup_name": "20260824-020001F_20260829-020003D"}, 1),
			sample(map[string]any{"stanza": stanza, "repo_key": "2", "backup_type": "full", "backup_name": "20260817-020001F"}, 1),
		},
		promBackupFullSize: {
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "full", "backup_name": "20260817-020001F"}, 100),
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "full", "backup_name": "20260824-020001F"}, 123456789),
		},
		promWALArchiveAge: {sample(nil, 120)},
		promDrillSuccess:  {sample(nil, float64(now.Add(-20*24*time.Hour).Unix()))},
		promDrillFailures: {sample(nil, 0)},
		promDrillMtime:    {sample(map[string]any{"file": "/var/lib/node_exporter/textfile_collector/restore_drill.prom"}, float64(now.Add(-20*24*time.Hour).Unix()))},
		promSnapshotLast:  {sample(nil, float64(now.Add(-6*time.Hour).Unix()))},
	}}
	// The all-stanzas row is fresher than the real one; the projection
	// takes the MIN age per type, so drop it from the fake the way the
	// PromQL matcher would — this test is about projection, not the
	// matcher (the matcher text is pinned by TestBackupsPromQL below).
	src.samples[promBackupSinceLast] = src.samples[promBackupSinceLast][:3]

	got := buildBackupsSnapshot(context.Background(), src, now)

	if got.SourceStatus != "ok" {
		t.Errorf("source_status = %q, want ok", got.SourceStatus)
	}
	wantFull := now.Add(-5 * 24 * time.Hour)
	if got.Postgres.LastFull == nil || !got.Postgres.LastFull.TS.Equal(wantFull) {
		t.Fatalf("last_full = %+v, want ts %v", got.Postgres.LastFull, wantFull)
	}
	if got.Postgres.LastFull.SizeBytes == nil || *got.Postgres.LastFull.SizeBytes != 123456789 {
		t.Errorf("last_full.size_bytes = %v, want 123456789 (the NEWEST full's size, not the older one)", got.Postgres.LastFull.SizeBytes)
	}
	if got.Postgres.LastFull.Repo != "1" {
		t.Errorf("last_full.repo = %q, want 1", got.Postgres.LastFull.Repo)
	}
	if got.Postgres.LastDiff == nil || !got.Postgres.LastDiff.TS.Equal(now.Add(-10*time.Hour)) {
		t.Errorf("last_diff = %+v", got.Postgres.LastDiff)
	}
	if got.Postgres.WALArchiveMaxAgeSeconds == nil || *got.Postgres.WALArchiveMaxAgeSeconds != 120 {
		t.Errorf("wal age = %v, want 120", got.Postgres.WALArchiveMaxAgeSeconds)
	}

	if len(got.Postgres.Repos) != 2 {
		t.Fatalf("repos = %+v, want 2 rows", got.Postgres.Repos)
	}
	r1, r2 := got.Postgres.Repos[0], got.Postgres.Repos[1]
	if r1.Repo != "1" || r1.Kind != "local" || r1.LastBackupTS == nil ||
		!r1.LastBackupTS.Equal(time.Date(2026, 8, 29, 2, 0, 3, 0, time.UTC)) {
		t.Errorf("repo1 = %+v, want local / newest = the diff's own start 2026-08-29T02:00:03Z", r1)
	}
	if r2.Repo != "2" || r2.Kind != "offsite" || r2.LastBackupTS == nil ||
		!r2.LastBackupTS.Equal(time.Date(2026, 8, 17, 2, 0, 1, 0, time.UTC)) {
		t.Errorf("repo2 = %+v, want offsite / 2026-08-17T02:00:01Z", r2)
	}
	if r1.Retention != nil || r2.Retention != nil {
		t.Errorf("retention must be nil (not exported), got %v / %v", r1.Retention, r2.Retention)
	}

	if got.RestoreDrill.Result != "pass" || got.RestoreDrill.FailedChecks == nil || *got.RestoreDrill.FailedChecks != 0 {
		t.Errorf("drill = %+v, want pass / 0 failed checks", got.RestoreDrill)
	}
	if got.RestoreDrill.LastRunTS == nil || got.RestoreDrill.LastSuccessTS == nil ||
		!got.RestoreDrill.LastRunTS.Equal(now.Add(-20*24*time.Hour)) {
		t.Errorf("drill timestamps = %+v", got.RestoreDrill)
	}
	if got.RestoreDrill.RestoredBackupTS != nil || got.RestoreDrill.DurationSeconds != nil {
		t.Errorf("reserved drill fields must be nil: %+v", got.RestoreDrill)
	}
	if got.ClickHouse.SchemaSnapshotLastTS == nil || !got.ClickHouse.SchemaSnapshotLastTS.Equal(now.Add(-6*time.Hour)) {
		t.Errorf("snapshot ts = %v", got.ClickHouse.SchemaSnapshotLastTS)
	}
	if got.ClickHouse.SchemaSnapshotOffsiteLastTS != nil || got.ClickHouse.ZFSSnapshotLatestTS != nil || got.ClickHouse.ReplicaLagSeconds != nil {
		t.Errorf("absent/reserved CH fields must be nil: %+v", got.ClickHouse)
	}

	// Verdicts: full 5d ≤ 8d ok; diff 10h ≤ 36h ok; WAL 120s ≤ 15m ok;
	// offsite repo2 newest is 12d 10h old > 8d → STALE; drill 20d ≤
	// 35d ok; snapshot 6h ≤ 36h ok. Roll-up is stale.
	f := got.Freshness
	for name, v := range map[string]FreshnessVerdict{"full": f.Full, "diff": f.Diff, "wal": f.WAL, "drill": f.Drill, "snapshot": f.Snapshot} {
		if v.Status != freshnessOK {
			t.Errorf("%s verdict = %q, want ok (%+v)", name, v.Status, v)
		}
	}
	if f.Offsite.Status != freshnessStale {
		t.Errorf("offsite verdict = %q, want stale (12d > 8d)", f.Offsite.Status)
	}
	if f.Offsite.AgeSeconds == nil || *f.Offsite.AgeSeconds != int64(now.Sub(*r2.LastBackupTS)/time.Second) {
		t.Errorf("offsite age = %v", f.Offsite.AgeSeconds)
	}
	if f.Overall != freshnessStale {
		t.Errorf("overall = %q, want stale", f.Overall)
	}
	if got.SLO.FullSeconds != 8*24*3600 || got.SLO.DiffSeconds != 36*3600 || got.SLO.WALSeconds != 15*60 ||
		got.SLO.OffsiteSeconds != 8*24*3600 || got.SLO.DrillSeconds != 35*24*3600 || got.SLO.SnapshotSeconds != 36*3600 {
		t.Errorf("slo = %+v", got.SLO)
	}
}

// TestBuildBackupsSnapshot_AbsentAndFailed: absent series → nil +
// "unknown" (never a zero), a failed query degrades source_status,
// and a failed drill reads "fail" with its last_success retained.
func TestBuildBackupsSnapshot_AbsentAndFailed(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	src := &fakeBackupMetrics{
		samples: map[string][]promSample{
			promDrillSuccess:  {sample(nil, float64(now.Add(-40*24*time.Hour).Unix()))},
			promDrillFailures: {sample(nil, 2)},
			promSnapshotLast:  {sample(nil, 0)}, // epoch-0 placeholder = never
		},
		fail: map[string]bool{promWALArchiveAge: true},
	}
	got := buildBackupsSnapshot(context.Background(), src, now)

	if got.SourceStatus != "degraded" {
		t.Errorf("source_status = %q, want degraded (one query failed)", got.SourceStatus)
	}
	if got.Postgres.LastFull != nil || got.Postgres.LastDiff != nil || got.Postgres.WALArchiveMaxAgeSeconds != nil {
		t.Errorf("absent postgres series must be nil: %+v", got.Postgres)
	}
	if len(got.Postgres.Repos) != 0 {
		t.Errorf("repos = %+v, want empty", got.Postgres.Repos)
	}
	if got.RestoreDrill.Result != "fail" || got.RestoreDrill.FailedChecks == nil || *got.RestoreDrill.FailedChecks != 2 {
		t.Errorf("drill = %+v, want fail / 2", got.RestoreDrill)
	}
	if got.ClickHouse.SchemaSnapshotLastTS != nil {
		t.Errorf("epoch-0 snapshot stamp must read as never (nil), got %v", got.ClickHouse.SchemaSnapshotLastTS)
	}
	f := got.Freshness
	for name, v := range map[string]FreshnessVerdict{"full": f.Full, "diff": f.Diff, "wal": f.WAL, "offsite": f.Offsite, "snapshot": f.Snapshot} {
		if v.Status != freshnessUnknown || v.AgeSeconds != nil {
			t.Errorf("%s verdict = %+v, want unknown with nil age", name, v)
		}
	}
	if f.Drill.Status != freshnessStale {
		t.Errorf("drill verdict = %q, want stale (40d > 35d)", f.Drill.Status)
	}
	if f.Overall != freshnessStale {
		t.Errorf("overall = %q, want stale", f.Overall)
	}

	// Everything failing → "unknown" document.
	all := &fakeBackupMetrics{fail: map[string]bool{}}
	for _, e := range []string{promBackupSinceLast, promBackupInfo, promBackupFullSize, promWALArchiveAge, promDrillSuccess, promDrillFailures, promDrillMtime, promSnapshotLast, promSnapshotOffsite} {
		all.fail[e] = true
	}
	if got := buildBackupsSnapshot(context.Background(), all, now); got.SourceStatus != "unknown" || got.Freshness.Overall != freshnessUnknown {
		t.Errorf("all-failed → source_status %q / overall %q, want unknown/unknown", got.SourceStatus, got.Freshness.Overall)
	}
}

// TestBuildBackupsSnapshot_FutureDatedOffsite is the #311 regression at
// document level: every source is fresh EXCEPT the off-site repo,
// whose pgBackRest label is stamped 8d 14h in the future (forward host
// clock, or a corrupt label). Before the fix that row rendered
// "ok · 0s ago" and the whole panel went green — an arbitrarily stale
// off-site copy behind an all-clear. It must now read "unknown"
// carrying the raw negative age, and drag the roll-up off "ok" (which
// is also what sets flags.stale on the wire).
func TestBuildBackupsSnapshot_FutureDatedOffsite(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	stanza := "stellarindex"
	src := &fakeBackupMetrics{samples: map[string][]promSample{
		promBackupSinceLast: {
			sample(map[string]any{"stanza": stanza, "backup_type": "full"}, 5*24*3600),
			sample(map[string]any{"stanza": stanza, "backup_type": "diff"}, 10*3600),
		},
		promBackupInfo: {
			sample(map[string]any{"stanza": stanza, "repo_key": "1", "backup_type": "full", "backup_name": "20260824-020001F"}, 1),
			// repo2's newest label is 2026-09-07T02:00:01Z — 8d 14h 0m 1s
			// AHEAD of `now`.
			sample(map[string]any{"stanza": stanza, "repo_key": "2", "backup_type": "full", "backup_name": "20260907-020001F"}, 1),
		},
		promWALArchiveAge: {sample(nil, 120)},
		promDrillSuccess:  {sample(nil, float64(now.Add(-20*24*time.Hour).Unix()))},
		promDrillFailures: {sample(nil, 0)},
		promSnapshotLast:  {sample(nil, float64(now.Add(-6*time.Hour).Unix()))},
	}}

	got := buildBackupsSnapshot(context.Background(), src, now)

	f := got.Freshness
	if f.Offsite.Status != freshnessUnknown {
		t.Errorf("offsite verdict = %q, want unknown — a future-dated label bounds nothing about the real backup age and must never read fresh", f.Offsite.Status)
	}
	const wantAge = -(8*24*3600 + 14*3600 + 1) // -741601 s
	if f.Offsite.AgeSeconds == nil || *f.Offsite.AgeSeconds != wantAge {
		t.Errorf("offsite age_seconds = %v, want %d (the RAW negative age, so an operator can see the skew — not a clamped 0)", f.Offsite.AgeSeconds, wantAge)
	}
	if f.Overall != freshnessUnknown {
		t.Errorf("overall = %q, want unknown — the panel (and flags.stale) must not read all-clear while a backup stamp is from the future", f.Overall)
	}
	// The other rows are genuinely fresh and must stay green: the guard
	// judges the skewed item, not the document.
	for name, v := range map[string]FreshnessVerdict{"full": f.Full, "diff": f.Diff, "wal": f.WAL, "drill": f.Drill, "snapshot": f.Snapshot} {
		if v.Status != freshnessOK {
			t.Errorf("%s verdict = %q, want ok (%+v)", name, v.Status, v)
		}
	}
}

// TestBackupsPromQL pins the matcher text: the exporter's aggregate
// pseudo-stanza must be excluded (as storage.yml's alerts do), the
// full-size query must select fulls only, and the drill mtime must
// match the textfile by basename regardless of directory.
func TestBackupsPromQL(t *testing.T) {
	for _, q := range []string{promBackupSinceLast, promBackupInfo, promBackupFullSize} {
		if !strings.Contains(q, `stanza!~"all-stanzas.*"`) {
			t.Errorf("%q lacks the all-stanzas exclusion", q)
		}
	}
	if !strings.Contains(promBackupFullSize, `backup_type="full"`) {
		t.Errorf("full-size query must select fulls only: %q", promBackupFullSize)
	}
	if !strings.Contains(promDrillMtime, `restore_drill\\.prom$`) {
		t.Errorf("drill mtime must match by basename: %q", promDrillMtime)
	}
}

// TestHandleDiagnosticsBackups_EndToEnd round-trips the handler
// through a canned Prometheus HTTP fake, pinning the wire JSON field
// names the explorer panel consumes, the 60 s cache header, the
// flags.stale mirror, and that the second request is served from the
// in-process cache (one Prometheus round of queries, not two).
func TestHandleDiagnosticsBackups_EndToEnd(t *testing.T) {
	var hits atomic.Int64
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		q := r.URL.Query().Get("query")
		var result string
		switch {
		case strings.HasPrefix(q, "pgbackrest_backup_since_last_completion_seconds"):
			result = `[{"metric":{"stanza":"stellarindex","backup_type":"full"},"value":[0,"777600"]},
			           {"metric":{"stanza":"stellarindex","backup_type":"diff"},"value":[0,"3600"]}]`
		case strings.HasPrefix(q, "stellarindex_ch_schema_snapshot_last_success_unix"):
			result = fmt.Sprintf(`[{"metric":{},"value":[0,"%d"]}]`, time.Now().Add(-time.Hour).Unix())
		default:
			result = `[]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":` + result + `}}`))
	}))
	defer prom.Close()

	srv := New(Options{StatusBackend: &PrometheusStatusBackend{URL: prom.URL}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/diagnostics/backups")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=60, s-maxage=60" {
		t.Errorf("Cache-Control = %q, want the 60 s public policy (overriding the /v1/diagnostics/ no-cache prefix rule)", cc)
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Flags Flags           `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(env.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["source_status"] != "ok" {
		t.Errorf("source_status = %v", doc["source_status"])
	}
	fresh, _ := doc["freshness"].(map[string]any)
	full, _ := fresh["full"].(map[string]any)
	if full["status"] != "stale" || full["slo_seconds"] != float64(8*24*3600) {
		t.Errorf("freshness.full = %v, want stale (9d > 8d SLO) with slo_seconds 691200", full)
	}
	if diff, _ := fresh["diff"].(map[string]any); diff["status"] != "ok" {
		t.Errorf("freshness.diff = %v, want ok", diff)
	}
	if wal, _ := fresh["wal"].(map[string]any); wal["status"] != "unknown" || wal["age_seconds"] != nil {
		t.Errorf("freshness.wal = %v, want unknown with null age (series absent)", wal)
	}
	if fresh["overall"] != "stale" {
		t.Errorf("freshness.overall = %v, want stale", fresh["overall"])
	}
	if !env.Flags.Stale {
		t.Error("flags.stale must mirror a non-ok roll-up")
	}
	pg, _ := doc["postgres"].(map[string]any)
	if _, has := pg["wal_archive_max_age_seconds"]; !has || pg["wal_archive_max_age_seconds"] != nil {
		t.Errorf("postgres.wal_archive_max_age_seconds must be present and null when absent: %v", pg)
	}
	if repos, ok := pg["repos"].([]any); !ok || len(repos) != 0 {
		t.Errorf("postgres.repos must be an empty array (never null) when the exporter reports none: %v", pg["repos"])
	}
	drill, _ := doc["restore_drill"].(map[string]any)
	if drill["result"] != "unknown" {
		t.Errorf("restore_drill.result = %v, want unknown", drill["result"])
	}

	// Second request within the TTL is served from cache.
	before := hits.Load()
	resp2, err := http.Get(ts.URL + "/v1/diagnostics/backups")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if got := hits.Load(); got != before {
		t.Errorf("second request re-queried Prometheus (%d → %d hits); want the 60 s in-process cache to serve it", before, got)
	}
}

// TestHandleDiagnosticsBackups_Unconfigured: no Prometheus → 503
// problem, same ladder as /v1/diagnostics/archive.
func TestHandleDiagnosticsBackups_Unconfigured(t *testing.T) {
	srv := New(Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/diagnostics/backups")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on problem responses", cc)
	}
}
