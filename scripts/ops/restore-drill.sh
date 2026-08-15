#!/usr/bin/env bash
# restore-drill.sh — the CS-110 answer: a backup that has never been
# restored is a hope, not a backup. NON-DESTRUCTIVE scratch restore
# of the pgBackRest stanza + a ClickHouse re-derive sample, with the
# results appended to the drill evidence log (ADR-0043).
#
# Safe by construction:
#   - restores into a throwaway data dir under $DRILL_ROOT
#   - starts a DISPOSABLE postgres on $DRILL_PG_PORT (never 5432)
#   - read-only against the live DB (comparison queries only)
#   - refuses to run with <  $MIN_FREE_GB free on the drill volume
#   - cleans up on exit (trap), even on failure
#
# Usage (r1, as root):
#   bash scripts/ops/restore-drill.sh                 # pg drill, repo1
#   DRILL_REPO=2 bash scripts/ops/restore-drill.sh    # prove the OFFSITE copy
#   DRILL_CH_WINDOW=100000 bash scripts/ops/restore-drill.sh  # + CH re-derive sample
#
# Exit code: number of failed verification checks; 2 for a precondition
# refusal (wrong user, missing tool, too little free space, or a
# drill-script/binary flag drift — see the CH preflight below). A
# precondition refusal is deliberately NOT counted as a verification
# failure: "the drill could not honestly run" and "the drill ran and
# found a problem" are different facts and must not share a signal.
set -euo pipefail

STANZA="${DRILL_STANZA:-stellarindex}"
DRILL_REPO="${DRILL_REPO:-1}"
DRILL_ROOT="${DRILL_ROOT:-/var/tmp/restore-drill}"
DRILL_PG_PORT="${DRILL_PG_PORT:-5499}"
MIN_FREE_GB="${MIN_FREE_GB:-200}"
PG_VERSION="${PG_VERSION:-15}"
PG_BIN="/usr/lib/postgresql/${PG_VERSION}/bin"
LIVE_DSN="${STELLARINDEX_POSTGRES_DSN:-}"
DRILL_LOG_NOTE="${DRILL_LOG_NOTE:-}"
OPS_BIN="${OPS_BIN:-/usr/local/bin/stellarindex-ops}"
OPS_CONFIG="${OPS_CONFIG:-/etc/stellarindex.toml}"
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
# Bucket the CH re-derive reads. The window is ~1M ledgers below the
# tip, i.e. history — which on r1 lives in galexie-archive, NOT the
# trimmed galexie-live default (the 5179250a wrong-bucket class). Passed
# explicitly so the drill never depends on a seam this host has not set.
DRILL_CH_BUCKET="${DRILL_CH_BUCKET:-galexie-archive}"
# WAL replay from the last backup to now legitimately takes tens of
# minutes (third drill failure mode: a daily-diff schedule means up
# to ~24h of a busy ingest DB's WAL replays through archive-get).
PG_START_TIMEOUT="${PG_START_TIMEOUT:-7200}"
# Evidence log + metric destinations. Both are EXPLICIT host paths
# (env-overridable), NOT computed from $0 — the installed copy runs as
# /usr/local/bin/restore-drill.sh, where the old $0-relative LOG_DIR
# resolved to a non-existent /usr/docs/operations/drills and silently
# dropped the whole evidence phase (BDR-03). /var is left writable by
# the systemd unit's ProtectSystem=full. Operators running from a
# checkout who want the evidence committed can point LOG_DIR at the
# repo: RESTORE_DRILL_LOG_DIR=$(pwd)/docs/operations/drills.
LOG_DIR="${RESTORE_DRILL_LOG_DIR:-/var/lib/stellarindex/restore-drills}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

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
command -v pgbackrest >/dev/null || { note "pgbackrest not installed"; exit 2; }
[[ -x "$PG_BIN/postgres" ]] || { note "postgres $PG_VERSION binaries not at $PG_BIN"; exit 2; }

# CH-stage preconditions, checked HERE rather than four hours into the
# run (2026-07-25): this stage passed `-database drill_scratch` — a flag
# ch-backfill has never had — so flag.ContinueOnError rejected the
# invocation at PARSE time. The failure was recorded as
# `check "ch_rederive" 0 "ch-backfill sample failed"` and its output was
# swallowed by `| tail -5`, so a drill-script BUG was indistinguishable
# from a genuine re-derive failure. The optional stage has therefore
# never once run since it shipped.
#
# A drill whose own invocation is broken must fail LOUDLY, immediately,
# and DIFFERENTLY from a drill that ran and found a problem: this is a
# precondition (exit 2, same class as "pgbackrest not installed"), not a
# verification failure that gets counted, logged as evidence and averaged
# into an RTO number.
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

mkdir -p "$DRILL_ROOT"
free_gb=$(df -BG --output=avail "$DRILL_ROOT" | tail -1 | tr -dc '0-9')
if (( free_gb < MIN_FREE_GB )); then
  note "only ${free_gb}G free under $DRILL_ROOT (< ${MIN_FREE_GB}G) — refusing"
  exit 2
fi

DATA_DIR="$DRILL_ROOT/pgdata-$(date +%Y%m%d-%H%M%S)"
cleanup() {
  if [[ -f "$DATA_DIR/postmaster.pid" ]]; then
    sudo -u postgres "$PG_BIN/pg_ctl" -D "$DATA_DIR" stop -m immediate || true
  fi
  if [[ "$fail_count" -eq 0 ]]; then
    rm -rf "$DATA_DIR"
  else
    echo "restore-drill: KEEPING $DATA_DIR for diagnosis (failures=$fail_count); delete it manually" >&2
  fi
}
trap cleanup EXIT

# ─── phase 1: restore ───────────────────────────────────────────────
note "restoring stanza=$STANZA repo=$DRILL_REPO into $DATA_DIR …"
mkdir -p "$DATA_DIR" && chown postgres:postgres "$DATA_DIR" && chmod 700 "$DATA_DIR"
restore_started=$(date +%s)
if sudo -u postgres pgbackrest --stanza="$STANZA" --repo="$DRILL_REPO" \
     --pg1-path="$DATA_DIR" --type=default restore; then
  restore_secs=$(( $(date +%s) - restore_started ))
  check "pg_restore" 1 "completed in ${restore_secs}s from repo${DRILL_REPO}"
else
  check "pg_restore" 0 "pgbackrest restore failed — see output above"
  exit "$fail_count"
fi

# ─── phase 2: start scratch instance + recover ──────────────────────
# Recovery target: end of archived WAL. Disposable instance — no
# archive_command, loopback only, alternate port.
# Debian layout: the live cluster's postgresql.conf + pg_hba.conf live
# under /etc/postgresql, NOT in PGDATA — so the restored datadir has
# neither (second drill failure mode, 2026-07-03). Synthesize minimal
# ones: config just includes the auto file; hba is loopback-trust for
# the postgres OS user only (the instance binds 127.0.0.1 on a
# non-standard port and dies at drill end).
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

# Scratch-instance overrides: the restored/live postgresql.conf carries
# PRODUCTION sizing (tens-of-GB shared_buffers, wide lock tables) —
# a second instance at those settings fails or starves the live DB.
# Downsize everything except what recovery correctness needs
# (timescaledb preload + a lock table big enough for the chunk count).
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

# Fourth drill failure mode (2026-07-03): with hot_standby=on, WAL
# replay ENFORCES that connection/worker GUCs are >= the primary's
# values ("recovery aborted because of insufficient parameter
# settings") — these must MIRROR the live primary, not be downsized.
# Read them live so a future primary retune can't re-break the drill.
sudo -u postgres psql -d stellarindex -tA -F' = ' -c \
  "SELECT name, setting FROM pg_settings WHERE name IN
   ('max_connections','max_worker_processes','max_wal_senders',
    'max_prepared_transactions','max_locks_per_transaction')" \
  | sudo -u postgres tee -a "$DATA_DIR/postgresql.auto.conf" >/dev/null
if sudo -u postgres "$PG_BIN/pg_ctl" -D "$DATA_DIR" -w -t "$PG_START_TIMEOUT" start; then
  check "pg_start" 1 "scratch instance up on :$DRILL_PG_PORT (recovery complete)"
else
  check "pg_start" 0 "scratch instance failed to reach consistency"
  exit "$fail_count"
fi

q() { sudo -u postgres psql -h 127.0.0.1 -p "$DRILL_PG_PORT" -d stellarindex -tA -c "$1"; }
qlive() {
  if [[ -n "$LIVE_DSN" ]]; then psql "$LIVE_DSN" -tA -c "$1"
  else sudo -u postgres psql -d stellarindex -tA -c "$1"; fi
}

# ─── phase 3: verification ──────────────────────────────────────────
# 3a. The restored DB answers and has the core tables.
tables=$(q "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('trades','oracle_updates','ledger_ingest_log','completeness_snapshots')")
check "core_tables" "$([[ "$tables" == "4" ]] && echo 1 || echo 0)" "found $tables/4 core tables"

# 3b. Restored tip is close to the live tip (WAL archiving healthy).
restored_tip=$(q "SELECT coalesce(max(ledger_seq),0) FROM ledger_ingest_log")
live_tip=$(qlive "SELECT coalesce(max(ledger_seq),0) FROM ledger_ingest_log")
lag=$(( live_tip - restored_tip ))
# One ledger ≈ 5-6s; 5000 ledgers ≈ ~7h of WAL not yet in the repo —
# generous for a daily-diff schedule.
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
# Proves the ADR-0043 §2.2 "lake is re-derivable" claim + measures RTO.
#
# WHY DRY-RUN rather than the ADR's "scratch database": clickhouse.Open
# pins the `stellar` database (internal/storage/clickhouse/sink.go), so
# ch-backfill has no scratch-database mode to offer — the `-database`
# flag this stage used to pass never existed. The only writing
# alternative is re-deriving into the LIVE lake, and that is the wrong
# default here: r1's pool is capacity-constrained and the lake already
# carries ~3 TiB of duplicate rows (61c16ccd), so a recurring drill that
# re-inserts 100k ledgers of ReplacingMergeTree duplicates would be
# paying for its own evidence in the scarcest resource on the box.
#
# `-dry-run` fetches every galexie object and runs the full
# clickhouse.ExtractLedger decode — the entire recovery path and
# essentially all of its wall-clock — and writes nothing. Honest limit,
# recorded in the drill log: this measures FETCH+DECODE throughput, not
# the ClickHouse INSERT (which the live sink exercises continuously and
# which is not the multi-week bottleneck). The count reconcile below is
# what checks the lake actually holds the window we just proved
# derivable.
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
    # Reconcile against the live lake (ADR-0043 §2.2): the window we
    # just proved re-derivable must already be complete in ClickHouse.
    ch_rows=$(curl -sf "$CH_HTTP" --data-binary \
      "SELECT count() FROM stellar.ledgers WHERE ledger_seq BETWEEN $lo AND $hi" | tr -dc '0-9')
    check "ch_lake_window_complete" "$([[ "$ch_rows" == "$DRILL_CH_WINDOW" ]] && echo 1 || echo 0)" \
      "stellar.ledgers[$lo,$hi]: lake=${ch_rows:-?} expected=$DRILL_CH_WINDOW"
  else
    # Full output, never `| tail -5`: the reason this stage sat broken
    # for weeks is that its diagnosis was truncated away.
    check "ch_rederive" 0 "ch-backfill -dry-run exited $ch_rc — full output below"
    printf '%s\n' "$ch_out" >&2
  fi
fi

# ─── phase 5: evidence log ─────────────────────────────────────────
# The evidence IS the deliverable (ADR-0043 §3 / CS-110: "a backup that
# has never been restored is a hope"). A drill that ran but recorded
# nothing is a FAILED drill, so an unwritable evidence log fails LOUDLY
# and is counted — it does not get silently skipped as it did while
# LOG_DIR was $0-relative (BDR-03).
evidence_file="$LOG_DIR/restore-drills.md"
if mkdir -p "$LOG_DIR" 2>/dev/null && {
      echo "## $(date -u +%F) restore drill (repo${DRILL_REPO})"
      echo "- restore: ${restore_secs}s; tip lag ${lag} ledgers; hash-chain breaks: ${breaks}; trades window match: ${restored_rows}=${live_rows}"
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

# ─── phase 6: node_exporter textfile metric ────────────────────────
# Mirrors ch-schema-snapshot.sh §6: stamp last_success ONLY on a fully
# clean run (fail_count==0, evidence written) so
# stellarindex_restore_drill_stale fires when the monthly drill stops
# producing evidence. Written atomically (.tmp then mv) under /var,
# which ProtectSystem=full leaves writable.
if [[ "$TEXTFILE_DIR" != "/dev/null" ]]; then
  if mkdir -p "$TEXTFILE_DIR" 2>/dev/null; then
    metric_out="$TEXTFILE_DIR/restore_drill.prom"
    metric_tmp="$metric_out.tmp.$$"
    {
      echo "# HELP stellarindex_restore_drill_last_success_unix Unix time of the most recent fully-successful pgBackRest restore-drill (ADR-0043 §3 / CS-110)."
      echo "# TYPE stellarindex_restore_drill_last_success_unix gauge"
      if [[ "$fail_count" -eq 0 ]]; then
        echo "stellarindex_restore_drill_last_success_unix $(date +%s)"
      fi
      echo "# HELP stellarindex_restore_drill_failures Number of failed verification checks in the most recent restore-drill run."
      echo "# TYPE stellarindex_restore_drill_failures gauge"
      echo "stellarindex_restore_drill_failures $fail_count"
    } > "$metric_tmp"
    chmod 644 "$metric_tmp"
    mv "$metric_tmp" "$metric_out"
  else
    note "WARN: could not write $TEXTFILE_DIR — restore-drill metric not emitted (staleness alert will fire on the absent series)"
  fi
fi

note "done: $fail_count failure(s)"
exit "$fail_count"
