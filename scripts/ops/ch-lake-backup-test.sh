#!/usr/bin/env bash
# ch-lake-backup-test.sh — fixture tests for the ClickHouse lake's data
# backup (scripts/ops/ch-lake-backup.sh).
#
# The properties that matter:
#   1. no configured disk is reported (configured 0), never stamped fresh;
#   2. the first run is a FULL and later runs are INCREMENTALS whose
#      base_backup is the previous link of the same chain;
#   3. a failed / empty backup does not stamp success or extend the chain;
#   4. an old chain is removed only AFTER a new full succeeded, and a prune
#      failure keeps it on record rather than forgetting it;
#   5. a base that vanished from the disk resets the chain instead of
#      failing every night forever.
#
# ClickHouse is a fake `curl` on PATH and clickhouse-disks a logging stub;
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
  *"status = 'CREATING_BACKUP'"*) echo "${MOCK_RUNNING:-}" ;;
  "BACKUP DATABASE"*)
    printf '%s\n' "$q" >> "$MOCK_LOG"
    printf 'op-1\tCREATING_BACKUP\n' ;;
  *"FROM system.backups WHERE id"*) printf '%b\n' "${MOCK_STATUS-BACKUP_CREATED\t4096\t12\t}" ;;
  *) echo "" ;;
esac
STUB
cat > "$TMP/bin/disks" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$MOCK_PRUNE_LOG"
exit "${MOCK_PRUNE_RC:-0}"
STUB
chmod +x "$TMP/bin/curl" "$TMP/bin/disks"

run() {
  PATH="$TMP/bin:$PATH" MOCK_LOG="$TMP/queries" MOCK_PRUNE_LOG="$TMP/prunes" \
    STATE_DIR="$TMP/state" TEXTFILE_DIR="$TMP/tf" POLL_SECONDS=0 \
    CH_DISKS_CMD="$TMP/bin/disks" BACKUP_DISK="${DISK-si_lake_backup}" \
    bash "$SCRIPT" 2>"$TMP/stderr"
}
reset() { rm -rf "$TMP/state" "$TMP/tf" "$TMP/queries" "$TMP/prunes"; }
age_chain() { # make the current chain's full look $1 days old
  local ts=$(( $(date -u +%s) - $1 * 86400 ))
  awk -v ts="$ts" 'BEGIN{FS=OFS="\t"} NR==1{$1=ts} {print}' "$TMP/state/chain" > "$TMP/c" && mv "$TMP/c" "$TMP/state/chain"
}

echo "1. no backup disk configured"
reset
DISK="" run; rc=$?
expect_rc 0 "exits 0"
prom_has "stellarindex_ch_lake_backup_configured 0" "reports configured 0"
prom_unstamped "never stamps success"
file_empty "$TMP/queries" "issues no BACKUP"

echo "2. first run is a full, later runs extend the chain"
reset
run; rc=$?
expect_rc 0 "full exits 0"
last_is_full "full has no base_backup"
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

echo
echo "ch-lake-backup-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
