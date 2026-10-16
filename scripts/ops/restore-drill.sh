#!/usr/bin/env bash
# restore-drill.sh — a backup that has never been restored is a hope, not a
# backup. NON-DESTRUCTIVE scratch restore of the pgBackRest stanza + a ClickHouse
# re-derive sample, results appended to the drill evidence log (ADR-0043).
#
# Safe by construction:
#   - restores into a throwaway data dir under $DRILL_ROOT
#   - starts a DISPOSABLE postgres on $DRILL_PG_PORT (never 5432)
#   - read-only against the live DB (comparison queries only)
#   - refuses to run unless the drill volume has room for the restore, sized from
#     the backup itself (capacity precondition below), never less than $MIN_FREE_GB
#   - cleans up on exit (trap), even on failure
#   - one drill at a time: a non-blocking lock ($DRILL_LOCK) serialises the repo1
#     and repo2 timers and any hand-run (they share the scratch port and the pool
#     the capacity check sizes); a separate pre-flight refuses when a postgres
#     left by a KILLED drill still answers on $DRILL_PG_PORT, which no lock covers
#   - every run that gets past the preconditions leaves evidence: a drill-log
#     entry AND a rewritten per-repo textfile metric (`repo` label), whether it
#     passed, failed a check, or aborted mid-restore
#
# Usage (r1, as root):
#   bash scripts/ops/restore-drill.sh                 # pg drill, repo1 (restore-drill.timer)
#   DRILL_REPO=2 bash scripts/ops/restore-drill.sh    # prove the OFFSITE copy (restore-drill-offsite.timer)
#   DRILL_CH_WINDOW=100000 bash scripts/ops/restore-drill.sh  # + CH re-derive sample
#
# Exit code: 1 if any verification check failed (count is in the evidence log and
# the failures metric), else 0; 2 for a precondition refusal (wrong user, missing
# tool, too little free space, or drill-script/binary flag drift, see the CH
# preflight). A refusal is deliberately NOT a verification failure: "the drill
# could not honestly run" and "the drill ran and found a problem" must not share
# a signal.
set -euo pipefail

STANZA="${DRILL_STANZA:-stellarindex}"
DRILL_REPO="${DRILL_REPO:-1}"
# Not /var/tmp: PrivateTmp=true in restore-drill.service gives the unit a private
# /tmp AND /var/tmp, so a root there is invisible to the service (226/NAMESPACE).
DRILL_ROOT="${DRILL_ROOT:-/srv/restore-drill}"
DRILL_PG_PORT="${DRILL_PG_PORT:-5499}"
# ABSOLUTE floor only. The real requirement is derived per run from the backup
# size (below); this is the minimum it is clamped up to.
MIN_FREE_GB="${MIN_FREE_GB:-200}"
# Margin over the backup's database size (percent) + fixed headroom for the WAL
# archive-get pulls into pg_wal during recovery.
DRILL_SIZE_MARGIN_PCT="${DRILL_SIZE_MARGIN_PCT:-125}"
DRILL_WAL_HEADROOM_GB="${DRILL_WAL_HEADROOM_GB:-50}"
# Pool free space run-heavy-job.sh's watchdog stops the job below
# (HEAVY_MIN_DATA_KB): the restore must fit above it or it is killed mid-way.
DRILL_POOL_FLOOR_GB="${DRILL_POOL_FLOOR_GB:-$(( ${HEAVY_MIN_DATA_KB:-314572800} / 1048576 ))}"
PG_VERSION="${PG_VERSION:-15}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
LIVE_DSN="${STELLARINDEX_POSTGRES_DSN:-}"
DRILL_LOG_NOTE="${DRILL_LOG_NOTE:-}"
OPS_BIN="${OPS_BIN:-/usr/local/bin/stellarindex-ops}"
OPS_CONFIG="${OPS_CONFIG:-/etc/stellarindex.toml}"
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
# CH as ops_monitor once the role renders this file; /dev/null (CH `default`, no
# credential) until then, so no deploy order strands this script.
CH_NETRC="${CH_NETRC:-/etc/clickhouse-client/ops-monitor.netrc}"
if [[ ! -r "$CH_NETRC" ]]; then
  echo "restore-drill.sh: credential file $CH_NETRC unreadable; using ClickHouse default user" >&2
  CH_NETRC=/dev/null
fi
# Bucket the CH re-derive reads. The window is ~1M ledgers below the tip, i.e.
# history, which on r1 lives in galexie-archive, NOT the trimmed galexie-live
# default. Passed explicitly so the drill never depends on a seam the host lacks.
DRILL_CH_BUCKET="${DRILL_CH_BUCKET:-galexie-archive}"
# WAL replay from the last backup to now legitimately takes tens of minutes: a
# daily-diff schedule means up to ~24h of busy-ingest WAL through archive-get.
PG_START_TIMEOUT="${PG_START_TIMEOUT:-7200}"
# Ceiling on draining the archive stream after consistency.
WAL_DRAIN_TIMEOUT="${WAL_DRAIN_TIMEOUT:-3600}"
# Evidence log + metric destinations: EXPLICIT host paths (env-overridable), NOT
# computed from $0. The installed copy runs as /usr/local/bin/restore-drill.sh,
# where a $0-relative LOG_DIR resolved to a non-existent path and silently
# dropped the whole evidence phase. /var is writable under the unit's
# ProtectSystem=full. To commit evidence from a checkout:
# RESTORE_DRILL_LOG_DIR=$(pwd)/docs/operations/drills.
LOG_DIR="${RESTORE_DRILL_LOG_DIR:-/var/lib/stellarindex/restore-drills}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
# Held for the life of the run (one-drill-at-a-time precondition). Under /run,
# which PrivateTmp does not shadow and ProtectSystem=full leaves writable; NOT
# under DRILL_ROOT, so a refusal leaves the drill volume as it found it.
DRILL_LOCK="${DRILL_LOCK:-/run/lock/restore-drill.lock}"
# The nightly backup's unit, and how long to wait for it. A drill that starts
# while it is still writing restores the PREVIOUS night's backup and must then
# drain a day of WAL, which overran WAL_DRAIN_TIMEOUT on r1 (2026-10-15).
BACKUP_UNIT="${BACKUP_UNIT:-pgbackrest-backup.service}"
BACKUP_WAIT_TIMEOUT="${BACKUP_WAIT_TIMEOUT:-21600}"

fail_count=0
note() { echo "restore-drill: $*" >&2; }
check() { # check <name> <ok:0|1> <detail>
  local name="$1" ok="$2" detail="$3"
  if [[ "$ok" == "0" ]]; then
    note "FAIL  $name — $detail"
    fail_count=$((fail_count + 1))
  else
    note "OK    $name — $detail"
  fi
}

# ─── preconditions ──────────────────────────────────────────────────
[[ "$(id -u)" == "0" ]] || { note "run as root (drops to postgres for pg ops)"; exit 2; }
# The repo number is a metric label and part of a file name, so it must be a
# small positive integer.
[[ "$DRILL_REPO" =~ ^[1-9][0-9]*$ ]] || { note "DRILL_REPO must be a pgBackRest repo number, got '$DRILL_REPO'"; exit 2; }
command -v pgbackrest >/dev/null || { note "pgbackrest not installed"; exit 2; }
[[ -x "$PG_BIN/postgres" ]] || { note "postgres $PG_VERSION binaries not at $PG_BIN"; exit 2; }

# CH-stage preconditions, checked HERE rather than four hours into the run. An
# invocation ch-backfill cannot parse (a flag it lacks, such as `-database
# drill_scratch`) is rejected at PARSE time by flag.ContinueOnError; recorded as
# an ordinary `check "ch_rederive" 0` with output swallowed, a drill-script BUG
# would look like a genuine re-derive failure and the stage would silently never
# run. A broken invocation must fail LOUDLY, immediately and DIFFERENTLY from a
# drill that found a problem: a precondition (exit 2), not a counted failure
# averaged into an RTO number.
if [[ -n "${DRILL_CH_WINDOW:-}" ]]; then
  [[ -x "$OPS_BIN" ]] || { note "DRILL_CH_WINDOW set but $OPS_BIN is not executable"; exit 2; }
  [[ -r "$OPS_CONFIG" ]] || { note "DRILL_CH_WINDOW set but $OPS_CONFIG is not readable"; exit 2; }
  ch_usage="$("$OPS_BIN" ch-backfill -help 2>&1 || true)"
  missing_flags=()
  for f in config from to bucket dry-run; do
    grep -qE "^[[:space:]]*-${f}([[:space:]]|$)" <<<"$ch_usage" || missing_flags+=("-$f")
  done
  if (( ${#missing_flags[@]} > 0 )); then
    note "─────────────────────────────────────────────────────────────"
    note "DRILL SCRIPT BUG — NOT a backup or re-derive failure."
    note "ch-backfill does not accept: ${missing_flags[*]}"
    note "This drill script and $OPS_BIN have drifted apart; the CH"
    note "re-derive stage would fail at flag-parse time and be recorded"
    note "as a re-derive failure. Refusing to run it. ch-backfill -help:"
    note "─────────────────────────────────────────────────────────────"
    printf '%s\n' "$ch_usage" >&2
    exit 2
  fi
fi

# Capacity precondition, DERIVED from the backup, not a constant. The drill
# writes a full database copy onto the SAME pool the live Postgres and ClickHouse
# use, with no quota on the restore-drill dataset. A fixed 200 G floor sat below
# the ~600 G database, so a pool at 500 G free passed, filled mid-restore and
# stalled live WAL + CH merges. Size from the latest backup in the stanza/repo:
# database size x margin + WAL headroom, clamped to MIN_FREE_GB. A backup that
# cannot be sized is a refusal (exit 2), not a guess.
command -v jq >/dev/null || { note "jq not installed (needed to size the restore from 'pgbackrest info')"; exit 2; }

# One drill at a time. Two timers drive this script (repo1 first Saturday, repo2
# the 15th via restore-drill-offsite.timer) and a hand run can land beside either.
# Two drills would share DRILL_PG_PORT and each size its capacity check against
# space the other is about to consume. So the lock is taken BEFORE the capacity
# check, non-blocking, and a held lock is a refusal (exit 2, nothing written), not
# a counted failure. flock ties the lock to the open descriptor, so it is
# released however this process ends.
command -v flock >/dev/null || { note "flock not installed (needed to run one drill at a time)"; exit 2; }
# Opening the lock file is itself a precondition: under `set -e` a failed `exec
# 9>` exits 1, this script's code for ONE FAILED CHECK, so an unwritable /run/lock
# (read-only mount, stray directory, hidden namespace) would read as "the backup
# failed a check". Nothing is written yet, so exit 2 like every refusal.
exec 9>"$DRILL_LOCK" || { note "could not open the lock file $DRILL_LOCK — refusing (one drill at a time)"; exit 2; }
flock -n 9 || { note "another restore drill holds $DRILL_LOCK — refusing (one drill at a time)"; exit 2; }

# The scratch port, which the lock does NOT cover: the lock guards a CONCURRENT
# drill, not the wreckage of a killed one. A run SIGKILLed after pg_start
# (TimeoutStartSec, OOM, `kill -9`) leaves a DAEMONISED postgres on
# DRILL_PG_PORT (the EXIT trap never ran) while its lock died with the shell, so
# the next drill restores several hundred GB and only then fails at pg_start on
# "address already in use": hours of pool I/O to record a failure that says
# nothing about the backup. Probe first and refuse (exit 2, nothing written),
# naming the datadir to stop. Skipped where pg_isready is absent.
if [[ -x "$PG_BIN/pg_isready" ]]; then
  isready_rc=0
  "$PG_BIN/pg_isready" -h 127.0.0.1 -p "$DRILL_PG_PORT" -t 5 >/dev/null 2>&1 || isready_rc=$?
  # 0 = accepting, 1 = rejecting (a server IS there, still starting), 2 = no
  # response (port free), 3 = bad invocation.
  if [[ "$isready_rc" == "0" || "$isready_rc" == "1" ]]; then
    note "a postgres already answers on $DRILL_PG_PORT — an earlier drill was killed before its cleanup ran; refusing (one scratch instance at a time)"
    for pidfile in "$DRILL_ROOT"/pgdata-*/postmaster.pid; do
      [[ -f "$pidfile" ]] || continue
      note "  stop it: sudo -u postgres $PG_BIN/pg_ctl -D ${pidfile%/postmaster.pid} stop -m immediate  (then remove that directory)"
    done
    exit 2
  fi
fi

# Wait out a running backup before choosing what to restore. A oneshot reads
# "activating" while it runs. Still running at the deadline is a refusal.
if command -v systemctl >/dev/null; then
  backup_deadline=$(( $(date +%s) + BACKUP_WAIT_TIMEOUT ))
  while [[ "$(systemctl is-active "$BACKUP_UNIT" 2>/dev/null || true)" =~ ^(active|activating|reloading)$ ]]; do
    if (( $(date +%s) >= backup_deadline )); then
      note "$BACKUP_UNIT still running after ${BACKUP_WAIT_TIMEOUT}s — refusing (the drill would restore the previous backup)"
      exit 2
    fi
    note "waiting for $BACKUP_UNIT to finish before choosing the backup"
    sleep 60
  done
fi

backup_bytes=$(sudo -u postgres pgbackrest --stanza="$STANZA" --repo="$DRILL_REPO" --output=json info 2>/dev/null \
  | jq -r --arg s "$STANZA" '[.[] | select(.name == $s) | .backup[-1].info.size // empty][0] // empty') || backup_bytes=""
if [[ ! "$backup_bytes" =~ ^[0-9]+$ ]] || (( backup_bytes == 0 )); then
  note "could not size the latest backup in stanza=$STANZA repo=$DRILL_REPO from 'pgbackrest info' — refusing"
  exit 2
fi
backup_gb=$(( (backup_bytes + 1073741823) / 1073741824 ))   # GiB, matching df -BG
need_gb=$(( backup_gb * DRILL_SIZE_MARGIN_PCT / 100 + DRILL_WAL_HEADROOM_GB ))
(( need_gb < MIN_FREE_GB )) && need_gb=$MIN_FREE_GB

mkdir -p "$DRILL_ROOT"
free_gb=$(df -BG --output=avail "$DRILL_ROOT" | tail -1 | tr -dc '0-9')
if (( free_gb < need_gb + DRILL_POOL_FLOOR_GB )); then
  note "only ${free_gb}G free under $DRILL_ROOT, need ${need_gb}G (latest backup ${backup_gb}G × ${DRILL_SIZE_MARGIN_PCT}% + ${DRILL_WAL_HEADROOM_GB}G WAL headroom; floor ${MIN_FREE_GB}G) + ${DRILL_POOL_FLOOR_GB}G pool floor — refusing"
  exit 2
fi
note "capacity: ${free_gb}G free under $DRILL_ROOT ≥ ${need_gb}G needed for a ${backup_gb}G backup (+ ${DRILL_POOL_FLOOR_GB}G pool floor)"

DATA_DIR="$DRILL_ROOT/pgdata-$(date +%Y%m%d-%H%M%S)"
# restore wipes its spool-path's archive dir; the default one holds the live
# cluster's archive-push acks, and losing them stalls its archiver.
SPOOL_DIR="$DATA_DIR.spool"
# 1 while `pgbackrest restore` is writing DATA_DIR. A restore that dies part-way
# (ENOSPC, repo corruption, kill) leaves a multi-hundred-GB partial copy with NO
# diagnostic value (the pgbackrest output is the diagnosis) that must not linger
# on the shared pool. Post-restore failures differ: the datadir IS the evidence,
# and is kept.
restore_in_progress=0
evidence_recorded=0
drill_stage="startup"
# shellcheck disable=SC2317,SC2329  # invoked indirectly via the EXIT trap below
cleanup() {
  local rc=$?
  # Any exit that bypassed record_evidence (an unguarded psql/tee/substitution
  # dying under `set -e`) is still a drill that ran: count and record it rather
  # than leave the old metric ageing.
  if [[ "$evidence_recorded" -eq 0 ]]; then
    [[ "$rc" -ne 0 ]] || rc=1
    fail_count=$((fail_count + 1))
    drill_aborted_at="${drill_stage} (unexpected exit, status $rc)"
    note "FAIL  unexpected exit during $drill_stage (status $rc) — recording the run"
    record_evidence
    emit_metric
    rc=1
  fi
  if [[ -f "$DATA_DIR/postmaster.pid" ]]; then
    sudo -u postgres "$PG_BIN/pg_ctl" -D "$DATA_DIR" stop -m immediate || true
  fi
  if [[ "$restore_in_progress" -eq 1 ]]; then
    echo "restore-drill: removing PARTIAL restore $DATA_DIR (pgbackrest restore did not complete; its output above is the diagnostic)" >&2
    rm -rf "$DATA_DIR" "$SPOOL_DIR"
  elif [[ "$fail_count" -eq 0 ]]; then
    rm -rf "$DATA_DIR" "$SPOOL_DIR"
  else
    echo "restore-drill: KEEPING $DATA_DIR for diagnosis (failures=$fail_count); delete it manually" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT

# ─── evidence + metric (phases 5/6, callable from every exit path) ──
# The evidence IS the deliverable (ADR-0043 §3). A drill that ran but recorded
# nothing is a FAILED drill, so an unwritable evidence log fails LOUDLY and is
# counted, never silently skipped.
#
# FUNCTIONS, not a tail phase, because the likeliest failures (`pgbackrest
# restore` failing, the scratch instance never reaching consistency) `exit`
# before a tail phase would run. The drill log would get no entry and the previous
# textfile (failures=0, fresh last_success) would keep being scraped as if the
# backup had just been proven restorable, until the 40-day staleness window.
# Every run past the preconditions records what it found, on every path.
# Precondition refusals (exit 2) deliberately write nothing.
drill_aborted_at=""
record_evidence() {
  evidence_recorded=1
  local evidence_file="$LOG_DIR/restore-drills.md"
  if mkdir -p "$LOG_DIR" 2>/dev/null && {
        echo "## $(date -u +%F) restore drill (repo${DRILL_REPO})"
        if [[ -n "$drill_aborted_at" ]]; then
          echo "- ABORTED at $drill_aborted_at — no verification ran (restore: ${restore_secs:-n/a}s)"
        else
          echo "- restore: ${restore_secs:-n/a}s; tip lag ${lag:-n/a} ledgers; hash-chain breaks: ${breaks:-n/a}; trades window match: ${restored_rows:-n/a}=${live_rows:-n/a}"
        fi
        [[ -n "${ch_secs:-}" ]] && echo "- CH re-derive (dry-run, fetch+decode only): ${DRILL_CH_WINDOW} ledgers in ${ch_secs}s from ${DRILL_CH_BUCKET}; lake rows in window: ${ch_rows:-n/a}"
        [[ -n "$DRILL_LOG_NOTE" ]] && echo "- note: $DRILL_LOG_NOTE"
        echo "- failures: $fail_count"
        echo
      } >> "$evidence_file"; then
    note "evidence appended to $evidence_file"
  else
    note "FATAL: could not write drill evidence to $evidence_file — the evidence is the drill's only deliverable; recording this run as a FAILURE"
    fail_count=$((fail_count + 1))
  fi
}

# Mirrors ch-schema-snapshot.sh §6: stamp last_success ONLY on a fully clean run
# (fail_count==0, evidence written), so stellarindex_restore_drill_stale fires
# when the monthly drill stops producing evidence and
# stellarindex_restore_drill_failed fires when a run records failures > 0.
# Written atomically (.tmp then mv) under /var (ProtectSystem=full leaves it
# writable).
#
# One textfile PER REPO with a `repo` label on every series: each run rewrites its
# file whole, so a shared file would let a clean repo2 run erase a failed repo1
# verdict, and an unlabelled series could not say which copy it proves. repo1
# keeps the historical file name so an unlabelled series there is REPLACED by the
# next repo1 run, not left as a forever-ageing second series. node_exporter merges
# a family across files when HELP text agrees, which the shared literals
# guarantee (verify-archive tiers rely on the same with `tier`).
# stellarindex_restore_drill_stale selects repo=~"1|" (an allow-list, so a repo3
# sample cannot stand in for a never-drilled repo1);
# stellarindex_restore_drill_offsite_stale selects repo="2". An unlabelled series
# in restore_drill.prom is whichever repo ran LAST before the label existed, read
# on-box as repo1's until a labelled repo1 run rewrites the file.
emit_metric() {
  [[ "$TEXTFILE_DIR" != "/dev/null" ]] || return 0
  if ! mkdir -p "$TEXTFILE_DIR" 2>/dev/null; then
    note "WARN: could not write $TEXTFILE_DIR — restore-drill metric for repo${DRILL_REPO} not emitted (its staleness alert will fire on the absent series)"
    return 0
  fi
  local metric_out="$TEXTFILE_DIR/restore_drill.prom"
  [[ "$DRILL_REPO" == "1" ]] || metric_out="$TEXTFILE_DIR/restore_drill_repo${DRILL_REPO}.prom"
  local metric_tmp="$metric_out.tmp.$$"
  local metric_checked="$metric_tmp.checked"
  local lbl="{repo=\"${DRILL_REPO}\"}"

  # Throughput is the ONLY non-integer value in this file and comes from `bc`,
  # a separate Debian package absent from a minimal image. Missing, the command
  # substitution yields the EMPTY STRING and the line becomes a metric NAME WITH
  # NO VALUE, which costs the file, not one series: node_exporter rejects an
  # unparseable textfile whole, taking _failures and _last_success_unix with it
  # (observed on r1, silent ~20 minutes). So compute it, prove it is a number,
  # and OMIT the series when not; an absent gauge already means "not measured".
  local lps=""
  if [[ -n "${ch_secs:-}" && -n "${DRILL_CH_WINDOW:-}" && "${ch_rc:-1}" -eq 0 ]]; then
    lps="$(echo "scale=2; $DRILL_CH_WINDOW / ($ch_secs + 0.0001)" | bc)" || lps=""
    # bc renders a sub-1 result as `.50`, a valid Prometheus float; two dots, a
    # digit-free string and bc's backslash line-continuation are not.
    case "$lps" in
      '' | '.' | *[!0-9.]* | *.*.*)
        note "WARN: ledgers-per-second came back '$lps' (bc missing or in error) — omitting stellarindex_restore_drill_ch_rederive_ledgers_per_second rather than writing a metric name with no value, which would make node_exporter drop $metric_out whole"
        lps=""
        ;;
    esac
  fi

  {
    echo "# HELP stellarindex_restore_drill_last_success_unix Unix time of the most recent fully-successful pgBackRest restore-drill (ADR-0043 §3 / CS-110)."
    echo "# TYPE stellarindex_restore_drill_last_success_unix gauge"
    if [[ "$fail_count" -eq 0 ]]; then
      echo "stellarindex_restore_drill_last_success_unix${lbl} $(date +%s)"
    fi
    echo "# HELP stellarindex_restore_drill_failures Number of failed verification checks in the most recent restore-drill run."
    echo "# TYPE stellarindex_restore_drill_failures gauge"
    echo "stellarindex_restore_drill_failures${lbl} $fail_count"
    # ClickHouse re-derive throughput (ADR-0043 §2.2): the lake RTO is only honest
    # MEASURED. Emitted only when the CH stage ran and succeeded; absent means
    # "not measured", never a stale figure.
    if [[ -n "${ch_secs:-}" && -n "${DRILL_CH_WINDOW:-}" && "${ch_rc:-1}" -eq 0 ]]; then
      echo "# HELP stellarindex_restore_drill_ch_rederive_seconds Wall seconds the drill took to fetch+decode its ClickHouse re-derive window (dry-run, single-threaded)."
      echo "# TYPE stellarindex_restore_drill_ch_rederive_seconds gauge"
      echo "stellarindex_restore_drill_ch_rederive_seconds${lbl} $ch_secs"
      echo "# HELP stellarindex_restore_drill_ch_rederive_window_ledgers Size of the re-derive window the drill measured."
      echo "# TYPE stellarindex_restore_drill_ch_rederive_window_ledgers gauge"
      echo "stellarindex_restore_drill_ch_rederive_window_ledgers${lbl} $DRILL_CH_WINDOW"
      echo "# HELP stellarindex_restore_drill_ch_rederive_ledgers_per_second Measured re-derive throughput (window / seconds); full-lake RTO ≈ live tip / this / parallelism."
      echo "# TYPE stellarindex_restore_drill_ch_rederive_ledgers_per_second gauge"
      if [[ -n "$lps" ]]; then
        echo "stellarindex_restore_drill_ch_rederive_ledgers_per_second${lbl} $lps"
      fi
    fi
    # Publication safety, not a drill signal. Emitted EVERY run so the healthy
    # value (0) exists as a series rather than an absence.
    echo "# HELP stellarindex_restore_drill_unparseable_lines Exposition lines this run rendered that are not valid Prometheus samples and were therefore WITHHELD from the published textfile. 0 every healthy run; above 0 means a value this drill composed is not a number and that one series is absent this tick."
    echo "# TYPE stellarindex_restore_drill_unparseable_lines gauge"
  } > "$metric_tmp"

  # ── Validate the rendered bytes before publishing ─────────────────
  #
  # The guard above stops the ONE value known able to go non-numeric; this stops
  # the class. Everything shares one .prom, so one bad line costs every series,
  # and they are the only evidence the backups are restorable.
  #
  # WITHHOLD rather than refuse: an unpublished file is not an absent one.
  # node_exporter re-serves the previous restore_drill.prom with a fresh
  # timestamp, so failures and last_success would FREEZE (last month's clean
  # drill reading as this month's, the false-clean the abort paths remove). A
  # withheld line leaves its series absent, which both staleness alerts read.
  #
  # This never changes the exit code: `failure_exit_code` is the verdict on FAILED
  # CHECKS OF THE BACKUP, and a metric-writer fault is not a fact about the backup
  # (same reasoning as the lock refusal). The tally gauge and journal carry it.
  #
  # Braces are written [{] / [}]: in an ERE a bare brace opens an interval
  # expression and the escape is undefined by POSIX; the bracket form means the
  # same under mawk (Debian default), gawk and BWK awk. No apostrophe in the awk
  # program: it is a single-quoted shell word.
  local dropped=""
  if ! dropped="$(awk -v keep="$metric_checked" '
    /^[ \t]*$/ || /^#/ { print > keep; next }
    {
      rest = $0
      sub(/^[a-zA-Z_:][a-zA-Z0-9_:]*([{][^}]*[}])?[ \t]+/, "", rest)
      if (rest != $0 && rest ~ /^[+-]?([0-9]+\.?[0-9]*([eE][+-]?[0-9]+)?|\.[0-9]+([eE][+-]?[0-9]+)?|Inf|NaN)([ \t]+[0-9]+)?[ \t]*$/) {
        print > keep
        next
      }
      printf "restore-drill: withholding unparseable exposition line: %s\n", $0 > "/dev/stderr"
      bad = bad + 1
    }
    END { print bad + 0 }
  ' "$metric_tmp")"; then
    rm -f "$metric_tmp" "$metric_checked"
    note "WARN: the exposition validator did not run — refusing to publish unvalidated bytes to $metric_out, since node_exporter drops an unparseable file whole and says so only in node_textfile_scrape_error. The previous file stays and ages; its staleness alert is what notices."
    return 0
  fi
  # The tally becomes a metric value, so same treatment: a validator that
  # answered with something other than a count validated nothing; refuse rather
  # than coerce to 0.
  case "$dropped" in
    '' | *[!0-9]*)
      rm -f "$metric_tmp" "$metric_checked"
      note "WARN: exposition validator returned a non-numeric tally ('$dropped') — refusing to publish $metric_out"
      return 0
      ;;
  esac
  # Guarded and returns rather than letting `set -e` fire: a failed `mv` would
  # leave $metric_tmp holding the UNVALIDATED render for the publish below, and
  # the caller reads this function's status as a count of failed backup checks.
  if ! mv "$metric_checked" "$metric_tmp"; then
    rm -f "$metric_tmp" "$metric_checked"
    note "WARN: could not swap in the validated render — refusing to publish $metric_out"
    return 0
  fi
  # printf on its own line, not folded into an `if !`: the exposition gate reads
  # lines that BEGIN with echo/printf, and a sample behind a condition keyword is
  # one it stops checking.
  local tally_ok=1
  printf 'stellarindex_restore_drill_unparseable_lines%s %s\n' "$lbl" "$dropped" >> "$metric_tmp" || tally_ok=0
  if [[ "$tally_ok" != 1 ]]; then
    rm -f "$metric_tmp"
    note "WARN: could not append the withheld-line tally — refusing to publish $metric_out"
    return 0
  fi
  if [[ "$dropped" -gt 0 ]]; then
    note "WARN: published $metric_out with $dropped unparseable line(s) withheld — stellarindex_restore_drill_unparseable_lines carries the count"
  fi
  # Mode set explicitly (mktemp-free tmp path): node_exporter runs unprivileged
  # and SILENTLY skips a textfile it cannot read. Chained so failure is a note,
  # not a `set -e` exit carrying this function's status out as the failure count.
  if ! { chmod 644 "$metric_tmp" && mv "$metric_tmp" "$metric_out"; }; then
    rm -f "$metric_tmp"
    note "WARN: could not publish $metric_out — restore-drill metric for repo${DRILL_REPO} not emitted (its staleness alert will fire on the frozen series)"
  fi
  return 0
}

# A raw count would collide with the refusal code at exactly 2 failures.
failure_exit_code() { if [[ "$fail_count" -gt 0 ]]; then echo 1; else echo 0; fi; }

# abort_drill <stage>: a stage the rest of the drill cannot proceed
# without has failed. Record it (evidence + metric) and exit with the
# failure code — never a bare `exit` from mid-drill.
abort_drill() {
  drill_aborted_at="$1"
  note "aborting at $drill_aborted_at — recording the run before exit"
  record_evidence
  emit_metric
  exit "$(failure_exit_code)"
}

# ─── phase 1: restore ───────────────────────────────────────────────
drill_stage="pg_restore"
note "restoring stanza=$STANZA repo=$DRILL_REPO into $DATA_DIR …"
mkdir -p "$DATA_DIR" "$SPOOL_DIR" && chown postgres:postgres "$DATA_DIR" "$SPOOL_DIR" && chmod 700 "$DATA_DIR" "$SPOOL_DIR"
restore_started=$(date +%s)
restore_in_progress=1
if sudo -u postgres pgbackrest --stanza="$STANZA" --repo="$DRILL_REPO" \
     --pg1-path="$DATA_DIR" --spool-path="$SPOOL_DIR" --type=default restore; then
  restore_in_progress=0
  restore_secs=$(( $(date +%s) - restore_started ))
  check "pg_restore" 1 "completed in ${restore_secs}s from repo${DRILL_REPO}"
else
  restore_secs=$(( $(date +%s) - restore_started ))
  check "pg_restore" 0 "pgbackrest restore failed — see output above"
  abort_drill "pg_restore"
fi

# ─── phase 2: start scratch instance + recover ──────────────────────
drill_stage="pg_start"
# Recovery target: end of archived WAL. Disposable instance: no archive_command,
# loopback only, alternate port. Debian keeps the live postgresql.conf and
# pg_hba.conf under /etc/postgresql, NOT in PGDATA, so the restored datadir has
# neither; synthesize minimal ones (config includes the auto file; hba is
# loopback-trust for the postgres OS user only).
if [[ ! -f "$DATA_DIR/postgresql.conf" ]]; then
  sudo -u postgres tee "$DATA_DIR/postgresql.conf" >/dev/null <<CONF
# synthesized by restore-drill.sh — Debian keeps the real config in /etc
include_if_exists = 'postgresql.auto.conf'
CONF
fi
if [[ ! -f "$DATA_DIR/pg_hba.conf" ]]; then
  sudo -u postgres tee "$DATA_DIR/pg_hba.conf" >/dev/null <<CONF
local   all  postgres                trust
host    all  postgres  127.0.0.1/32  trust
CONF
fi
if [[ ! -f "$DATA_DIR/pg_ident.conf" ]]; then
  sudo -u postgres touch "$DATA_DIR/pg_ident.conf"
fi

# Scratch-instance overrides: the restored postgresql.conf carries PRODUCTION
# sizing (tens-of-GB shared_buffers, wide lock tables); a second instance at
# those settings fails or starves the live DB. Downsize everything except what
# recovery needs (timescaledb preload + a lock table big enough for the chunks).
sudo -u postgres tee -a "$DATA_DIR/postgresql.auto.conf" >/dev/null <<CONF
port = $DRILL_PG_PORT
listen_addresses = '127.0.0.1'
archive_mode = off
shared_preload_libraries = 'timescaledb'
shared_buffers = 2GB
effective_cache_size = 4GB
maintenance_work_mem = 256MB
work_mem = 16MB
max_parallel_workers = 2
timescaledb.max_background_workers = 2
hot_standby = on
CONF

# With hot_standby=on, WAL replay ENFORCES that connection/worker GUCs are >= the
# primary's ("recovery aborted because of insufficient parameter settings"), so
# these must MIRROR the live primary, not be downsized. Read them live so a
# primary retune cannot re-break the drill.
sudo -u postgres psql -d stellarindex -tA -F' = ' -c \
  "SELECT name, setting FROM pg_settings WHERE name IN
   ('max_connections','max_worker_processes','max_wal_senders',
    'max_prepared_transactions','max_locks_per_transaction')" \
  | sudo -u postgres tee -a "$DATA_DIR/postgresql.auto.conf" >/dev/null
if sudo -u postgres "$PG_BIN/pg_ctl" -D "$DATA_DIR" -w -t "$PG_START_TIMEOUT" start; then
  check "pg_start" 1 "scratch instance up on :$DRILL_PG_PORT (recovery complete)"
else
  check "pg_start" 0 "scratch instance failed to reach consistency"
  abort_drill "pg_start"
fi

q() { sudo -u postgres psql -h 127.0.0.1 -p "$DRILL_PG_PORT" -d stellarindex -tA -c "$1"; }
qlive() {
  if [[ -n "$LIVE_DSN" ]]; then psql "$LIVE_DSN" -tA -c "$1"
  else sudo -u postgres psql -d stellarindex -tA -c "$1"; fi
}

# ─── phase 3: verification ──────────────────────────────────────────
drill_stage="verification"
# 3a. The restored DB answers and has the core tables.
tables=$(q "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('trades','oracle_updates','ledger_ingest_log','completeness_snapshots')")
check "core_tables" "$([[ "$tables" == "4" ]] && echo 1 || echo 0)" "found $tables/4 core tables"

# 3b. Restored tip is close to the live tip (WAL archiving healthy).
#
# DRAIN THE ARCHIVE STREAM FIRST. `hot_standby = on` plus `pg_ctl -w` accepts
# connections at CONSISTENCY while replay of the remaining archived WAL continues.
# Measuring then answers "how old was the backup", NOT "how far forward can we
# recover", the only claim worth making. Measured on r1 without the drain: lag
# 13,392 ledgers (~18.6h) against a diff backup 21h old, i.e. the backup's AGE;
# on a daily-diff schedule the < 5000 threshold was passable only by luck (a
# 240-ledger pass was that accident).
#
# The finish line is an LSN captured from the LIVE primary now; replay drains to
# it. A standby replays only ARCHIVED WAL, so on a quiet primary the target
# segment may not archive until it fills: hence bounded, and a timeout is its own
# outcome rather than a silent early measurement.
wal_target=$(qlive "SELECT pg_current_wal_lsn()")
drain_deadline=$(( $(date +%s) + WAL_DRAIN_TIMEOUT ))
drained=0
drain_why=""
while (( $(date +%s) < drain_deadline )); do
  # Two terminal states, both meaning "everything available was applied": still
  # in recovery AND replay passed the target LSN; or no longer in recovery.
  # pgBackRest type=default writes recovery.signal, so once the archive stream is
  # exhausted Postgres promotes and pg_last_wal_replay_lsn() returns NULL; polling
  # only the LSN would spin to the timeout on the run that succeeded.
  if [[ "$(q "SELECT pg_is_in_recovery()" 2>/dev/null)" == "f" ]]; then
    drained=1; drain_why="recovery ended (archive stream exhausted, promoted)"
    break
  fi
  replayed=$(q "SELECT coalesce(pg_last_wal_replay_lsn()::text,'')" 2>/dev/null || echo "")
  if [[ -n "$replayed" ]] && [[ "$(q "SELECT ('$replayed'::pg_lsn >= '$wal_target'::pg_lsn)")" == "t" ]]; then
    drained=1; drain_why="replay reached $wal_target"
    break
  fi
  sleep 10
done
check "wal_drain" "$drained" "${drain_why:-did NOT reach $wal_target within ${WAL_DRAIN_TIMEOUT}s (last replayed: ${replayed:-none})}"

restored_tip=$(q "SELECT coalesce(max(ledger_seq),0) FROM ledger_ingest_log")
live_tip=$(qlive "SELECT coalesce(max(ledger_seq),0) FROM ledger_ingest_log")
lag=$(( live_tip - restored_tip ))
# With the stream drained, residual lag is only what the primary wrote DURING the
# drain (minutes). One ledger is ~5-6s, so 5000 (~7h) is a generous ceiling that
# now fails on a real WAL-restore gap, not the backup's age.
check "tip_lag" "$(( lag >= 0 && lag < 5000 ? 1 : 0 ))" "restored tip $restored_tip vs live $live_tip (lag $lag ledgers)"

# 3c. Hash-chain sanity on the restored copy (100k-ledger sample):
# consecutive ledger_ingest_log rows must chain prev_ledger_hash.
breaks=$(q "
  WITH w AS (
    SELECT ledger_seq, ledger_hash, prev_ledger_hash,
           lag(ledger_hash) OVER (ORDER BY ledger_seq) AS prior_hash,
           lag(ledger_seq)  OVER (ORDER BY ledger_seq) AS prior_seq
    FROM ledger_ingest_log
    WHERE ledger_seq > $restored_tip - 100000
  )
  SELECT count(*) FROM w
  WHERE prior_seq = ledger_seq - 1 AND prior_hash IS DISTINCT FROM prev_ledger_hash")
check "hash_chain_sample" "$([[ "$breaks" == "0" ]] && echo 1 || echo 0)" "$breaks chain breaks in the restored 100k tail"

# 3d. Row-count spot agreement on an immutable window (well below tip
# so live writes don't skew it).
window_hi=$(( restored_tip - 50000 )); window_lo=$(( window_hi - 50000 ))
restored_rows=$(q "SELECT count(*) FROM trades WHERE ledger BETWEEN $window_lo AND $window_hi")
live_rows=$(qlive "SELECT count(*) FROM trades WHERE ledger BETWEEN $window_lo AND $window_hi")
check "trades_window_match" "$([[ "$restored_rows" == "$live_rows" ]] && echo 1 || echo 0)" "trades[$window_lo,$window_hi]: restored=$restored_rows live=$live_rows"

# ─── phase 4 (optional): ClickHouse re-derive sample ────────────────
drill_stage="ch_rederive"
# Proves the ADR-0043 §2.2 "lake is re-derivable" claim + measures RTO.
#
# WHY DRY-RUN rather than the ADR's "scratch database": clickhouse.Open pins the
# `stellar` database (internal/storage/clickhouse/sink.go), so ch-backfill has no
# `-database` flag. The only writing alternative is re-deriving into the LIVE
# lake, wrong here: r1's pool is capacity-constrained and the lake already holds
# ~3 TiB of duplicate rows, so re-inserting 100k ledgers of ReplacingMergeTree
# duplicates would spend the scarcest resource on the box.
#
# `-dry-run` fetches every galexie object and runs the full
# clickhouse.ExtractLedger decode (the whole recovery path and nearly all its
# wall-clock), writing nothing. Honest limit, recorded in the log: this measures
# FETCH+DECODE throughput, not the ClickHouse INSERT (which the live sink
# exercises continuously and is not the multi-week bottleneck). The count
# reconcile checks the lake actually holds the window just proved derivable.
if [[ -n "${DRILL_CH_WINDOW:-}" ]]; then
  note "CH re-derive drill: window=$DRILL_CH_WINDOW ledgers, dry-run (see ADR-0043 §2.2)"
  ch_started=$(date +%s)
  lo=$(( restored_tip - 1000000 )); hi=$(( lo + DRILL_CH_WINDOW - 1 ))
  ch_rc=0
  ch_out="$("$OPS_BIN" ch-backfill -config "$OPS_CONFIG" \
    -from "$lo" -to "$hi" -bucket "$DRILL_CH_BUCKET" -dry-run 2>&1)" || ch_rc=$?
  if [[ "$ch_rc" -eq 0 ]]; then
    ch_secs=$(( $(date +%s) - ch_started ))
    per_ledger=$(echo "scale=4; $ch_secs / $DRILL_CH_WINDOW" | bc)
    full_days=$(echo "scale=1; $per_ledger * $live_tip / 86400" | bc)
    check "ch_rederive" 1 "window in ${ch_secs}s (${per_ledger}s/ledger → full rebuild ≈ ${full_days} days single-threaded, fetch+decode only — parallelism divides this)"
    # Reconcile against the live lake (ADR-0043 §2.2): the window just proved
    # re-derivable must already be complete in ClickHouse.
    ch_rows=$(curl -sf --netrc-file "$CH_NETRC" "$CH_HTTP" --data-binary \
      "SELECT count() FROM stellar.ledgers WHERE ledger_seq BETWEEN $lo AND $hi" | tr -dc '0-9')
    check "ch_lake_window_complete" "$([[ "$ch_rows" == "$DRILL_CH_WINDOW" ]] && echo 1 || echo 0)" \
      "stellar.ledgers[$lo,$hi]: lake=${ch_rows:-?} expected=$DRILL_CH_WINDOW"
  else
    # Full output, never `| tail -5`: this stage sat broken for weeks because its
    # diagnosis was truncated away.
    check "ch_rederive" 0 "ch-backfill -dry-run exited $ch_rc — full output below"
    printf '%s\n' "$ch_out" >&2
  fi
fi

# ─── phase 5/6: evidence log + node_exporter textfile metric ────────
# (defined above so the abort paths share them)
record_evidence
emit_metric

note "done: $fail_count failure(s)"
exit "$(failure_exit_code)"
