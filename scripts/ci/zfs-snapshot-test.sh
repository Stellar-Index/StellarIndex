#!/usr/bin/env bash
# zfs-snapshot-test.sh — fixture tests for scripts/ops/zfs-snapshot.sh
# (rolling ZFS snapshots of data/clickhouse + data/postgres, 2026-08-29).
#
# `zfs` and `zpool` are STUBBED on PATH with a file-backed fake pool, so
# this runs on any box (macOS included) and never touches a real pool.
# The properties pinned here are the ones a bug would turn into data
# loss or a silently-starving pool:
#
#   1. retention prunes auto-* snapshots older than the per-dataset
#      window (3 d clickhouse / 7 d postgres) and NOTHING newer;
#   2. the min-free guard prunes oldest-first across datasets, stops as
#      soon as the floor is met, and never takes a dataset's NEWEST
#      auto snapshot;
#   3. with the floor unreachable the run SKIPS the snapshot, exits 0,
#      and says so via stellarindex_zfs_snapshot_guard_skipped=1;
#   4. non-auto snapshots (`manual-*`, an operator's hand-made name, an
#      `auto-...-copy` look-alike) are NEVER destroyed — by retention,
#      by the guard, in any mode;
#   5. datasets outside ZFS_SNAPSHOT_DATASETS are never touched;
#   6. `now <ds>` takes one auto snapshot; `--keep <label>` takes a
#      manual-* one that a later rotate leaves alone;
#   7. the textfile carries the alert-facing gauges with the correct
#      values.
#
# Run: bash scripts/ci/zfs-snapshot-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/zfs-snapshot.sh"
[[ -x "$SCRIPT" ]] || { echo "zfs-snapshot-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
# res <rc> <what> [<detail shown on failure>]
res() { if [[ "$1" -eq 0 ]]; then ok "$2"; else bad "$2 — ${3:-}"; fi; }
# t <test(1) args...> — a plain `test` behind a function so `res $?` reads a
# command status (SC2319 objects to `$?` straight after `[[ ]]`).
t() { test "$@"; }

# ─── fake pool ──────────────────────────────────────────────────────
# State files:
#   $POOL/datasets    one dataset name per line
#   $POOL/snapshots   "<ds>@<snap>\t<creation_unix>\t<used>" per line
#   $POOL/free        pool free bytes; each destroy adds the snapshot's
#                     `used` back (the guard's re-read sees the reclaim,
#                     like ZFS after the next txg)
#   $POOL/destroyed   append-only log of every destroy
#   $POOL/created     append-only log of every snapshot create
export FAKE_POOL="$TMP/pool"
mkdir -p "$FAKE_POOL" "$TMP/bin"

cat > "$TMP/bin/zfs" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
P="$FAKE_POOL"
cmd="${1:-}"; shift || true
has_flag() { local f; for f in "$@"; do [[ "$f" == "$WANT" ]] && return 0; done; return 1; }
case "$cmd" in
  list)
    # Forms the script uses:
    #   zfs list -H -o name <dataset>                   (dataset exists?)
    #   zfs list -H -o name -t snapshot <ds@snap>       (snapshot exists?)
    #   zfs list -Hp -t snapshot -d 1 -o name,creation,used -s creation <ds>
    target="${*: -1}"
    WANT=snapshot
    if has_flag "$@"; then
      if [[ "$target" == *@* ]]; then
        grep -q "^${target}"$'\t' "$P/snapshots" 2>/dev/null || { echo "cannot open '$target': dataset does not exist" >&2; exit 1; }
        echo "$target"; exit 0
      fi
      grep -qx "$target" "$P/datasets" || { echo "cannot open '$target': dataset does not exist" >&2; exit 1; }
      grep "^${target}@" "$P/snapshots" 2>/dev/null | sort -t $'\t' -k2,2n || true
      exit 0
    fi
    grep -qx "$target" "$P/datasets" || { echo "cannot open '$target': dataset does not exist" >&2; exit 1; }
    echo "$target"; exit 0 ;;
  snapshot)
    full="$1"; ds="${full%%@*}"
    grep -qx "$ds" "$P/datasets" || { echo "cannot create snapshot '$full': no such dataset" >&2; exit 1; }
    grep -q "^${full}"$'\t' "$P/snapshots" 2>/dev/null && { echo "cannot create snapshot '$full': dataset already exists" >&2; exit 1; }
    printf '%s\t%s\t%s\n' "$full" "$(date +%s)" 0 >> "$P/snapshots"
    echo "$full" >> "$P/created"; exit 0 ;;
  destroy)
    full="$1"
    line="$(grep "^${full}"$'\t' "$P/snapshots" 2>/dev/null || true)"
    [[ -n "$line" ]] || { echo "could not find any snapshots to destroy; check snapshot names." >&2; exit 1; }
    used="$(printf '%s' "$line" | cut -f3)"
    grep -v "^${full}"$'\t' "$P/snapshots" > "$P/snapshots.new" || true
    mv "$P/snapshots.new" "$P/snapshots"
    echo $(( $(cat "$P/free") + used )) > "$P/free"
    echo "$full" >> "$P/destroyed"; exit 0 ;;
  get)
    # zfs get -Hp -o value usedbysnapshots <ds>
    ds="${*: -1}"
    grep "^${ds}@" "$P/snapshots" 2>/dev/null | awk -F'\t' '{s+=$3} END{print s+0}'; exit 0 ;;
  *) echo "zfs stub: unsupported: $cmd $*" >&2; exit 99 ;;
esac
STUB
cat > "$TMP/bin/zpool" <<'STUB'
#!/usr/bin/env bash
# zpool list -Hp -o free <pool>
[[ "${*: -1}" == "data" ]] || { echo "cannot open '${*: -1}': no such pool" >&2; exit 1; }
cat "$FAKE_POOL/free"
STUB
# `logger` must not reach the real journal from a test run.
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/logger"
chmod +x "$TMP/bin/zfs" "$TMP/bin/zpool" "$TMP/bin/logger"
export PATH="$TMP/bin:$PATH"

export TEXTFILE_DIR="$TMP/textfile"
export ZFS_SNAPSHOT_POOL=data
export ZFS_SNAPSHOT_DATASETS="data/clickhouse:3 data/postgres:7"

NOW="$(date +%s)"
GB=1073741824
TIB=1099511627776
# Fixture ages sit ONE HOUR inside their day boundary so a snapshot "3 d
# old" stays inside a 3 d window for the whole test run (a second passing
# between fixture creation and the cutoff must not flip a case).
days_ago() { echo $(( NOW - $1 * 86400 + 3600 )); }
stamp_days_ago() {
  # BSD date (macOS) takes -r <epoch>; GNU date takes -d @<epoch>.
  date -u -r "$(days_ago "$1")" +%Y%m%d-%H%M 2>/dev/null || date -u -d "@$(days_ago "$1")" +%Y%m%d-%H%M
}
add_snap() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$FAKE_POOL/snapshots"; }

# Baseline fixture: 5 days of auto snapshots on both datasets, plus the
# names that must survive everything.
reset_pool() {
  local free="$1" d
  printf '%s\n' data/clickhouse data/postgres data/minio > "$FAKE_POOL/datasets"
  : > "$FAKE_POOL/snapshots"; : > "$FAKE_POOL/destroyed"; : > "$FAKE_POOL/created"
  echo "$free" > "$FAKE_POOL/free"
  for d in 5 4 3 2 1; do
    add_snap "data/clickhouse@auto-$(stamp_days_ago "$d")" "$(days_ago "$d")" $(( 250 * GB ))
    add_snap "data/postgres@auto-$(stamp_days_ago "$d")"   "$(days_ago "$d")" $(( 2 * GB ))
  done
  # Older postgres history (inside its 7 d window at 6 d, outside at 9 d).
  add_snap "data/postgres@auto-$(stamp_days_ago 6)" "$(days_ago 6)" $(( 2 * GB ))
  add_snap "data/postgres@auto-$(stamp_days_ago 9)" "$(days_ago 9)" $(( 2 * GB ))
  # Never-touch population: manual, operator-named, look-alike, unmanaged dataset.
  add_snap "data/clickhouse@manual-pre-ddl"               "$(days_ago 30)" $(( 500 * GB ))
  add_snap "data/postgres@pre-migration-0120"             "$(days_ago 40)" $(( 5 * GB ))
  add_snap "data/clickhouse@auto-$(stamp_days_ago 20)-copy" "$(days_ago 20)" $(( 100 * GB ))
  add_snap "data/minio@auto-$(stamp_days_ago 30)"         "$(days_ago 30)" $(( 1 * GB ))
}
protected_intact() {
  grep -q '^data/clickhouse@manual-pre-ddl' "$FAKE_POOL/snapshots" \
    && grep -q '^data/postgres@pre-migration-0120' "$FAKE_POOL/snapshots" \
    && grep -q -- '-copy'$'\t' "$FAKE_POOL/snapshots" \
    && grep -q '^data/minio@auto-' "$FAKE_POOL/snapshots" \
    && ! grep -q -E 'manual-|pre-migration|-copy|data/minio' "$FAKE_POOL/destroyed"
}
metric() { grep -E "^$1 " "$TEXTFILE_DIR/zfs_snapshot.prom" 2>/dev/null | awk '{print $2}'; }
n_destroyed() { grep -c . "$FAKE_POOL/destroyed" || true; }
n_created()   { grep -c . "$FAKE_POOL/created" || true; }
destroyed_list() { tr '\n' ' ' < "$FAKE_POOL/destroyed"; }
created_list()   { tr '\n' ' ' < "$FAKE_POOL/created"; }
was_destroyed() { grep -qx "$1" "$FAKE_POOL/destroyed"; }

echo "zfs-snapshot-test: rotate with ample free space (retention only)"
reset_pool $(( 5 * TIB ))
"$SCRIPT" rotate 2>"$TMP/log1"; rc=$?
res $(( rc != 0 )) "rotate exits 0" "exit $rc: $(cat "$TMP/log1")"
# clickhouse 3 d window: 5d + 4d expire; 3d (71 h old) survives.
was_destroyed "data/clickhouse@auto-$(stamp_days_ago 5)" \
  && was_destroyed "data/clickhouse@auto-$(stamp_days_ago 4)" \
  && ! was_destroyed "data/clickhouse@auto-$(stamp_days_ago 3)" \
  && ! was_destroyed "data/clickhouse@auto-$(stamp_days_ago 2)"
res $? "clickhouse retention 3d: pruned 5d+4d, kept 3d..1d" "destroyed=[$(destroyed_list)]"
was_destroyed "data/postgres@auto-$(stamp_days_ago 9)" \
  && ! was_destroyed "data/postgres@auto-$(stamp_days_ago 6)" \
  && [[ "$(grep -c '^data/postgres@' "$FAKE_POOL/destroyed")" -eq 1 ]]
res $? "postgres retention 7d: pruned only the 9d snapshot" "destroyed=[$(destroyed_list)]"
t "$(grep -c '^data/clickhouse@auto-' "$FAKE_POOL/created")" -eq 1 -a "$(grep -c '^data/postgres@auto-' "$FAKE_POOL/created")" -eq 1
res $? "one new auto snapshot per managed dataset" "created=[$(created_list)]"
grep -qE '^data/(clickhouse|postgres)@auto-[0-9]{8}-[0-9]{4}$' "$FAKE_POOL/created"
res $? "snapshot name is auto-YYYYMMDD-HHMM" "created=[$(created_list)]"
protected_intact
res $? "manual-/operator/look-alike/unmanaged snapshots untouched" "destroyed=[$(destroyed_list)]"
t "$(metric 'stellarindex_zfs_pool_free_bytes{pool="data"}')" = "$(cat "$FAKE_POOL/free")"
res $? "pool_free_bytes gauge matches zpool" "got $(metric 'stellarindex_zfs_pool_free_bytes{pool="data"}')"
latest="$(metric 'stellarindex_zfs_snapshot_latest_unix{dataset="data/clickhouse"}')"
t -n "$latest" -a "${latest:-0}" -ge "$NOW"
res $? "snapshot_latest_unix{clickhouse} is the fresh snapshot" "latest=$latest now=$NOW"
t "$(metric 'stellarindex_zfs_snapshot_count{dataset="data/clickhouse"}')" = 4
res $? "snapshot_count{clickhouse}=4 (3 kept + 1 new; manual/look-alike excluded)" "got $(metric 'stellarindex_zfs_snapshot_count{dataset="data/clickhouse"}')"
t "$(metric 'stellarindex_zfs_snapshot_count{dataset="data/postgres"}')" = 7
res $? "snapshot_count{postgres}=7 (6 kept + 1 new)" "got $(metric 'stellarindex_zfs_snapshot_count{dataset="data/postgres"}')"
# usedbysnapshots covers ALL snapshots: 3×250 GB auto + 500 GB manual + 100 GB look-alike + 0 new.
t "$(metric 'stellarindex_zfs_snapshot_used_bytes{dataset="data/clickhouse"}')" = $(( (750 + 500 + 100) * GB ))
res $? "snapshot_used_bytes{clickhouse} = zfs usedbysnapshots" "got $(metric 'stellarindex_zfs_snapshot_used_bytes{dataset="data/clickhouse"}')"
t "$(metric 'stellarindex_zfs_snapshot_guard_skipped{dataset="data/clickhouse"}')" = 0
res $? "guard_skipped=0 when the floor is met"

echo "zfs-snapshot-test: min-free guard prunes oldest-first, just enough"
# Free 1.6 TiB, floor 2 TiB: retention alone frees ch 5d+4d (500 GB) +
# pg 9d (2 GB) → ~2.09 TiB ≥ floor, so the guard must prune NOTHING more.
reset_pool $(( 16 * TIB / 10 ))
"$SCRIPT" rotate 2>"$TMP/log2"; rc=$?
res $(( rc != 0 )) "rotate exits 0" "exit $rc: $(cat "$TMP/log2")"
t "$(n_destroyed)" -eq 3
res $? "retention alone met the floor; guard pruned nothing extra" "destroyed=[$(destroyed_list)]"
t "$(metric 'stellarindex_zfs_snapshot_guard_skipped{dataset="data/clickhouse"}')" = 0
res $? "not skipped"

# Free 1.0 TiB: retention frees 502 GB → 1.49 TiB; the guard then prunes
# strictly oldest-first across datasets — postgres 6d, 5d, 4d (2 GB each,
# the age order is the contract, not the byte yield), then clickhouse 3d
# / postgres 3d, clickhouse 2d / postgres 2d (→ ~1.98 TiB) — still short,
# so it must STOP at the newest-per-dataset floor: clickhouse@1d +
# postgres@1d survive, the snapshot is SKIPPED, exit 0, guard_skipped=1.
reset_pool $(( 1 * TIB ))
"$SCRIPT" rotate 2>"$TMP/log3"; rc=$?
res $(( rc != 0 )) "guard-skip run still exits 0" "exit $rc: $(cat "$TMP/log3")"
grep -q "^data/clickhouse@auto-$(stamp_days_ago 1)"$'\t' "$FAKE_POOL/snapshots" \
  && grep -q "^data/postgres@auto-$(stamp_days_ago 1)"$'\t' "$FAKE_POOL/snapshots"
res $? "guard never pruned a dataset's NEWEST auto snapshot" "destroyed=[$(destroyed_list)]"
t ! -s "$FAKE_POOL/created"
res $? "no snapshot taken while below the floor" "created=[$(created_list)]"
t "$(metric 'stellarindex_zfs_snapshot_guard_skipped{dataset="data/clickhouse"}')" = 1 \
  -a "$(metric 'stellarindex_zfs_snapshot_guard_skipped{dataset="data/postgres"}')" = 1
res $? "guard_skipped=1 emitted for both datasets"
grep -q "SKIPPED snapshot of data/clickhouse" "$TMP/log3"
res $? "skip is logged"
protected_intact
res $? "guard left manual-/operator/look-alike/unmanaged snapshots alone" "destroyed=[$(destroyed_list)]"
t "$(sed -n 4p "$FAKE_POOL/destroyed")" = "data/postgres@auto-$(stamp_days_ago 6)" \
  -a "$(sed -n 5p "$FAKE_POOL/destroyed")" = "data/postgres@auto-$(stamp_days_ago 5)"
res $? "guard prunes oldest-first ACROSS datasets (4th/5th destroy = postgres@6d/5d, not the bigger clickhouse@3d)" "4th/5th were $(sed -n 4,5p "$FAKE_POOL/destroyed" | tr '\n' ' ')"
ch3="$(grep -n "^data/clickhouse@auto-$(stamp_days_ago 3)$" "$FAKE_POOL/destroyed" | cut -d: -f1)"
ch2="$(grep -n "^data/clickhouse@auto-$(stamp_days_ago 2)$" "$FAKE_POOL/destroyed" | cut -d: -f1)"
t -n "$ch3" -a -n "$ch2" -a "${ch3:-0}" -lt "${ch2:-0}"
res $? "within a dataset the guard prunes older before newer (clickhouse@3d before @2d)" "ch3=$ch3 ch2=$ch2"

# Free 1.9 TiB, no retention candidates: the guard needs 100 GiB, must
# destroy exactly ONE 250 GB clickhouse snapshot, stop, then snapshot.
printf '%s\n' data/clickhouse data/postgres > "$FAKE_POOL/datasets"
: > "$FAKE_POOL/snapshots"; : > "$FAKE_POOL/destroyed"; : > "$FAKE_POOL/created"
echo $(( 19 * TIB / 10 )) > "$FAKE_POOL/free"
for d in 2 1; do
  add_snap "data/clickhouse@auto-$(stamp_days_ago "$d")" "$(days_ago "$d")" $(( 250 * GB ))
  add_snap "data/postgres@auto-$(stamp_days_ago "$d")"   "$(days_ago "$d")" $(( 2 * GB ))
done
"$SCRIPT" rotate 2>"$TMP/log4"; rc=$?
t "$rc" -eq 0 -a "$(n_destroyed)" -eq 1 -a "$(cat "$FAKE_POOL/destroyed")" = "data/clickhouse@auto-$(stamp_days_ago 2)"
res $? "guard stops as soon as the floor is met (1 destroy, not 2)" "rc=$rc destroyed=[$(destroyed_list)]"
t "$(n_created)" -eq 2
res $? "snapshots taken after the guard freed space" "created=[$(created_list)]"

echo "zfs-snapshot-test: now / --keep / unmanaged"
reset_pool $(( 5 * TIB ))
"$SCRIPT" now data/clickhouse 2>"$TMP/log5"; rc=$?
[[ $rc -eq 0 && "$(n_created)" -eq 1 ]] && grep -qE '^data/clickhouse@auto-[0-9]{8}-[0-9]{4}$' "$FAKE_POOL/created"
res $? "now <ds> takes exactly one auto snapshot of that dataset" "rc=$rc created=[$(created_list)]"
"$SCRIPT" now data/clickhouse 2>>"$TMP/log5"; rc=$?
t "$rc" -eq 0 -a "$(n_created)" -eq 1
res $? "now twice in the same minute is an idempotent no-op" "rc=$rc created=[$(created_list)]"
"$SCRIPT" now data/postgres --keep pre-0123 2>>"$TMP/log5"; rc=$?
[[ $rc -eq 0 ]] && grep -qx 'data/postgres@manual-pre-0123' "$FAKE_POOL/created"
res $? "now --keep takes manual-<label>" "rc=$rc created=[$(created_list)]"
# Age it 30 d and rotate under pressure: it must survive retention AND the guard.
awk -F'\t' -v OFS='\t' -v c="$(days_ago 30)" '$1=="data/postgres@manual-pre-0123"{$2=c} 1' "$FAKE_POOL/snapshots" > "$FAKE_POOL/s.new"
mv "$FAKE_POOL/s.new" "$FAKE_POOL/snapshots"
echo $(( 1 * TIB )) > "$FAKE_POOL/free"
"$SCRIPT" rotate 2>>"$TMP/log5"
grep -q '^data/postgres@manual-pre-0123' "$FAKE_POOL/snapshots" && ! grep -q 'manual-pre-0123' "$FAKE_POOL/destroyed"
res $? "manual-* survives retention and a starving-pool guard" "destroyed=[$(destroyed_list)]"
"$SCRIPT" now data/minio 2>"$TMP/log6"; rc=$?
[[ $rc -eq 2 ]] && ! grep -q 'data/minio' "$FAKE_POOL/created"
res $? "now on an unmanaged dataset is refused (exit 2)" "rc=$rc"
reset_pool $(( 1 * TIB ))
# Drop every prunable (exact auto-*) snapshot so the guard cannot reach
# the floor; the `-copy` look-alike stays, and must still not be touched.
grep -Ev '^data/(clickhouse|postgres)@auto-[0-9]{8}-[0-9]{4}'$'\t' "$FAKE_POOL/snapshots" > "$FAKE_POOL/s.new"; mv "$FAKE_POOL/s.new" "$FAKE_POOL/snapshots"
"$SCRIPT" now data/clickhouse 2>"$TMP/log7"; rc=$?
t "$rc" -eq 1 -a ! -s "$FAKE_POOL/created"
res $? "now below an unreachable floor refuses with exit 1 (no snapshot)" "rc=$rc created=[$(created_list)]"
t "$(metric 'stellarindex_zfs_snapshot_guard_skipped{dataset="data/clickhouse"}')" = 1
res $? "refused now still emits guard_skipped=1"
protected_intact
res $? "protected snapshots intact through the now/--keep block" "destroyed=[$(destroyed_list)]"

echo "zfs-snapshot-test: config validation"
ZFS_SNAPSHOT_DATASETS="data/clickhouse" "$SCRIPT" rotate 2>/dev/null; rc=$?
t "$rc" -eq 2
res $? "dataset entry without :days is rejected (exit 2)" "rc=$rc"
ZFS_SNAPSHOT_MIN_FREE_BYTES="2TiB" "$SCRIPT" rotate 2>/dev/null; rc=$?
t "$rc" -eq 2
res $? "non-integer floor is rejected (exit 2)" "rc=$rc"

echo "zfs-snapshot-test: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
