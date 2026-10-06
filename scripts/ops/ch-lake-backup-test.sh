#!/usr/bin/env bash
# ch-lake-backup-test.sh — fixture tests for the ClickHouse lake's data
# backup (scripts/ops/ch-lake-backup.sh).
#
# The properties that matter:
#   1. no configured disk is reported (configured 0), never stamped fresh,
#      and exits 1 so the unit shows failed;
#   2. the first run is a FULL and later runs are INCREMENTALS whose
#      base_backup is the previous link of the same chain;
#   3. a failed / empty backup does not stamp success or extend the chain;
#   4. an old chain is removed only AFTER a new full succeeded, and a prune
#      failure keeps it on record rather than forgetting it;
#   5. a base that vanished from the disk resets the chain instead of
#      failing every night forever;
#   6. a failed full's partial upload is swept as an orphan, but never the
#      current chain, a retained one, a newer one, a foreign name, anything
#      outside the database directory, or anything at all when the listing
#      or the running-backup check cannot be trusted.
#
# ClickHouse is a fake `curl` on PATH and clickhouse-disks a stub over a
# directory standing in for the backup disk;
# ch-lake-backup-roundtrip-test.sh runs the same script against a real
# ClickHouse. Run: bash scripts/ops/ch-lake-backup-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/ch-lake-backup.sh"
[[ -r "$SCRIPT" ]] || { echo "ch-lake-backup-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
expect_rc() { if [[ "$rc" -eq "$1" ]]; then ok "$2"; else bad "$2 (rc=$rc)"; fi; }
PROM="$TMP/tf/ch_lake_backup.prom"
prom_has() { if grep -qx -- "$1" "$PROM" 2>/dev/null; then ok "$2"; else bad "$2"; fi; }
prom_stamped() { if grep -q '^stellarindex_ch_lake_backup_last_success_unix [0-9]' "$PROM" 2>/dev/null; then ok "$1"; else bad "$1"; fi; }
prom_full() { if grep -q '^stellarindex_ch_lake_backup_last_full_unix [0-9]' "$PROM" 2>/dev/null; then ok "$1"; else bad "$1"; fi; }
prom_unstamped() { if grep -q last_success_unix "$PROM" 2>/dev/null; then bad "$1"; else ok "$1"; fi; }
last_query() { tail -n1 "$TMP/queries"; }
last_is_full() { local q; q="$(last_query)"; if [[ "$q" == *base_backup* ]]; then bad "$1"; else ok "$1"; fi; }
last_base_is() { local q; q="$(last_query)"; if [[ "$q" == *"base_backup = Disk('si_lake_backup', '$1')"* ]]; then ok "$2"; else bad "$2"; fi; }
file_empty() { if [[ -s "$1" ]]; then bad "$2"; else ok "$2"; fi; }
chains_count() { if [[ "$(grep -c . "$TMP/state/chains")" -eq "$1" ]]; then ok "$2"; else bad "$2"; fi; }
on_record() { if grep -qx -- "$1" "$TMP/state/chains"; then ok "$2"; else bad "$2"; fi; }
off_record() { if grep -qx -- "$1" "$TMP/state/chains"; then bad "$2"; else ok "$2"; fi; }
chain_unchanged() { if cmp -s "$TMP/chain.before" "$TMP/state/chain"; then ok "$1"; else bad "$1"; fi; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
q=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--data-binary" ]]; then q="$2"; shift 2; else shift; fi
done
case "$q" in
  *"status = 'CREATING_BACKUP'"*)
    # The first call is the start-of-run check; MOCK_SWEEP_RUNNING answers
    # the later one (op id, or "fail" for an unreachable server).
    n=$(( $(cat "$MOCK_DISK.calls" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$MOCK_DISK.calls"
    if [[ "$n" -eq 1 && "${MOCK_START_FAIL:-}" == 1 ]]; then exit 22; fi
    if [[ "$n" -gt 1 && "${MOCK_SWEEP_RUNNING:-}" == fail ]]; then exit 22; fi
    if [[ "$n" -gt 1 ]]; then echo "${MOCK_SWEEP_RUNNING:-}"; else echo "${MOCK_RUNNING:-}"; fi ;;
  "BACKUP DATABASE"*)
    printf '%s\n' "$q" >> "$MOCK_LOG"
    mkdir -p "$MOCK_DISK/$(sed -E "s/.* TO Disk\('[^']*', '([^']*)'\).*/\1/" <<<"$q")"
    printf 'op-1\tCREATING_BACKUP\n' ;;
  *"FROM system.backups WHERE id"*) printf '%b\n' "${MOCK_STATUS-BACKUP_CREATED\t4096\t12\t}" ;;
  *) echo "" ;;
esac
STUB
cat > "$TMP/bin/disks" <<'STUB'
#!/usr/bin/env bash
q="$4" # --disk <name> --query <q>
case "$q" in
  "ls "*)
    printf '%s\n' "$*" >> "$MOCK_LS_LOG"
    [[ -n "${MOCK_LS_RC:-}" ]] && exit "$MOCK_LS_RC"
    if [[ -n "${MOCK_LS+set}" ]]; then printf '%s' "$MOCK_LS"; exit 0; fi
    ls "$MOCK_DISK/${q#ls }" 2>/dev/null; exit 0 ;;  # like clickhouse-disks: a missing dir lists empty
  "remove -r "*)
    printf '%s\n' "$*" >> "$MOCK_PRUNE_LOG"
    [[ "${MOCK_PRUNE_RC:-0}" -eq 0 ]] || exit "$MOCK_PRUNE_RC"
    rm -rf "${MOCK_DISK:?}/${q#remove -r }" ;;
  *) exit 64 ;;
esac
STUB
# Wraps flock (absent on macOS: then it answers "held" only for MOCK_LOCK_HELD=1).
# Logs every flock call; MOCK_HEAVY_HELD=1 makes the heavy-lock fd (8) busy.
REAL_FLOCK="$(command -v flock 2>/dev/null || true)"
cat > "$TMP/bin/flock" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "\$MOCK_FLOCK_LOG"
[[ "\${!#}" == 8 && "\${MOCK_HEAVY_HELD:-}" == 1 ]] && exit 1
if [[ -n "$REAL_FLOCK" ]]; then exec "$REAL_FLOCK" "\$@"; fi
[[ "\${MOCK_LOCK_HELD:-}" != 1 ]]
STUB
chmod +x "$TMP/bin/curl" "$TMP/bin/disks" "$TMP/bin/flock"

run() {
  rm -f "$TMP/disk.calls"
  PATH="$TMP/bin:$PATH" MOCK_LOG="$TMP/queries" MOCK_PRUNE_LOG="$TMP/prunes" \
    MOCK_LS_LOG="$TMP/lists" MOCK_DISK="$TMP/disk" MOCK_FLOCK_LOG="$TMP/flocks" \
    HEAVY_JOB_LOCK_DIR="$TMP/locks" \
    STATE_DIR="$TMP/state" TEXTFILE_DIR="$TMP/tf" POLL_SECONDS=0 \
    CH_DISKS_CMD="$TMP/bin/disks" BACKUP_DISK="${DISK-si_lake_backup}" \
    bash "$SCRIPT" 2>"$TMP/stderr"
}
reset() { rm -rf "$TMP/locks" "$TMP/flocks"; mkdir -p "$TMP/locks"; rm -rf "$TMP/state" "$TMP/tf" "$TMP/queries" "$TMP/prunes" "$TMP/lists" "$TMP/disk"; }
on_disk() { if [[ -d "$TMP/disk/$1" ]]; then ok "$2"; else bad "$2"; fi; }
off_disk() { if [[ -d "$TMP/disk/$1" ]]; then bad "$2"; else ok "$2"; fi; }
age_chain() { # make the current chain's full look $1 days old
  local ts=$(( $(date -u +%s) - $1 * 86400 ))
  awk -v ts="$ts" 'BEGIN{FS=OFS="\t"} NR==1{$1=ts} {print}' "$TMP/state/chain" > "$TMP/c" && mv "$TMP/c" "$TMP/state/chain"
}

echo "1. no backup disk configured"
reset
DISK="" run; rc=$?
expect_rc 1 "exits 1 (the unit must not report success)"
prom_has "stellarindex_ch_lake_backup_configured 0" "reports configured 0"
prom_unstamped "never stamps success"
file_empty "$TMP/queries" "issues no BACKUP"

echo "2. first run is a full, later runs extend the chain"
reset
run; rc=$?
expect_rc 0 "full exits 0"
last_is_full "full has no base_backup"
if [[ "$(last_query)" == *"max_memory_usage = 0, s3_strict_upload_part_size = 33554432, s3_max_inflight_parts_for_one_file = 4"* ]]; then
  ok "BACKUP lifts the drifting per-query cap and bounds upload buffers"
else
  bad "BACKUP lifts the drifting per-query cap and bounds upload buffers"
fi
if grep -Eq "TO Disk\('si_lake_backup', 'stellar/[0-9]{8}T[0-9]{6}Z/[0-9]{8}T[0-9]{6}Z-full'\)" "$TMP/queries"; then
  ok "full targets <db>/<chain>/<stamp>-full on the configured disk"
else
  bad "full targets <db>/<chain>/<stamp>-full on the configured disk"
fi
prom_stamped "stamps success"
prom_has "stellarindex_ch_lake_backup_chain_length 1" "chain length 1"
prom_has "stellarindex_ch_lake_backup_last_bytes 4096" "records bytes written"
full_path="$(cut -f2 "$TMP/state/chain")"
sleep 1
run; rc=$?
expect_rc 0 "incremental exits 0"
last_base_is "$full_path" "incremental's base is the full"
incr_path="$(tail -n1 "$TMP/state/chain" | cut -f2)"
if [[ "$(cut -d/ -f2 <<<"$incr_path")" == "$(cut -d/ -f2 <<<"$full_path")" ]]; then
  ok "incremental stays in the full's chain"
else
  bad "incremental stays in the full's chain"
fi
sleep 1
run
last_base_is "$incr_path" "next incremental's base is the previous incremental"
prom_has "stellarindex_ch_lake_backup_chain_length 3" "chain length 3"
file_empty "$TMP/prunes" "no prune while the chain is current"

echo "3. a failed or empty backup does not count"
cp "$TMP/state/chain" "$TMP/chain.before"
MOCK_STATUS='BACKUP_FAILED\t0\t0\tCode: 243. NOT_ENOUGH_SPACE' run; rc=$?
expect_rc 1 "failed backup exits 1"
prom_unstamped "failed backup drops the success stamp"
chain_unchanged "failed backup leaves the chain untouched"
MOCK_STATUS='BACKUP_CREATED\t0\t0\t' run; rc=$?
expect_rc 1 "zero-file backup exits 1"
prom_unstamped "zero-file backup is not stamped"
MOCK_STATUS='' run; rc=$?
expect_rc 1 "vanished operation exits 1"
chain_unchanged "vanished operation leaves the chain untouched"
n_before="$(grep -c . "$TMP/queries")"
MOCK_RUNNING='op-0' run; rc=$?
expect_rc 1 "refuses to start beside a running backup"
if [[ "$(grep -c . "$TMP/queries")" -eq "$n_before" ]]; then ok "issues no second BACKUP"; else bad "issues no second BACKUP"; fi

echo "4. a new full retires the old chain — only after it succeeds"
old_chain="$(cut -d/ -f2 <<<"$full_path")"
age_chain 30
MOCK_STATUS='BACKUP_FAILED\t0\t0\tboom' run
file_empty "$TMP/prunes" "a failed full prunes nothing"
on_record "$old_chain" "a failed full keeps the old chain on record"
sleep 1
run; rc=$?
expect_rc 0 "full after the interval exits 0"
last_is_full "the run after the interval is a full"
if grep -qx -- "--disk si_lake_backup --query remove -r stellar/$old_chain" "$TMP/prunes"; then
  ok "old chain removed from the disk"
else
  bad "old chain removed from the disk"
fi
chains_count 1 "only one chain remains on record"
off_record "$old_chain" "the removed chain is off the record"
prom_has "stellarindex_ch_lake_backup_chain_length 1" "new chain length 1"
fulls="$(grep -v base_backup "$TMP/queries" | sed -n "s/.*TO Disk('si_lake_backup', 'stellar\/\([^/]*\)\/.*/\1/p")"
failed_full="$(tail -n2 <<<"$fulls")"
failed_full="${failed_full%%$'\n'*}"
off_disk "stellar/$failed_full" "the failed full's partial upload is swept as an orphan"
prom_has "stellarindex_ch_lake_backup_orphans_removed 1" "reports one orphan removed"

echo "5. prune failure is loud and forgets nothing"
old_chain="$(cat "$TMP/state/chains")"
age_chain 30
sleep 1
MOCK_PRUNE_RC=1 run; rc=$?
expect_rc 2 "prune failure exits 2"
prom_stamped "the backup itself still stamps success"
on_record "$old_chain" "the unpruned chain stays on record for the next run"
chains_count 2 "both chains on record"

echo "6. a vanished base starts a new chain"
MOCK_STATUS='BACKUP_FAILED\t0\t0\tCode: 599. Backup not found. (BACKUP_NOT_FOUND)' run; rc=$?
expect_rc 1 "missing base exits 1"
if [[ -e "$TMP/state/chain" ]]; then bad "missing base resets the chain"; else ok "missing base resets the chain"; fi
sleep 1
run
last_is_full "the run after a missing base is a full"

echo "7. a malformed chain file is a full, not an incremental on nothing"
printf 'garbage\n' > "$TMP/state/chain"
sleep 1
run
last_is_full "malformed state takes a full"

echo "8. the orphan sweep deletes only unrecorded older chains, and only when it can trust what it sees"
reset
mkdir -p "$TMP/disk/stellar/20200101T000000Z/20200101T000000Z-full" "$TMP/disk/stellar/29990101T000000Z" \
  "$TMP/disk/stellar/not-a-chain" "$TMP/disk/elsewhere/20200101T000000Z"
run; rc=$?
expect_rc 0 "first full with orphans present exits 0"
cur="$(cat "$TMP/state/chains")"
off_disk "stellar/20200101T000000Z" "an older unrecorded chain is removed"
on_disk "stellar/$cur" "the chain just created is kept"
on_disk "stellar/29990101T000000Z" "a chain newer than the current one is kept (could be in flight)"
on_disk "stellar/not-a-chain" "a name this script never mints is kept"
on_disk "elsewhere/20200101T000000Z" "nothing outside the database directory is touched"
prom_has "stellarindex_ch_lake_backup_orphans_removed 1" "one orphan removed"
prom_full "last_full_unix emitted"
prom_has "stellarindex_ch_lake_backup_full_interval_days 28" "full interval emitted"
sleep 1
run
if [[ "$(grep -c . "$TMP/lists")" -eq 1 ]]; then ok "an incremental after a clean sweep does not list the disk"; else bad "an incremental after a clean sweep does not list the disk"; fi
age_chain 30
sleep 1
RETAIN_CHAINS=2 run; rc=$?
expect_rc 0 "second full with RETAIN_CHAINS=2 exits 0"
on_disk "stellar/$cur" "a retained chain is kept"
chains_count 2 "both chains on record"
cur="$(tail -n1 "$TMP/state/chains")"
MOCK_STATUS='BACKUP_FAILED\t0\t0\tboom' run
prom_full "a failed run still reports the recorded full"
mkdir -p "$TMP/disk/stellar/20200102T000000Z"
sweep_case() { # $1 label; env set by caller; forces a sweep via a missing sweep record
  rm -f "$TMP/state/swept"; sleep 1
  run; rc=$?
  expect_rc 2 "$1: the backup counts but the run exits 2"
  on_disk "stellar/20200102T000000Z" "$1: nothing deleted"
  prom_stamped "$1: the backup itself still stamps success"
}
MOCK_LS_RC=1 sweep_case "listing error"
MOCK_LS="" sweep_case "empty listing"
MOCK_LS="20200102T000000Z" sweep_case "listing without the current chain"
MOCK_SWEEP_RUNNING=op-9 sweep_case "a backup in flight at sweep time"
MOCK_SWEEP_RUNNING=fail sweep_case "running-backup check unreachable"
MOCK_PRUNE_RC=1 sweep_case "removal failure"
if [[ -e "$TMP/state/swept" ]]; then bad "a failed sweep leaves no sweep record"; else ok "a failed sweep leaves no sweep record"; fi
sleep 1
run; rc=$?
expect_rc 0 "the next incremental retries the sweep and exits 0"
off_disk "stellar/20200102T000000Z" "the retried sweep removes the orphan"
on_disk "stellar/$cur" "the current chain survives every case"
if [[ -e "$TMP/state/swept" ]]; then ok "a clean sweep is recorded"; else bad "a clean sweep is recorded"; fi

echo "9. the run lock and the fail-closed start check"
reset
mkdir -p "$TMP/state"
holder=""
if command -v flock >/dev/null 2>&1; then
  ( exec 9>"$TMP/state/lock"; flock -n 9 || exit 1; sleep 30 ) &
  holder=$!
  sleep 1
fi
MOCK_LOCK_HELD=1 run; rc=$?
[[ -n "$holder" ]] && { kill "$holder" 2>/dev/null; wait "$holder" 2>/dev/null; }
if [[ "$rc" -ne 0 ]]; then ok "a held lock exits non-zero"; else bad "a held lock exits non-zero"; fi
file_empty "$TMP/queries" "a held lock issues no BACKUP"
prom_unstamped "a held lock stamps nothing"
reset
MOCK_START_FAIL=1 run; rc=$?
if [[ "$rc" -ne 0 ]]; then ok "an unreadable system.backups exits non-zero"; else bad "an unreadable system.backups exits non-zero"; fi
file_empty "$TMP/queries" "an unreadable system.backups issues no BACKUP"
if grep -q "cannot read system.backups" "$TMP/stderr"; then ok "the refusal is logged"; else bad "the refusal is logged"; fi

echo "10. the sweep refuses when the current chain is not on record"
reset
mkdir -p "$TMP/disk/stellar/20200101T000000Z"
run
cur="$(cat "$TMP/state/chains")"
off_disk "stellar/20200101T000000Z" "setup: the sweep removed the orphan on a trusted record"
mkdir -p "$TMP/disk/stellar/20200102T000000Z"
rm -f "$TMP/state/swept"
echo "20190101T000000Z" > "$TMP/state/chains"
sleep 1
run; rc=$?
expect_rc 2 "an untrusted record exits 2"
on_disk "stellar/20200102T000000Z" "nothing deleted when the current chain is not on record"
on_disk "stellar/$cur" "the current chain is kept"

echo "11. only timestamp-named folders under the database directory are deletable"
reset
mkdir -p "$TMP/disk/stellar/20200101T000000Z" "$TMP/disk/stellar/lake-manual-copy"   "$TMP/disk/stellar/20200101T000000" "$TMP/disk/stellar/x20200101T000000Zx"
run
off_disk "stellar/20200101T000000Z" "the timestamp-named orphan is removed"
on_disk "stellar/lake-manual-copy" "a non-timestamp folder is never deleted"
on_disk "stellar/20200101T000000" "a near-miss name without the Z is never deleted"
on_disk "stellar/x20200101T000000Zx" "a name merely containing a timestamp is never deleted"

echo "12. the host-wide heavy-job lock"
reset
run; rc=$?
expect_rc 0 "a free heavy lock lets the backup run"
if grep -qx -- "-s -n 8" "$TMP/flocks" && [[ -e "$TMP/locks/stellarindex-heavy.lock" ]]; then ok "the backup takes stellarindex-heavy.lock shared"; else bad "the backup takes stellarindex-heavy.lock shared"; fi
reset
MOCK_HEAVY_HELD=1 run; rc=$?
expect_rc 75 "a held heavy lock defers the run with exit 75"
file_empty "$TMP/queries" "a deferred run issues no BACKUP"
prom_unstamped "a deferred run stamps nothing"
reset
run; rc=$?
before="$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")"
sleep 1
MOCK_HEAVY_HELD=1 run; rc=$?
expect_rc 75 "a second deferred run exits 75"
if [[ -n "$before" ]] && [[ "$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")" == "$before" ]]; then ok "a deferred run keeps the previous last-success stamp"; else bad "a deferred run keeps the previous last-success stamp"; fi
if grep -q "deferring" "$TMP/stderr"; then ok "the deferral is logged"; else bad "the deferral is logged"; fi
reset
run; rc=$?
before="$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")"
sleep 1
MOCK_RUNNING=op-9 run; rc=$?
expect_rc 1 "a run refused because a backup is already running exits 1"
if [[ -n "$before" ]] && [[ "$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")" == "$before" ]]; then ok "an already-running refusal keeps the previous last-success stamp"; else bad "an already-running refusal keeps the previous last-success stamp"; fi
reset
run; rc=$?
before="$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")"
{ cat "$PROM"; echo "$before"; } > "$PROM.dup" && mv "$PROM.dup" "$PROM"
MOCK_HEAVY_HELD=1 run; rc=$?
if [[ "$(grep -c '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")" -eq 1 && "$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")" == "$before"  ]] && ! grep -qx '[0-9]*' "$PROM"; then ok "a duplicated success line is re-emitted as one value"; else bad "a duplicated success line is re-emitted as one value"; fi
reset
run; rc=$?
before="$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")"
sleep 1
MOCK_LOCK_HELD=1 run; rc=$?
if [[ -n "$before" ]] && [[ "$(grep '^stellarindex_ch_lake_backup_last_success_unix ' "$PROM")" == "$before" ]]; then ok "a run-lock overlap keeps the previous last-success stamp"; else bad "a run-lock overlap keeps the previous last-success stamp"; fi

echo
echo "ch-lake-backup-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
