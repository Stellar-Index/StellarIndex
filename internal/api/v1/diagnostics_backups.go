package v1

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackupsDiagnostics is the wire shape of GET /v1/diagnostics/backups —
// the public status page's "Backups" panel. One snapshot of backup +
// DR-evidence freshness for the region, judged against the SLO
// thresholds echoed in `slo` (same convention as StatusLatency's
// P95TargetMs: the API declares the thresholds it judges by, the UI
// never carries its own literals).
//
// SOURCE OF TRUTH: Prometheus, via the same [PrometheusStatusBackend]
// that backs /v1/status. The API never shells out to pgbackrest; it
// reads what the host already exports —
//   - woblerr/pgbackrest_exporter (job `pgbackrest_exporter`, port 9854;
//     16-prometheus-exporters.yml): `pgbackrest_backup_*` series with
//     `stanza`, `backup_type`, `backup_name`, `repo_key` labels;
//   - node_exporter textfile collector: `stellarindex_restore_drill_*`
//     (scripts/ops/restore-drill.sh) and
//     `stellarindex_ch_schema_snapshot_*` (scripts/ops/ch-schema-snapshot.sh),
//     plus node_exporter's own `node_textfile_mtime_seconds` for the
//     drill's last-run time (the script stamps last_success ONLY on a
//     clean run, so mtime is the only "it ran at all" signal).
//
// Every timestamp / age is a pointer: nil means "no data" (series
// absent, or the query failed) and the matching freshness verdict is
// "unknown". The panel renders that grey, never as a fresh zero
// (web-status-1 class, PR #273).
//
// No secrets, no filesystem paths, no hostnames: repo keys are the
// pgBackRest `repoN` ordinals, backup labels are pgBackRest's own
// `YYYYMMDD-HHMMSS[F|D|I]` names.
type BackupsDiagnostics struct {
	// SourceStatus is the tri-state trust signal for the whole
	// document (same rationale as StatusResponse.IncidentsStatus):
	//   - "ok":       every Prometheus query succeeded;
	//   - "degraded": some queries failed — the items they feed are
	//                 nil / "unknown", the rest are trustworthy;
	//   - "unknown":  every query failed (Prometheus unreachable) —
	//                 nothing below is trustworthy.
	SourceStatus string `json:"source_status"`

	Postgres     BackupPostgres     `json:"postgres"`
	RestoreDrill BackupRestoreDrill `json:"restore_drill"`
	ClickHouse   BackupClickHouse   `json:"clickhouse"`

	// Freshness is the per-item verdict vs. SLO — the ONLY thing the
	// status page needs to colour a row.
	Freshness BackupFreshness `json:"freshness"`

	// SLO echoes the thresholds the verdicts above were judged
	// against, in seconds.
	SLO BackupSLO `json:"slo"`
}

// BackupPostgres is the pgBackRest section.
type BackupPostgres struct {
	// LastFull / LastDiff: the most recent completed backup of that
	// type (any repo), from the exporter's server-side
	// `pgbackrest_backup_since_last_completion_seconds`. Nil when the
	// series is absent. Note the exporter's `diff` series counts a
	// full as satisfying a diff (a full IS a superset), matching
	// pgBackRest's own retention semantics.
	LastFull *BackupRun `json:"last_full"`
	LastDiff *BackupRun `json:"last_diff"`

	// WALArchiveMaxAgeSeconds is the age of the newest archived WAL
	// segment (`pg_stat_archiver_last_archive_age`, postgres_exporter).
	// Nil when the collector isn't enabled on the host — rendered
	// "no data", never 0.
	WALArchiveMaxAgeSeconds *float64 `json:"wal_archive_max_age_seconds"`

	// Repos lists every pgBackRest repository the exporter reports,
	// sorted by key. repo1 is the on-host copy, repo2 the encrypted
	// S3 offsite copy (configs/ansible/.../pgbackrest.conf.j2 — the
	// key→kind mapping is that template's convention, not a metric).
	Repos []BackupRepo `json:"repos"`
}

// BackupRun is one completed pgBackRest backup.
type BackupRun struct {
	// TS is the backup's completion time (UTC).
	TS time.Time `json:"ts"`
	// SizeBytes is the database size the backup captured
	// (`pgbackrest_backup_size_bytes`). Nil when the exporter has no
	// per-backup row for it (e.g. the full aged out of `info` but the
	// since-last-completion gauge still reports it).
	SizeBytes *int64 `json:"size_bytes"`
	// Repo is the repository key the backup landed in ("1", "2");
	// empty when unknown.
	Repo string `json:"repo,omitempty"`
}

// BackupRepo is one pgBackRest repository's freshness row.
type BackupRepo struct {
	// Repo is the pgBackRest repo key ("1", "2").
	Repo string `json:"repo"`
	// Kind is "local" (repo1), "offsite" (repo2) or "unknown".
	Kind string `json:"kind"`
	// LastBackupTS is the start time of the newest backup of any
	// type in this repo, parsed from pgBackRest's backup label
	// (`YYYYMMDD-HHMMSS`, host-local clock — UTC on every host this
	// role provisions). Nil when the repo has no backups.
	LastBackupTS *time.Time `json:"last_backup_ts"`
	// Retention is the repo's retention policy when known. The
	// exporter does not publish pgBackRest's retention settings, so
	// this is nil today; reserved so the panel's column exists.
	Retention *string `json:"retention"`
}

// BackupRestoreDrill is the monthly pgBackRest restore-drill evidence
// (ADR-0043 §3 / CS-110), from restore-drill.sh's textfile.
type BackupRestoreDrill struct {
	// LastRunTS is when the drill last wrote its textfile at all —
	// node_exporter's mtime of restore_drill.prom. Nil = never ran on
	// this host (or the textfile collector can't see it).
	LastRunTS *time.Time `json:"last_run_ts"`
	// LastSuccessTS is the last FULLY-clean run
	// (`stellarindex_restore_drill_last_success_unix`, stamped only
	// when every verification check passed and evidence was written).
	LastSuccessTS *time.Time `json:"last_success_ts"`
	// Result classifies the most recent run: "pass" (zero failed
	// checks), "fail" (one or more), "unknown" (no run recorded).
	Result string `json:"result"`
	// FailedChecks is `stellarindex_restore_drill_failures` for the
	// most recent run; nil when unknown.
	FailedChecks *int64 `json:"failed_checks"`
	// RestoredBackupTS / DurationSeconds are reserved: the drill does
	// not export which backup it restored or how long it took yet
	// (follow-up to scripts/ops/restore-drill.sh once PR #271 lands).
	RestoredBackupTS *time.Time `json:"restored_backup_ts"`
	DurationSeconds  *float64   `json:"duration_s"`
}

// BackupClickHouse is the lake-protection section (ADR-0043 §2.1: the
// lake has no DATA backup by design — the schema+state snapshot is
// its only copy).
type BackupClickHouse struct {
	// SchemaSnapshotLastTS is the last clean schema+state capture
	// (`stellarindex_ch_schema_snapshot_last_success_unix`).
	SchemaSnapshotLastTS *time.Time `json:"schema_snapshot_last_ts"`
	// SchemaSnapshotOffsiteLastTS is the last successful offsite push
	// of that snapshot; nil on a host that acknowledged local-only.
	SchemaSnapshotOffsiteLastTS *time.Time `json:"schema_snapshot_offsite_last_ts"`
	// ZFSSnapshotLatestTS is reserved: no ZFS snapshot schedule
	// exists for the lake pool today (ADR-0043 §2 rejects a data
	// copy), so there is no producer and this is always nil.
	ZFSSnapshotLatestTS *time.Time `json:"zfs_snapshot_latest_ts"`
	// ReplicaLagSeconds is reserved: the lake is single-node today,
	// so there is no replica and this is always nil.
	ReplicaLagSeconds *float64 `json:"replica_lag_s"`
}

// BackupFreshness holds one verdict per monitored item plus the
// roll-up. Field names are the panel's row keys.
type BackupFreshness struct {
	Full     FreshnessVerdict `json:"full"`
	Diff     FreshnessVerdict `json:"diff"`
	WAL      FreshnessVerdict `json:"wal"`
	Offsite  FreshnessVerdict `json:"offsite"`
	Drill    FreshnessVerdict `json:"drill"`
	Snapshot FreshnessVerdict `json:"snapshot"`

	// Overall is the worst verdict across the items above, with
	// "unknown" ranking between "ok" and "stale": a panel that can't
	// see its sources is degraded, not green — but a definite SLO
	// breach outranks a blind spot.
	Overall string `json:"overall"`
}

// FreshnessVerdict judges one item's age against its SLO.
type FreshnessVerdict struct {
	// Status is "ok" (age ≤ SLO), "stale" (age > SLO) or "unknown"
	// (no data, or a stamp from the future — see freshnessVerdict).
	Status string `json:"status"`
	// AgeSeconds is the item's age at snapshot time; nil when there is
	// no data. Negative on a future-dated stamp: the raw skew is
	// reported (with Status "unknown"), never clamped into a fresh 0.
	AgeSeconds *int64 `json:"age_seconds"`
	// SLOSeconds is the threshold the status was judged against.
	SLOSeconds int64 `json:"slo_seconds"`
}

// BackupSLO echoes the freshness thresholds, in seconds.
type BackupSLO struct {
	FullSeconds     int64 `json:"full_seconds"`
	DiffSeconds     int64 `json:"diff_seconds"`
	WALSeconds      int64 `json:"wal_seconds"`
	OffsiteSeconds  int64 `json:"offsite_seconds"`
	DrillSeconds    int64 `json:"drill_seconds"`
	SnapshotSeconds int64 `json:"snapshot_seconds"`
}

// Freshness verdict states for [FreshnessVerdict.Status] and
// [BackupFreshness.Overall].
const (
	freshnessOK      = "ok"
	freshnessStale   = "stale"
	freshnessUnknown = "unknown"
)

// Backup freshness SLOs (Ash, 2026-08-29). Weekly full (Sunday) +
// daily diff + archive-async WAL + monthly drill + daily snapshot,
// each with one missed cycle of slack. The alert thresholds in
// storage.yml / restore-drill.yml are deliberately looser (they page
// people); these colour a public panel and can afford to be tight.
const (
	backupSLOFull     = 8 * 24 * time.Hour
	backupSLODiff     = 36 * time.Hour
	backupSLOWAL      = 15 * time.Minute
	backupSLOOffsite  = 8 * 24 * time.Hour
	backupSLODrill    = 35 * 24 * time.Hour
	backupSLOSnapshot = 36 * time.Hour
)

// backupClockSkewTolerance bounds how far a stamp may sit in the
// FUTURE of the API's own clock and still be judged on its face.
//
// The ages here cross clocks: `now` is the API host's wall clock while
// the stamps come from the backup host's exporters / textfiles, so a
// sub-minute negative age is ordinary NTP divergence on an item that
// IS fresh (a stamp at most a minute ahead bounds the true age at a
// minute) — it floors to 0 and keeps its verdict. A minute is two
// orders of magnitude past anything a disciplined host shows and still
// 15× inside the tightest SLO (WAL, 15 min), so it cannot mask a
// breach.
//
// Beyond the tolerance the reading is self-inconsistent and bounds
// NOTHING: a forward-skewed host clock or a corrupt future-dated
// backup label says nothing about when the backup actually ran, so the
// verdict is "unknown" (grey, and it degrades the roll-up) rather than
// a green zero. Same shape as canonical.SafeUnixFutureWindow,
// tightened from 24 h because these are our own NTP-disciplined hosts,
// not a third-party relayer.
const backupClockSkewTolerance = time.Minute

// backupsCacheTTL bounds how often the handler re-queries Prometheus.
// The freshest signal here moves on a 5-min WAL cadence; 60 s keeps
// a status page polling every 30 s from doubling the query load.
const backupsCacheTTL = 60 * time.Second

// backupMetricsSource is the seam the backups diagnostics reads
// Prometheus through. *PrometheusStatusBackend satisfies it; New()
// derives it from Options.StatusBackend so production needs no extra
// wiring, and Options.BackupMetrics overrides it for tests.
type backupMetricsSource interface {
	queryVector(ctx context.Context, expr string) ([]promSample, error)
}

// BackupMetricsSource is the exported alias of the seam, so a test in
// another package (or a future non-Prometheus backend) can supply one
// via Options.BackupMetrics.
type BackupMetricsSource = backupMetricsSource

// freshnessVerdict judges an age against an SLO. A nil age is "no
// data" → "unknown"; it never collapses to a fresh zero. Neither does
// an age from the future: past backupClockSkewTolerance the stamp is
// an artefact that bounds nothing, so it reads "unknown" and carries
// its RAW (negative) age so an operator can see the skew that caused
// it — the one case where an "unknown" verdict has a non-nil age.
func freshnessVerdict(age *time.Duration, slo time.Duration) FreshnessVerdict {
	out := FreshnessVerdict{Status: freshnessUnknown, SLOSeconds: int64(slo / time.Second)}
	if age == nil {
		return out
	}
	secs := int64(*age / time.Second)
	if *age < -backupClockSkewTolerance {
		// Future-dated beyond ordinary skew: the item's real age is
		// unknowable from this stamp, so it must NOT be rewarded with
		// "ok". Surface the raw negative age; leave Status "unknown".
		out.AgeSeconds = &secs
		return out
	}
	if secs < 0 {
		// Ordinary skew on a genuinely fresh item: floor the reported
		// age at 0 rather than publishing "-3s ago".
		secs = 0
	}
	out.AgeSeconds = &secs
	if *age > slo {
		out.Status = freshnessStale
	} else {
		out.Status = freshnessOK
	}
	return out
}

// freshnessRank orders verdicts worst-first for the roll-up.
func freshnessRank(status string) int {
	switch status {
	case freshnessStale:
		return 0
	case freshnessUnknown:
		return 1
	default:
		return 2
	}
}

// overallFreshness is the worst verdict across items (stale > unknown
// > ok — see BackupFreshness.Overall).
func overallFreshness(items ...FreshnessVerdict) string {
	worst := freshnessOK
	for _, it := range items {
		if freshnessRank(it.Status) < freshnessRank(worst) {
			worst = it.Status
		}
	}
	return worst
}

// ageSince returns now-ts as a pointer, or nil for a nil ts.
func ageSince(now time.Time, ts *time.Time) *time.Duration {
	if ts == nil {
		return nil
	}
	d := now.Sub(*ts)
	return &d
}

// parseBackupLabelTime extracts the start time from a pgBackRest
// backup label. Labels are `YYYYMMDD-HHMMSSF` for a full and
// `<full-label>_YYYYMMDD-HHMMSS[D|I]` for a diff/incr — the LAST
// segment is the backup's own start. pgBackRest formats the label on
// the host's local clock; every host this role provisions runs UTC.
func parseBackupLabelTime(label string) (time.Time, bool) {
	seg := label
	if i := strings.LastIndexByte(label, '_'); i >= 0 {
		seg = label[i+1:]
	}
	if len(seg) < len("20060102-150405") {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102-150405", seg[:len("20060102-150405")], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// repoKind maps a pgBackRest repo key onto the role's convention:
// repo1 is the on-host copy, repo2 the S3 offsite copy
// (configs/ansible/roles/archival-node/templates/pgbackrest.conf.j2).
func repoKind(key string) string {
	switch key {
	case "1":
		return "local"
	case "2":
		return "offsite"
	default:
		return "unknown"
	}
}

// PromQL for every series the snapshot reads. The `all-stanzas*`
// pseudo-stanza exclusion mirrors storage.yml's backup alerts: the
// exporter's aggregate row must not mask (or stand in for) the real
// stanza.
const (
	promBackupSinceLast = `pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}`
	promBackupInfo      = `pgbackrest_backup_info{stanza!~"all-stanzas.*"}`
	promBackupFullSize  = `pgbackrest_backup_size_bytes{stanza!~"all-stanzas.*",backup_type="full"}`
	promWALArchiveAge   = `pg_stat_archiver_last_archive_age`
	promDrillSuccess    = `stellarindex_restore_drill_last_success_unix`
	promDrillFailures   = `stellarindex_restore_drill_failures`
	promDrillMtime      = `node_textfile_mtime_seconds{file=~".*restore_drill\\.prom$"}`
	promSnapshotLast    = `stellarindex_ch_schema_snapshot_last_success_unix`
	promSnapshotOffsite = `stellarindex_ch_schema_snapshot_offsite_last_success_unix`
)

// buildBackupsSnapshot assembles one BackupsDiagnostics from the
// metrics source. Every query runs concurrently under ctx; a failed
// query leaves its item nil (→ "unknown") and degrades SourceStatus
// rather than failing the whole document.
func buildBackupsSnapshot(ctx context.Context, src backupMetricsSource, now time.Time) BackupsDiagnostics {
	now = now.UTC()
	exprs := []string{
		promBackupSinceLast, promBackupInfo, promBackupFullSize, promWALArchiveAge,
		promDrillSuccess, promDrillFailures, promDrillMtime,
		promSnapshotLast, promSnapshotOffsite,
	}
	results := make([][]promSample, len(exprs))
	errs := make([]error, len(exprs))
	var wg sync.WaitGroup
	wg.Add(len(exprs))
	for i, expr := range exprs {
		go func(i int, expr string) {
			defer wg.Done()
			results[i], errs[i] = src.queryVector(ctx, expr)
		}(i, expr)
	}
	wg.Wait()

	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	out := BackupsDiagnostics{
		SourceStatus: "ok",
		SLO: BackupSLO{
			FullSeconds:     int64(backupSLOFull / time.Second),
			DiffSeconds:     int64(backupSLODiff / time.Second),
			WALSeconds:      int64(backupSLOWAL / time.Second),
			OffsiteSeconds:  int64(backupSLOOffsite / time.Second),
			DrillSeconds:    int64(backupSLODrill / time.Second),
			SnapshotSeconds: int64(backupSLOSnapshot / time.Second),
		},
	}
	switch {
	case failed == len(exprs):
		out.SourceStatus = "unknown"
	case failed > 0:
		out.SourceStatus = "degraded"
	}

	pg := projectPostgres(now, results[0], results[1], results[2], results[3])
	out.Postgres = pg.section
	out.RestoreDrill = projectRestoreDrill(results[4], results[5], results[6])
	out.ClickHouse.SchemaSnapshotLastTS = firstUnixTime(results[7])
	out.ClickHouse.SchemaSnapshotOffsiteLastTS = firstUnixTime(results[8])

	out.Freshness = BackupFreshness{
		Full:     freshnessVerdict(pg.fullAge, backupSLOFull),
		Diff:     freshnessVerdict(pg.diffAge, backupSLODiff),
		WAL:      freshnessVerdict(pg.walAge, backupSLOWAL),
		Offsite:  freshnessVerdict(ageSince(now, pg.offsiteTS), backupSLOOffsite),
		Drill:    freshnessVerdict(ageSince(now, out.RestoreDrill.LastSuccessTS), backupSLODrill),
		Snapshot: freshnessVerdict(ageSince(now, out.ClickHouse.SchemaSnapshotLastTS), backupSLOSnapshot),
	}
	out.Freshness.Overall = overallFreshness(
		out.Freshness.Full, out.Freshness.Diff, out.Freshness.WAL,
		out.Freshness.Offsite, out.Freshness.Drill, out.Freshness.Snapshot,
	)
	return out
}

// postgresProjection is projectPostgres's result: the wire section
// plus the ages the verdicts are judged on (kept as durations so the
// exporter-computed ages are used verbatim, not re-derived from a
// rounded timestamp).
type postgresProjection struct {
	section   BackupPostgres
	fullAge   *time.Duration
	diffAge   *time.Duration
	walAge    *time.Duration
	offsiteTS *time.Time
}

// projectPostgres maps the pgbackrest_exporter + postgres_exporter
// samples onto the Postgres section.
func projectPostgres(now time.Time, sinceLast, backupInfo, fullSizes, walSamples []promSample) postgresProjection {
	var p postgresProjection
	p.fullAge, p.diffAge = minAgeByType(sinceLast)
	if p.fullAge != nil {
		p.section.LastFull = &BackupRun{TS: now.Add(-*p.fullAge)}
	}
	if p.diffAge != nil {
		p.section.LastDiff = &BackupRun{TS: now.Add(-*p.diffAge)}
	}
	if p.section.LastFull != nil {
		p.section.LastFull.SizeBytes, p.section.LastFull.Repo = newestFullSize(fullSizes)
	}
	p.section.Repos, p.offsiteTS = projectRepos(backupInfo)
	for _, s := range walSamples {
		if v, ok := s.Float(); ok {
			p.section.WALArchiveMaxAgeSeconds = &v
			d := time.Duration(v * float64(time.Second))
			p.walAge = &d
			break
		}
	}
	return p
}

// minAgeByType returns the smallest since-last-completion age per
// backup_type for full and diff (nil when that type has no sample).
func minAgeByType(sinceLast []promSample) (fullAge, diffAge *time.Duration) {
	for _, s := range sinceLast {
		v, ok := s.Float()
		if !ok {
			continue
		}
		d := time.Duration(v * float64(time.Second))
		switch bt, _ := s.Labels["backup_type"].(string); bt {
		case "full":
			if fullAge == nil || d < *fullAge {
				fullAge = &d
			}
		case "diff":
			if diffAge == nil || d < *diffAge {
				diffAge = &d
			}
		}
	}
	return fullAge, diffAge
}

// newestFullSize picks the size + repo of the newest full backup,
// keyed by backup label (labels sort chronologically by construction).
func newestFullSize(fullSizes []promSample) (size *int64, repo string) {
	var newest string
	for _, s := range fullSizes {
		name, _ := s.Labels["backup_name"].(string)
		if name == "" || name < newest {
			continue
		}
		v, ok := s.Float()
		if !ok {
			continue
		}
		newest = name
		n := int64(v)
		size = &n
		repo, _ = s.Labels["repo_key"].(string)
	}
	return size, repo
}

// projectRepos builds the per-repo rows (sorted by key) from the
// per-backup info series and returns the newest offsite stamp.
func projectRepos(backupInfo []promSample) ([]BackupRepo, *time.Time) {
	repoLatest := map[string]time.Time{}
	for _, s := range backupInfo {
		key, _ := s.Labels["repo_key"].(string)
		name, _ := s.Labels["backup_name"].(string)
		if key == "" {
			continue
		}
		t, ok := parseBackupLabelTime(name)
		if !ok {
			// Register the repo even if the label is unparsable so
			// "repo exists, newest unknown" is distinguishable from
			// "repo absent".
			if _, seen := repoLatest[key]; !seen {
				repoLatest[key] = time.Time{}
			}
			continue
		}
		if t.After(repoLatest[key]) {
			repoLatest[key] = t
		}
	}
	keys := make([]string, 0, len(repoLatest))
	for k := range repoLatest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]BackupRepo, 0, len(keys))
	var offsiteTS *time.Time
	for _, k := range keys {
		row := BackupRepo{Repo: k, Kind: repoKind(k)}
		if t := repoLatest[k]; !t.IsZero() {
			row.LastBackupTS = &t
			if row.Kind == "offsite" && (offsiteTS == nil || t.After(*offsiteTS)) {
				offsiteTS = &t
			}
		}
		rows = append(rows, row)
	}
	return rows, offsiteTS
}

// projectRestoreDrill maps the restore-drill textfile series (+ the
// textfile's mtime) onto the drill section.
func projectRestoreDrill(success, failures, mtime []promSample) BackupRestoreDrill {
	out := BackupRestoreDrill{
		Result:        freshnessUnknown,
		LastSuccessTS: firstUnixTime(success),
		LastRunTS:     firstUnixTime(mtime),
	}
	for _, s := range failures {
		if v, ok := s.Float(); ok {
			n := int64(v)
			out.FailedChecks = &n
			if n == 0 {
				out.Result = "pass"
			} else {
				out.Result = "fail"
			}
			break
		}
	}
	return out
}

// firstUnixTime reads the first sample of a unix-seconds gauge as a
// UTC time; nil for an empty vector or a non-positive stamp (an
// epoch-0 placeholder is "never", not 1970).
func firstUnixTime(samples []promSample) *time.Time {
	for _, s := range samples {
		if v, ok := s.Float(); ok && v > 0 {
			t := time.Unix(int64(v), 0).UTC()
			return &t
		}
	}
	return nil
}

// backupsCache is the handler's 60 s in-process snapshot cache.
type backupsCache struct {
	mu      sync.Mutex
	snap    BackupsDiagnostics
	builtAt time.Time
}

// handleDiagnosticsBackups serves GET /v1/diagnostics/backups.
// Anonymous-friendly; 503 when no metrics backend is wired (same
// ladder as /v1/diagnostics/archive's unconfigured state); otherwise
// always 200 with the trust state in the body.
//
// Caching: 60 s in-process (backupsCacheTTL) plus an explicit
// `public, max-age=60` — this overrides the `/v1/diagnostics/`
// prefix rule in middleware/cachecontrol.go (private, no-cache) the
// way /v1/diagnostics/ingestion does, because this document carries
// nothing per-caller and moves on a minutes-to-days cadence.
func (s *Server) handleDiagnosticsBackups(w http.ResponseWriter, r *http.Request) {
	if s.backupMetrics == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/backups-unavailable",
			"Backup diagnostics not available", http.StatusServiceUnavailable,
			"this deployment has no api.prometheus_url configured — backup freshness is read from Prometheus and is not served here")
		return
	}

	now := time.Now().UTC()
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if s.backups.builtAt.IsZero() || now.Sub(s.backups.builtAt) >= backupsCacheTTL {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		s.backups.snap = buildBackupsSnapshot(ctx, s.backupMetrics, now)
		cancel()
		s.backups.builtAt = now
	}
	snap := s.backups.snap

	w.Header().Set("Cache-Control", "public, max-age=60, s-maxage=60")
	// flags.stale mirrors the roll-up so polling clients can spot a
	// non-green document without parsing every verdict.
	writeJSON(w, snap, Flags{Stale: snap.Freshness.Overall != freshnessOK})
}
