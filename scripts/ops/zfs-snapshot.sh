#!/usr/bin/env bash
# zfs-snapshot.sh — rolling ZFS snapshots of the ClickHouse lake and
# Postgres datasets on r1 (decision 2026-08-29).
#
# WHAT THIS PROTECTS AGAINST. pgBackRest (ADR-0043) covers Postgres
# off-host with PITR, and the lake is re-derivable from the Galexie
# archive — but both paths are HOURS-TO-DAYS. The failure this closes is
# the fast logical fault: a bad migration, a `DROP TABLE` / `ALTER ...
# DELETE` with the wrong predicate, a re-derive that wrote garbage. A
# dataset snapshot taken minutes or hours earlier turns that into a
# `zfs clone` + copy-a-table-back (or `zfs rollback`) instead of a
# multi-day rebuild. See docs/operations/runbooks/zfs-snapshots.md for
# the recovery procedures and the HONEST consistency semantics (a ZFS
# snapshot is crash-consistent for both engines, nothing more).
#
# WHAT IT DOES, per run (`rotate`, the timer's mode):
#
#   1. RETENTION: for every managed dataset, destroy `auto-*` snapshots
#      older than that dataset's retention window.
#   2. MIN-FREE GUARD (hard): if the pool's free space is below
#      $ZFS_SNAPSHOT_MIN_FREE_BYTES, destroy `auto-*` snapshots
#      oldest-first across all managed datasets, re-reading free after
#      each, until the floor is met or every dataset is down to its
#      NEWEST auto snapshot (the newest is never pruned by the guard —
#      it is the last line of defence and pruning it gains ~one day's
#      churn at most).
#   3. SNAPSHOT: if free is (now) at or above the floor, take
#      `<dataset>@auto-YYYYMMDD-HHMM` on each managed dataset. If it is
#      still below, SKIP — a snapshot on a starving pool pins ~250 GB/day
#      of merged ClickHouse parts that the pool cannot afford — and say
#      so via `stellarindex_zfs_snapshot_guard_skipped{dataset}=1`.
#   4. METRICS: write node_exporter textfile gauges (pool free, latest
#      snapshot time / count / bytes per dataset) so the alert rules in
#      deploy/monitoring/rules/zfs-snapshots.yml have a producer.
#
# `now <dataset>` (scripts/ops/zfs-snapshot-now.sh) runs the guard and
# takes ONE fresh auto-snapshot of that dataset — what the ClickHouse
# destructive-DDL runbook (docs/operations/clickhouse-destructive-ddl.md)
# requires before any DROP / irreversible ALTER. `--keep <label>` names
# it `manual-<label>` instead, which this script will NEVER destroy.
#
# INVARIANTS (pinned by scripts/ci/zfs-snapshot-test.sh):
#   - Only snapshots named `auto-YYYYMMDD-HHMM` are ever destroyed.
#     Anything else (`manual-*`, `pre-migration`, an operator's
#     hand-made snapshot) is invisible to retention AND to the guard.
#   - Only datasets in $ZFS_SNAPSHOT_DATASETS are touched at all.
#   - The guard never prunes a dataset's newest auto snapshot.
#   - Every `zfs` call is idempotent from the caller's view: taking an
#     already-existing snapshot name is a no-op success, destroying an
#     already-gone one is a no-op success.
#
# Configuration (environment; /etc/default/zfs-snapshot is rendered by
# the archival-node role, tag `zfs-snapshots`):
#
#   ZFS_SNAPSHOT_POOL            pool to read free space from   (data)
#   ZFS_SNAPSHOT_DATASETS        space-separated `<dataset>:<retention_days>`
#                                (data/clickhouse:3 data/postgres:7)
#   ZFS_SNAPSHOT_MIN_FREE_BYTES  guard floor in bytes           (2 TiB)
#   TEXTFILE_DIR                 node_exporter textfile dir; /dev/null
#                                disables the metric write.
#
# Logging: stderr, which is the journal under systemd. Outside systemd
# (operator shell) the same lines are also sent to the journal via
# logger(1) so a hand-run snapshot leaves the same audit trail.
#
# Usage:
#   zfs-snapshot.sh rotate
#   zfs-snapshot.sh now <dataset> [--keep <label>]
#   zfs-snapshot.sh metrics            # refresh the textfile only
#
# Exit: 0 ok (including a guard-skip in `rotate`: the skip is a metric,
#       not a crash); 1 a zfs/zpool call failed or `now` was refused by
#       the guard; 2 usage / configuration error.
set -euo pipefail

ZFS_SNAPSHOT_POOL="${ZFS_SNAPSHOT_POOL:-data}"
ZFS_SNAPSHOT_DATASETS="${ZFS_SNAPSHOT_DATASETS:-data/clickhouse:3 data/postgres:7}"
ZFS_SNAPSHOT_MIN_FREE_BYTES="${ZFS_SNAPSHOT_MIN_FREE_BYTES:-2199023255552}"  # 2 TiB
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

# The ONLY name shape this script will ever destroy. Anchored on both
# ends on purpose: `auto-20260829-0215-copy` or `xauto-...` must not match.
AUTO_RE='^auto-[0-9]{8}-[0-9]{4}$'

log() {
  printf 'zfs-snapshot: %s\n' "$*" >&2
  # Under systemd stderr already IS the journal; outside it, mirror there.
  if [[ -z "${JOURNAL_STREAM:-}" ]] && command -v logger >/dev/null 2>&1; then
    logger -t zfs-snapshot -- "$*" || true
  fi
}
die() { log "ERROR: $1"; exit "${2:-1}"; }

# ─── configuration parsing ──────────────────────────────────────────
# Parallel indexed arrays (no `declare -A`: bash 3.2 on an operator's
# macOS must still be able to run scripts/ci/zfs-snapshot-test.sh).
DATASETS=()
RETENTION_DAYS=()
GUARD_SKIPPED=()
parse_datasets() {
  local entry ds days
  for entry in $ZFS_SNAPSHOT_DATASETS; do
    ds="${entry%%:*}"
    days="${entry##*:}"
    [[ "$entry" == *:* && -n "$ds" && "$days" =~ ^[0-9]+$ && "$days" -ge 1 ]] \
      || die "bad ZFS_SNAPSHOT_DATASETS entry '$entry' (want <dataset>:<retention_days>, days >= 1)" 2
    DATASETS+=("$ds")
    RETENTION_DAYS+=("$days")
    GUARD_SKIPPED+=(0)
  done
  [[ ${#DATASETS[@]} -gt 0 ]] || die "ZFS_SNAPSHOT_DATASETS is empty" 2
  [[ "$ZFS_SNAPSHOT_MIN_FREE_BYTES" =~ ^[0-9]+$ ]] \
    || die "ZFS_SNAPSHOT_MIN_FREE_BYTES must be an integer (bytes)" 2
}

# Index of a managed dataset in DATASETS; returns 1 if not managed.
dataset_index() {
  local ds="$1" i
  for i in "${!DATASETS[@]}"; do
    if [[ "${DATASETS[$i]}" == "$ds" ]]; then
      printf '%s' "$i"
      return 0
    fi
  done
  return 1
}
is_managed() { dataset_index "$1" >/dev/null; }
retention_days() {
  local i
  i="$(dataset_index "$1")" || die "$1 is not a managed dataset"
  printf '%s' "${RETENTION_DAYS[$i]}"
}

# ─── zfs wrappers (every zfs/zpool call goes through here) ──────────
# Sets the global POOL_FREE. Deliberately NOT `free="$(...)"`: a die()
# inside a command substitution only exits the subshell, and in condition
# context (`if enforce_min_free`, `... || die`) set -e is inert — so an
# unreadable pool would have left free="" (= 0 < floor) and the guard
# would have destroyed every prunable snapshot with rc=0 (verifier
# finding on 69204ee). Called in the parent shell, die() aborts the run:
# no destroy, no snapshot, non-zero exit, error textfile.
POOL_FREE=""
read_pool_free() {
  local v
  POOL_FREE=""
  if ! v="$(zpool list -Hp -o free "$ZFS_SNAPSHOT_POOL")"; then
    write_error_metric
    die "zpool list $ZFS_SNAPSHOT_POOL failed — refusing to prune or snapshot on unknown free space"
  fi
  if [[ -z "$v" || ! "$v" =~ ^[0-9]+$ ]]; then
    write_error_metric
    die "zpool free for $ZFS_SNAPSHOT_POOL is not a number: '$v' — refusing to prune or snapshot"
  fi
  POOL_FREE="$v"
}

# Written INSTEAD of the main textfile when free space is unreadable, so
# the last good gauges stay visible (and go stale → the stale alert) and
# the failure has its own series. Removed again by a successful run.
write_error_metric() {
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return 0
  local out tmp
  mkdir -p "$TEXTFILE_DIR"
  out="$TEXTFILE_DIR/zfs_snapshot_error.prom"
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_zfs_snapshot_pool_free_unreadable 1 while the last snapshot run could not read zpool free space and therefore refused to prune or snapshot."
    echo "# TYPE stellarindex_zfs_snapshot_pool_free_unreadable gauge"
    echo "stellarindex_zfs_snapshot_pool_free_unreadable{pool=\"$ZFS_SNAPSHOT_POOL\"} 1"
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
}

dataset_exists() { zfs list -H -o name "$1" >/dev/null 2>&1; }
snapshot_exists() { zfs list -H -o name -t snapshot "$1" >/dev/null 2>&1; }

# Direct snapshots of one dataset, oldest first:
# "<dataset>@<snap>\t<creation_unix>\t<used_bytes>"
list_snapshots() {
  zfs list -Hp -t snapshot -d 1 -o name,creation,used -s creation "$1"
}

# Same, restricted to the auto-* population this script owns:
# "<dataset>@<snap>\t<creation_unix>"
list_auto_snapshots() {
  local name created snap
  while IFS=$'\t' read -r name created _; do
    [[ -z "$name" ]] && continue
    snap="${name#*@}"
    [[ "$snap" =~ $AUTO_RE ]] || continue
    printf '%s\t%s\n' "$name" "$created"
  done < <(list_snapshots "$1")
}

# The single destroy path. Refuses anything that is not
# <managed-dataset>@auto-YYYYMMDD-HHMM — this is the invariant, not a
# convenience check, so it lives at the choke point rather than at each
# caller.
destroy_auto_snapshot() {
  local full="$1" why="$2" ds snap
  ds="${full%%@*}"
  snap="${full#*@}"
  [[ "$full" == *@* ]] || die "refusing to destroy '$full': not a snapshot name"
  is_managed "$ds" || die "refusing to destroy '$full': $ds is not a managed dataset"
  [[ "$snap" =~ $AUTO_RE ]] || die "refusing to destroy '$full': not an auto-* snapshot"
  if ! snapshot_exists "$full"; then
    log "destroy $full: already gone (no-op)"
    return 0
  fi
  zfs destroy "$full" || die "zfs destroy $full failed"
  log "destroyed $full ($why)"
}

take_snapshot() {
  local full="$1"
  if snapshot_exists "$full"; then
    log "snapshot $full already exists (no-op)"
    return 0
  fi
  zfs snapshot "$full" || die "zfs snapshot $full failed"
  log "created $full"
}

# ─── the three phases ───────────────────────────────────────────────
prune_expired() {
  local ds now cutoff name created days
  now="$(date +%s)"
  for ds in "${DATASETS[@]}"; do
    days="$(retention_days "$ds")"
    cutoff=$(( now - days * 86400 ))
    while IFS=$'\t' read -r name created; do
      [[ -z "$name" ]] && continue
      if (( created < cutoff )); then
        destroy_auto_snapshot "$name" "retention ${days}d"
      fi
    done < <(list_auto_snapshots "$ds")
  done
}

# Returns 0 if free >= floor (possibly after pruning), 1 if still below.
enforce_min_free() {
  local free ds name created candidates
  read_pool_free; free="$POOL_FREE"
  [[ -n "$free" && "$free" =~ ^[0-9]+$ ]] || die "internal: pool free unreadable ('$free')"
  if (( free >= ZFS_SNAPSHOT_MIN_FREE_BYTES )); then
    return 0
  fi
  log "pool $ZFS_SNAPSHOT_POOL free ${free} < floor ${ZFS_SNAPSHOT_MIN_FREE_BYTES}: pruning auto snapshots oldest-first"
  while (( free < ZFS_SNAPSHOT_MIN_FREE_BYTES )); do
    # Prunable = every auto snapshot except each dataset's newest one
    # (`sed '$d'` drops the last, i.e. newest, line of the oldest-first list).
    candidates=""
    for ds in "${DATASETS[@]}"; do
      candidates+="$(list_auto_snapshots "$ds" | sed '$d')"$'\n'
    done
    # Oldest across datasets first (creation is column 2).
    # $candidates is every prunable auto snapshot across all datasets —
    # unbounded on a long-lived pool, so `| head` could EPIPE `sort` under
    # pipefail and abort the prune loop exactly when the pool is filling
    # (#475). Sort to completion in a substitution, then slice in the shell.
    sorted_candidates="$(printf '%s' "$candidates" | awk -F'\t' 'NF==2' | sort -t $'\t' -k2,2n)"
    name="${sorted_candidates%%$'\n'*}"
    name="${name%%$'\t'*}"
    if [[ -z "$name" ]]; then
      log "guard: nothing left to prune (each dataset is down to its newest auto snapshot); free ${free} still < ${ZFS_SNAPSHOT_MIN_FREE_BYTES}"
      return 1
    fi
    created="$(printf '%s' "$candidates" | awk -F'\t' -v n="$name" '$1==n{print $2; exit}')"
    destroy_auto_snapshot "$name" "min-free guard, created $created"
    # ZFS reclaims space asynchronously; a re-read right after destroy
    # can lag by a txg. Worst case this over-prunes by one snapshot,
    # never below the newest-per-dataset floor.
    read_pool_free; free="$POOL_FREE"
    [[ -n "$free" && "$free" =~ ^[0-9]+$ ]] || die "internal: pool free unreadable ('$free')"
  done
  log "guard: pool free ${free} >= floor after pruning"
  return 0
}

snapshot_datasets() {
  local ds stamp i
  stamp="auto-$(date -u +%Y%m%d-%H%M)"
  for ds in "$@"; do
    i="$(dataset_index "$ds")" || die "$ds is not a managed dataset"
    dataset_exists "$ds" || die "dataset $ds does not exist"
    if enforce_min_free; then
      GUARD_SKIPPED[i]=0
      take_snapshot "${ds}@${stamp}"
    else
      GUARD_SKIPPED[i]=1
      log "SKIPPED snapshot of $ds: pool free below ZFS_SNAPSHOT_MIN_FREE_BYTES after pruning"
    fi
  done
}

write_metrics() {
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return 0
  local out tmp free ds latest count used name created i
  mkdir -p "$TEXTFILE_DIR"
  out="$TEXTFILE_DIR/zfs_snapshot.prom"
  tmp="$out.tmp.$$"
  read_pool_free; free="$POOL_FREE"
  {
    echo "# HELP stellarindex_zfs_pool_free_bytes Free bytes in the ZFS pool as reported by zpool list (pool-level, unlike node_filesystem_avail_bytes)."
    echo "# TYPE stellarindex_zfs_pool_free_bytes gauge"
    echo "stellarindex_zfs_pool_free_bytes{pool=\"$ZFS_SNAPSHOT_POOL\"} $free"
    echo "# HELP stellarindex_zfs_snapshot_min_free_bytes The guard floor: below this the snapshot job prunes, then skips."
    echo "# TYPE stellarindex_zfs_snapshot_min_free_bytes gauge"
    echo "stellarindex_zfs_snapshot_min_free_bytes{pool=\"$ZFS_SNAPSHOT_POOL\"} $ZFS_SNAPSHOT_MIN_FREE_BYTES"
    echo "# HELP stellarindex_zfs_snapshot_latest_unix Creation time of the newest auto-* snapshot per dataset (0 = none)."
    echo "# TYPE stellarindex_zfs_snapshot_latest_unix gauge"
    for ds in "${DATASETS[@]}"; do
      latest=0
      while IFS=$'\t' read -r name created; do
        [[ -n "$name" ]] && latest="$created"
      done < <(list_auto_snapshots "$ds")
      echo "stellarindex_zfs_snapshot_latest_unix{dataset=\"$ds\"} $latest"
    done
    echo "# HELP stellarindex_zfs_snapshot_count Number of auto-* snapshots currently held per dataset."
    echo "# TYPE stellarindex_zfs_snapshot_count gauge"
    for ds in "${DATASETS[@]}"; do
      count="$(list_auto_snapshots "$ds" | grep -c . || true)"
      echo "stellarindex_zfs_snapshot_count{dataset=\"$ds\"} $count"
    done
    echo "# HELP stellarindex_zfs_snapshot_used_bytes Bytes held exclusively by snapshots of the dataset (zfs usedbysnapshots — all snapshots, not only auto-*)."
    echo "# TYPE stellarindex_zfs_snapshot_used_bytes gauge"
    for ds in "${DATASETS[@]}"; do
      used="$(zfs get -Hp -o value usedbysnapshots "$ds")" || die "zfs get usedbysnapshots $ds failed"
      echo "stellarindex_zfs_snapshot_used_bytes{dataset=\"$ds\"} $used"
    done
    echo "# HELP stellarindex_zfs_snapshot_guard_skipped 1 if the last run skipped this dataset's snapshot because the pool was below the min-free floor even after pruning."
    echo "# TYPE stellarindex_zfs_snapshot_guard_skipped gauge"
    for i in "${!DATASETS[@]}"; do
      ds="${DATASETS[$i]}"
      echo "stellarindex_zfs_snapshot_guard_skipped{dataset=\"$ds\"} ${GUARD_SKIPPED[$i]}"
    done
    echo "# HELP stellarindex_zfs_snapshot_last_run_unix Unix time the snapshot job last completed (any mode)."
    echo "# TYPE stellarindex_zfs_snapshot_last_run_unix gauge"
    echo "stellarindex_zfs_snapshot_last_run_unix $(date +%s)"
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
  rm -f "$TEXTFILE_DIR/zfs_snapshot_error.prom"
}

# ─── entrypoints ────────────────────────────────────────────────────
usage() {
  cat >&2 <<EOF
usage: $0 rotate
       $0 now <dataset> [--keep <label>]
       $0 metrics
EOF
  exit 2
}

main() {
  local mode="${1:-}" ds label i
  parse_datasets
  case "$mode" in
    rotate)
      log "rotate: datasets=${DATASETS[*]} floor=${ZFS_SNAPSHOT_MIN_FREE_BYTES}"
      prune_expired
      snapshot_datasets "${DATASETS[@]}"
      write_metrics
      ;;
    now)
      ds="${2:-}"
      [[ -n "$ds" ]] || usage
      i="$(dataset_index "$ds")" || die "$ds is not in ZFS_SNAPSHOT_DATASETS (${DATASETS[*]})" 2
      label=""
      if [[ "${3:-}" == "--keep" ]]; then
        label="${4:-}"
        [[ "$label" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "--keep label must be [A-Za-z0-9_.-]+" 2
      elif [[ -n "${3:-}" ]]; then
        usage
      fi
      if [[ -n "$label" ]]; then
        dataset_exists "$ds" || die "dataset $ds does not exist"
        # A kept snapshot is exempt from retention/guard pruning, so the
        # guard is still applied before taking it: pinning churn on a
        # starving pool is the failure the guard exists to prevent.
        enforce_min_free || die "pool below ZFS_SNAPSHOT_MIN_FREE_BYTES even after pruning; free space first"
        take_snapshot "${ds}@manual-${label}"
        log "manual-${label} is exempt from auto-pruning: destroy it by hand when done"
      else
        snapshot_datasets "$ds"
        if [[ "${GUARD_SKIPPED[$i]}" == "1" ]]; then
          write_metrics
          die "snapshot of $ds refused by the min-free guard"
        fi
      fi
      write_metrics
      ;;
    metrics)
      write_metrics
      ;;
    *)
      usage
      ;;
  esac
}

# Sourced by scripts/ci/zfs-snapshot-test.sh to pin the destroy choke
# point directly; executed everywhere else.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
