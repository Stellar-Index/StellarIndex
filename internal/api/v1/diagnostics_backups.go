package v1

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// BackupsDiagnostics is the GET /v1/diagnostics/backups shape: backup and DR freshness judged against the echoed SLO; nil timestamps mean "unknown", never a fresh zero.
type BackupsDiagnostics struct {
	// SourceStatus is the tri-state trust signal (as StatusResponse.IncidentsStatus): "ok" when
	// every Prometheus query succeeded; "degraded" when some failed, leaving their items nil /
	// "unknown"; "unknown" when all failed.
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
	// LastFull / LastDiff: the most recent completed backup of that type (any repo), from
	// `pgbackrest_backup_since_last_completion_seconds`; nil when the series is absent. The `diff`
	// series counts a full as satisfying a diff, matching pgBackRest's retention semantics.
	LastFull *BackupRun `json:"last_full"`
	LastDiff *BackupRun `json:"last_diff"`

	// WALArchiveMaxAgeSeconds is the age of the newest archived WAL segment
	// (`pg_stat_archiver_last_archive_age`). Nil when the collector is not enabled: "no data", never 0.
	WALArchiveMaxAgeSeconds *float64 `json:"wal_archive_max_age_seconds"`

	// Repos lists every pgBackRest repository, sorted by key. repo1 is the on-host copy, repo2 the
	// encrypted S3 offsite copy (the key→kind mapping is pgbackrest.conf.j2's convention, not a metric).
	Repos []BackupRepo `json:"repos"`
}

// BackupRun is one completed pgBackRest backup.
type BackupRun struct {
	// TS is the backup's completion time (UTC).
	TS WireTime `json:"ts"`
	// SizeBytes is the database size captured (`pgbackrest_backup_size_bytes`). Nil when the
	// exporter has no per-backup row (e.g. the full aged out of `info` but the gauge still reports it).
	SizeBytes *int64 `json:"size_bytes"`
	// Repo is the repository key the backup landed in ("1", "2"); empty when unknown.
	Repo string `json:"repo,omitempty"`
}

// BackupRepo is one pgBackRest repository's freshness row.
type BackupRepo struct {
	// Repo is the pgBackRest repo key ("1", "2").
	Repo string `json:"repo"`
	// Kind is "local" (repo1), "offsite" (repo2) or "unknown".
	Kind string `json:"kind"`
	// LastBackupTS is the start time of the newest backup of any type in this repo, parsed from
	// pgBackRest's label (`YYYYMMDD-HHMMSS`, host-local clock, UTC on provisioned hosts). Nil when
	// the repo has no backups.
	LastBackupTS *WireTime `json:"last_backup_ts"`
	// Retention is the repo's retention policy when known. The
	// exporter does not publish pgBackRest's retention settings, so
	// this is nil today; reserved so the panel's column exists.
	Retention *string `json:"retention"`
}

// BackupRestoreDrill is the monthly pgBackRest restore-drill evidence (ADR-0043 section 3) from restore-drill.sh's textfile.
type BackupRestoreDrill struct {
	// LastRunTS is when the drill last wrote its textfile at all —
	// node_exporter's mtime of restore_drill.prom. Nil = never ran on
	// this host (or the textfile collector can't see it).
	LastRunTS *WireTime `json:"last_run_ts"`
	// LastSuccessTS is the last FULLY-clean run
	// (`stellarindex_restore_drill_last_success_unix`, stamped only
	// when every verification check passed and evidence was written).
	LastSuccessTS *WireTime `json:"last_success_ts"`
	// Result classifies the most recent run: "pass" (zero failed
	// checks), "fail" (one or more), "unknown" (no run recorded).
	Result string `json:"result"`
	// FailedChecks is `stellarindex_restore_drill_failures` for the
	// most recent run; nil when unknown.
	FailedChecks *int64 `json:"failed_checks"`
	// RestoredBackupTS / DurationSeconds are reserved: the drill does
	// not export which backup it restored or how long it took yet.
	RestoredBackupTS *WireTime `json:"restored_backup_ts"`
	DurationSeconds  *float64  `json:"duration_s"`
}

// BackupClickHouse is the lake-protection section (ADR-0043 section 2.1): the lake has no DATA backup by design, only the schema+state snapshot.
type BackupClickHouse struct {
	// SchemaSnapshotLastTS is the last clean schema+state capture
	// (`stellarindex_ch_schema_snapshot_last_success_unix`).
	SchemaSnapshotLastTS *WireTime `json:"schema_snapshot_last_ts"`
	// SchemaSnapshotOffsiteLastTS is the last successful offsite push
	// of that snapshot; nil on a host that acknowledged local-only.
	SchemaSnapshotOffsiteLastTS *WireTime `json:"schema_snapshot_offsite_last_ts"`
	// ZFSSnapshotLatestTS is reserved and always nil: zfs-snapshot.sh
	// publishes `stellarindex_zfs_snapshot_latest_unix` for the lake
	// dataset, but this handler does not read it.
	ZFSSnapshotLatestTS *WireTime `json:"zfs_snapshot_latest_ts"`
	// ReplicaLagSeconds is reserved: the lake is single-node today,
	// so there is no replica and this is always nil.
	ReplicaLagSeconds *float64 `json:"replica_lag_s"`
}

// BackupFreshness holds one verdict per monitored item plus the roll-up; field names are the panel's row keys.
type BackupFreshness struct {
	Full     FreshnessVerdict `json:"full"`
	Diff     FreshnessVerdict `json:"diff"`
	WAL      FreshnessVerdict `json:"wal"`
	Offsite  FreshnessVerdict `json:"offsite"`
	Drill    FreshnessVerdict `json:"drill"`
	Snapshot FreshnessVerdict `json:"snapshot"`

	// Overall is the worst verdict above, with "unknown" ranking between "ok" and "stale": a panel
	// that cannot see its sources is degraded, not green, but a definite SLO breach outranks a blind spot.
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

// Backup freshness SLOs: weekly full, daily diff, archive-async WAL, monthly drill and daily
// snapshot, each with one missed cycle of slack. The alert thresholds in storage.yml /
// restore-drill.yml are deliberately looser because they page people; these only colour a panel.
const (
	backupSLOFull     = 8 * 24 * time.Hour
	backupSLODiff     = 36 * time.Hour
	backupSLOWAL      = 15 * time.Minute
	backupSLOOffsite  = 8 * 24 * time.Hour
	backupSLODrill    = 35 * 24 * time.Hour
	backupSLOSnapshot = 36 * time.Hour
)

// backupClockSkewTolerance bounds how far a stamp may sit in the FUTURE of the API clock and still be judged: a sub-minute
// negative age is NTP skew and floors to 0; beyond it the verdict is "unknown" (degrades the roll-up), never a green zero. 15x
// inside the tightest SLO (WAL, 15 min), so it cannot mask a breach.
const backupClockSkewTolerance = time.Minute

// backupsCacheTTL bounds how often the handler re-queries Prometheus; the freshest signal moves on
// a 5-min WAL cadence, and 60 s keeps a status page polling every 30 s from doubling the load.
const backupsCacheTTL = 60 * time.Second

// backupsQueryBudget bounds a rebuild's Prometheus fan-out; it runs on its own detached context (see handleDiagnosticsBackups)
// because the rebuild is shared via s.backups.buildMu with every queued request.
const backupsQueryBudget = 3 * time.Second

// errBackupsQueryPanicked is recorded when a query goroutine panicked, so it counts toward `failed`.
var errBackupsQueryPanicked = errors.New("backups snapshot: query panicked")

// backupMetricsSource is the seam the diagnostics read Prometheus through; New() derives it from
// Options.StatusBackend, and Options.BackupMetrics overrides it for tests.
type backupMetricsSource interface {
	queryVector(ctx context.Context, expr string) ([]promSample, error)
}

// BackupMetricsSource is the exported alias of the seam so other packages can supply one via Options.BackupMetrics.
type BackupMetricsSource = backupMetricsSource

// freshnessVerdict judges an age against an SLO. A nil age, or one beyond backupClockSkewTolerance in the future, is "unknown",
// never a fresh zero; the future case keeps its RAW negative age so the skew is visible.
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

// parseBackupLabelTime extracts the start time from a pgBackRest label (`YYYYMMDD-HHMMSSF` full,
// `<full-label>_YYYYMMDD-HHMMSS[D|I]` diff/incr; the LAST segment is the backup's own start). Labels use the host's local
// clock; hosts run UTC.
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

// repoKind maps a pgBackRest repo key to the role's convention: repo1 on-host, repo2 S3 offsite
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

// PromQL for every series the snapshot reads. The `all-stanzas*` exclusion mirrors storage.yml's
// backup alerts: the exporter's aggregate row must not mask the real stanza.
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

// buildBackupsSnapshot assembles one BackupsDiagnostics. Queries run concurrently under ctx; a
// failed query leaves its item nil (→ "unknown") and degrades SourceStatus, not the whole document.
func buildBackupsSnapshot(ctx context.Context, logger *slog.Logger, src backupMetricsSource, now time.Time) BackupsDiagnostics {
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
			// A panicking query must count toward `failed`: a nil errs[i] would read as an empty
			// vector and let SourceStatus report "ok" though the query never ran. The snapshot is
			// rebuilt on the next refresh, so nothing is latched.
			defer func() {
				if rec := recover(); rec != nil {
					worker.Report(logger, "api-backups-snapshot-query", rec)
					errs[i] = errBackupsQueryPanicked
				}
			}()
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
	out.ClickHouse.SchemaSnapshotLastTS = wireTimePtr(firstUnixTime(results[7]))
	out.ClickHouse.SchemaSnapshotOffsiteLastTS = wireTimePtr(firstUnixTime(results[8]))

	out.Freshness = BackupFreshness{
		Full:     freshnessVerdict(pg.fullAge, backupSLOFull),
		Diff:     freshnessVerdict(pg.diffAge, backupSLODiff),
		WAL:      freshnessVerdict(pg.walAge, backupSLOWAL),
		Offsite:  freshnessVerdict(ageSince(now, pg.offsiteTS), backupSLOOffsite),
		Drill:    freshnessVerdict(ageSince(now, wireTimeUnptr(out.RestoreDrill.LastSuccessTS)), backupSLODrill),
		Snapshot: freshnessVerdict(ageSince(now, wireTimeUnptr(out.ClickHouse.SchemaSnapshotLastTS)), backupSLOSnapshot),
	}
	out.Freshness.Overall = overallFreshness(
		out.Freshness.Full, out.Freshness.Diff, out.Freshness.WAL,
		out.Freshness.Offsite, out.Freshness.Drill, out.Freshness.Snapshot,
	)
	return out
}

// postgresProjection is projectPostgres's result: the wire section plus the ages verdicts are judged on (durations, so
// exporter-computed ages are used verbatim, not re-derived from a rounded timestamp).
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
		p.section.LastFull = &BackupRun{TS: WireTime(now.Add(-*p.fullAge))}
	}
	if p.diffAge != nil {
		p.section.LastDiff = &BackupRun{TS: WireTime(now.Add(-*p.diffAge))}
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
			row.LastBackupTS = wireTimePtr(&t)
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
		LastSuccessTS: wireTimePtr(firstUnixTime(success)),
		LastRunTS:     wireTimePtr(firstUnixTime(mtime)),
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

// firstUnixTime reads the first sample of a unix-seconds gauge as UTC; nil for an empty vector or non-positive stamp (epoch-0
// placeholder is "never", not 1970).
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
	mu      sync.Mutex // guards snap and builtAt only; never held across IO
	snap    BackupsDiagnostics
	builtAt time.Time
	// buildMu admits one rebuild at a time so concurrent stale readers
	// share the leader's Prometheus fan-out instead of each running one.
	buildMu sync.Mutex
}

// cached returns the snapshot if it is within backupsCacheTTL of now.
func (c *backupsCache) cached(now time.Time) (BackupsDiagnostics, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.builtAt.IsZero() || now.Sub(c.builtAt) >= backupsCacheTTL {
		return BackupsDiagnostics{}, false
	}
	return c.snap, true
}

// rebuildBackups refreshes s.backups and returns the snapshot to serve.
func (s *Server) rebuildBackups() BackupsDiagnostics {
	s.backups.buildMu.Lock()
	defer s.backups.buildMu.Unlock()
	now := time.Now().UTC()
	if snap, ok := s.backups.cached(now); ok {
		return snap // the rebuild this caller queued behind already refreshed it
	}
	// Detached from r.Context(): the rebuild is shared with every request
	// queued on buildMu, so the leader's disconnect must not fail it for all.
	ctx, cancel := context.WithTimeout(context.Background(), backupsQueryBudget)
	fresh := buildBackupsSnapshot(ctx, s.logger, s.backupMetrics, now)
	cancel()
	// builtAt is stamped AFTER the build, else the cache would count as fresh for up to
	// backupsQueryBudget longer than it is.
	builtAt := time.Now().UTC()
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if fresh.SourceStatus == "unknown" && !s.backups.builtAt.IsZero() {
		// Every query failed (or the fan-out was starved): keep the previous good snapshot rather
		// than an all-"unknown" document, but advance builtAt so requests do not hammer Prometheus
		// until a query succeeds again.
		s.backups.builtAt = builtAt
	} else {
		s.backups.snap = fresh
		s.backups.builtAt = builtAt
	}
	return s.backups.snap
}

// handleDiagnosticsBackups serves GET /v1/diagnostics/backups: 503 when no metrics backend is wired, else 200 with the trust
// state. Cached in-process (backupsCacheTTL) plus `public, max-age=60`, overriding the `/v1/diagnostics/` no-cache rule in
// middleware/cachecontrol.go (not per-caller).
func (s *Server) handleDiagnosticsBackups(w http.ResponseWriter, r *http.Request) {
	if s.backupMetrics == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/backups-unavailable",
			"Backup diagnostics not available", http.StatusServiceUnavailable,
			"this deployment has no api.prometheus_url configured — backup freshness is read from Prometheus and is not served here")
		return
	}

	snap, ok := s.backups.cached(time.Now().UTC())
	if !ok {
		snap = s.rebuildBackups() //nolint:contextcheck // intentional detach; see rebuildBackups
	}

	w.Header().Set("Cache-Control", "public, max-age=60, s-maxage=60")
	// flags.stale mirrors the roll-up so polling clients can spot a
	// non-green document without parsing every verdict.
	writeJSON(w, snap, Flags{Stale: snap.Freshness.Overall != freshnessOK, Degraded: snap.SourceStatus != "ok"})
}
