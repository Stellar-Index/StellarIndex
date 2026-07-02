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
# Exit code: number of failed verification checks.
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
# Scratch-instance overrides: the restored postgresql.conf carries
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
max_connections = 20
max_locks_per_transaction = 4096
max_parallel_workers = 2
timescaledb.max_background_workers = 2
hot_standby = on
CONF
if sudo -u postgres "$PG_BIN/pg_ctl" -D "$DATA_DIR" -w -t 600 start; then
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
# Proves the ADR-0043 "lake is re-derivable" claim + measures RTO.
if [[ -n "${DRILL_CH_WINDOW:-}" ]]; then
  note "CH re-derive drill: window=$DRILL_CH_WINDOW ledgers (see ADR-0043 §2.2)"
  ch_started=$(date +%s)
  lo=$(( restored_tip - 1000000 )); hi=$(( lo + DRILL_CH_WINDOW - 1 ))
  if /usr/local/bin/stellarindex-ops ch-backfill -config /etc/stellarindex.toml \
       -from "$lo" -to "$hi" -database "drill_scratch" 2>&1 | tail -5; then
    ch_secs=$(( $(date +%s) - ch_started ))
    per_ledger=$(echo "scale=4; $ch_secs / $DRILL_CH_WINDOW" | bc)
    full_days=$(echo "scale=1; $per_ledger * $live_tip / 86400" | bc)
    check "ch_rederive" 1 "window in ${ch_secs}s (${per_ledger}s/ledger → full rebuild ≈ ${full_days} days single-threaded — parallelism divides this)"
    curl -s 'http://127.0.0.1:8123/' --data-binary "DROP DATABASE IF EXISTS drill_scratch" >/dev/null || true
  else
    check "ch_rederive" 0 "ch-backfill sample failed"
  fi
fi

# ─── phase 5: evidence log ─────────────────────────────────────────
LOG_DIR="$(cd "$(dirname "$0")/../.." && pwd)/docs/operations/drills"
if [[ -d "$LOG_DIR" ]]; then
  {
    echo "## $(date -u +%F) restore drill (repo${DRILL_REPO})"
    echo "- restore: ${restore_secs}s; tip lag ${lag} ledgers; hash-chain breaks: ${breaks}; trades window match: ${restored_rows}=${live_rows}"
    [[ -n "${ch_secs:-}" ]] && echo "- CH re-derive: ${DRILL_CH_WINDOW} ledgers in ${ch_secs}s"
    [[ -n "$DRILL_LOG_NOTE" ]] && echo "- note: $DRILL_LOG_NOTE"
    echo "- failures: $fail_count"
    echo
  } >> "$LOG_DIR/restore-drills.md"
  note "evidence appended to docs/operations/drills/restore-drills.md — commit it"
fi

note "done: $fail_count failure(s)"
exit "$fail_count"
