#!/usr/bin/env bash
# ch-lake-backup-roundtrip-test.sh — runs scripts/ops/ch-lake-backup.sh
# against a REAL ClickHouse (the image test/integration pins) and proves the
# backups it takes restore: full, then an incremental on top of it, then
# DROP DATABASE and RESTORE from the incremental must give back every row,
# byte-identical by content hash. A third run past the full interval must
# start a new chain and remove the old one from the backup disk.
#
# The backup disk here is a local disk; production declares an s3_plain disk
# with the same name (configs/ansible/.../clickhouse-lake-backup-disk.xml.j2).
# The script only ever addresses it as Disk('<name>', ...), so the SQL path
# is identical.
#
# Needs docker. Without it this exits 2 — not run, never a pass.
# Run: bash scripts/ops/ch-lake-backup-roundtrip-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/ch-lake-backup.sh"
IMAGE="${CH_IMAGE:-clickhouse/clickhouse-server:26.5}"
command -v docker >/dev/null || { echo "ch-lake-backup-roundtrip-test: docker not found — NOT RUN" >&2; exit 2; }

TMP="$(mktemp -d)"
name="ch-lake-backup-rt-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1; rm -rf "$TMP"; }
trap cleanup EXIT

cat > "$TMP/si-lake-backup.xml" <<'EOF'
<clickhouse>
  <storage_configuration><disks>
    <si_lake_backup><type>local</type><path>/backups/</path></si_lake_backup>
  </disks></storage_configuration>
  <backups><allowed_disk>si_lake_backup</allowed_disk></backups>
</clickhouse>
EOF
chmod 644 "$TMP/si-lake-backup.xml"

docker run -d --name "$name" -e CLICKHOUSE_SKIP_USER_SETUP=1 -p 127.0.0.1::8123 \
  -v "$TMP/si-lake-backup.xml:/etc/clickhouse-server/config.d/si-lake-backup.xml:ro" \
  "$IMAGE" >/dev/null || { echo "cannot start $IMAGE" >&2; exit 2; }
mapping="$(docker port "$name" 8123/tcp)"
mapping="${mapping%%$'\n'*}"
port="${mapping##*:}"
CH="http://127.0.0.1:$port/"
for _ in $(seq 1 60); do curl -sf "${CH}ping" >/dev/null && break; sleep 1; done
docker exec "$name" sh -c 'mkdir -p /backups && chown clickhouse /backups'

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
q() { curl -sSf "$CH" --data-binary "$1"; }
fingerprint() {
  q "SELECT 'ledgers', count(), sum(cityHash64(*)) FROM stellar.ledgers
     UNION ALL SELECT 'ops', count(), sum(cityHash64(*)) FROM stellar.operations
     ORDER BY 1 FORMAT TabSeparated"
}
backup() {
  BACKUP_DISK=si_lake_backup CH_HTTP="$CH" STATE_DIR="$TMP/state" TEXTFILE_DIR="$TMP/tf" \
    POLL_SECONDS=1 CH_DISKS_CMD="docker exec $name clickhouse-disks -C /etc/clickhouse-server/config.xml" \
    bash "$SCRIPT"
}
metric() { awk -v m="$1" '$1 == m {print $2}' "$TMP/tf/ch_lake_backup.prom"; }

q "CREATE DATABASE stellar"
q "CREATE TABLE stellar.ledgers (ledger_seq UInt32, hash String) ENGINE = ReplacingMergeTree
   PARTITION BY intDiv(ledger_seq, 100000) ORDER BY ledger_seq"
q "CREATE TABLE stellar.operations (ledger_seq UInt32, op_index UInt16, body String) ENGINE = MergeTree
   PARTITION BY intDiv(ledger_seq, 100000) ORDER BY (ledger_seq, op_index)"
q "INSERT INTO stellar.ledgers SELECT number, hex(cityHash64(number)) FROM numbers(300000)"
q "INSERT INTO stellar.operations SELECT intDiv(number, 3), number % 3, repeat('x', 40) FROM numbers(900000)"

echo "1. full"
if backup; then ok "full backup exits 0"; else bad "full backup exits 0"; fi
full_bytes="$(metric stellarindex_ch_lake_backup_last_bytes)"
first_chain="$(cut -f2 "$TMP/state/chain" | cut -d/ -f2)"

echo "2. incremental on top of it"
q "INSERT INTO stellar.ledgers SELECT number, hex(cityHash64(number)) FROM numbers(300000, 1000)"
q "INSERT INTO stellar.operations SELECT intDiv(number, 3), number % 3, 'y' FROM numbers(900000, 3000)"
sleep 1
if backup; then ok "incremental backup exits 0"; else bad "incremental backup exits 0"; fi
incr_bytes="$(metric stellarindex_ch_lake_backup_last_bytes)"
if [[ "${incr_bytes:-0}" -gt 0 && "$incr_bytes" -lt $(( ${full_bytes:-0} / 10 )) ]]; then
  ok "incremental wrote only the new parts ($incr_bytes bytes vs full $full_bytes)"
else
  bad "incremental wrote only the new parts ($incr_bytes bytes vs full $full_bytes)"
fi
before="$(fingerprint)"
last_path="$(tail -n1 "$TMP/state/chain" | cut -f2)"

echo "3. DROP + RESTORE from the incremental"
q "DROP DATABASE stellar SYNC"
q "RESTORE DATABASE stellar FROM Disk('si_lake_backup', '$last_path')" >/dev/null
after="$(fingerprint)"
if [[ -n "$before" && "$before" == "$after" ]]; then
  ok "restored lake is identical: $(tr '\n\t' '; ' <<<"$after")"
else
  bad "restored lake differs: before [$before] after [$after]"
fi

echo "4. a new chain retires the old one"
ts=$(( $(date -u +%s) - 30 * 86400 ))
awk -v ts="$ts" 'BEGIN{FS=OFS="\t"} NR==1{$1=ts} {print}' "$TMP/state/chain" > "$TMP/c" && mv "$TMP/c" "$TMP/state/chain"
sleep 1
if backup; then ok "new full exits 0"; else bad "new full exits 0"; fi
on_disk="$(docker exec "$name" ls /backups/stellar)"
new_chain="$(cut -f2 "$TMP/state/chain" | cut -d/ -f2)"
if ! grep -qx "$first_chain" <<<"$on_disk" && grep -qx "$new_chain" <<<"$on_disk"; then
  ok "old chain $first_chain removed, new chain $new_chain present"
else
  bad "chains on disk after the new full: $(tr '\n' ' ' <<<"$on_disk")"
fi

echo "5. a base deleted out from under the chain resets it"
docker exec "$name" rm -rf "/backups/stellar/$new_chain"
sleep 1
backup; rc=$?
if [[ "$rc" -eq 1 && ! -e "$TMP/state/chain" ]]; then
  ok "missing base fails the run (rc=$rc) and resets the chain"
else
  bad "missing base: rc=$rc, chain file $( [[ -e "$TMP/state/chain" ]] && echo kept || echo reset)"
fi

echo
echo "ch-lake-backup-roundtrip-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 && "$pass" -eq 7 ]]
