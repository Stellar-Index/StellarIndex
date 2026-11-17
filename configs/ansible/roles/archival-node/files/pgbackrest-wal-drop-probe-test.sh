#!/usr/bin/env bash
# Drives the shipped pgbackrest-wal-drop-probe.sh against fixture logs.
# Run: bash configs/ansible/roles/archival-node/files/pgbackrest-wal-drop-probe-test.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 1
PROBE="$PWD/pgbackrest-wal-drop-probe.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
pass=0
fail=0
ok() { echo "ok: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }

# GNU date parses "YYYY-MM-DD HH:MM:SS"; BSD date (macOS) does not.
if ! date -d '2026-01-01 00:00:00' +%s >/dev/null 2>&1; then
  if command -v gdate >/dev/null; then
    mkdir -p "$WORK/bin" && ln -s "$(command -v gdate)" "$WORK/bin/date"
    export PATH="$WORK/bin:$PATH"
  else
    echo "pgbackrest-wal-drop-probe-test: SKIP (no GNU date)"
    exit 0
  fi
fi
export TZ=UTC

run() { # <log dir> -> value or "rc=N"
  local out="$WORK/out" rc
  mkdir -p "$out"
  rm -f "$out"/*.prom
  TEXTFILE_DIR="$out" PGBACKREST_LOG_DIR="$1" sh "$PROBE" 2>/dev/null
  rc=$?
  if [ "$rc" -ne 0 ]; then echo "rc=$rc"; return; fi
  awk '/^stellarindex_pgbackrest_wal_last_drop_unix /{print $2}' "$out/pgbackrest_wal_drop.prom"
}

mkdir -p "$WORK/clean" "$WORK/dropped"
printf '2026-11-17 09:00:00.001 P00   INFO: pushed WAL file 000000010000000A000000FF to the archive asynchronously\n' \
  > "$WORK/clean/stellarindex-archive-push-async.log"
cp "$WORK/clean/stellarindex-archive-push-async.log" "$WORK/dropped/"
printf "2026-11-17 10:00:00.123 P00   WARN: dropped WAL file '000000010000000B00000001' because archive queue exceeded 192GB\n" \
  >> "$WORK/dropped/stellarindex-archive-push-async.log"
printf "2026-11-17 11:30:00.000 P00   WARN: dropped WAL file '000000010000000B00000002' because archive queue exceeded 192GB\n" \
  > "$WORK/dropped/stellarindex-archive-push.log"

v="$(run "$WORK/clean")"
if [ "$v" = 0 ]; then ok "no drop in the logs publishes 0"; else bad "clean logs gave '$v', want 0"; fi

v="$(run "$WORK/dropped")"
if [ "$v" = 1794915000 ]; then ok "the newest drop across logs is published (2026-11-17 11:30 UTC)"; else bad "drop logs gave '$v', want 1794915000"; fi

v="$(run "$WORK/missing-dir")"
if [ "$v" = 0 ]; then ok "a host without pgBackRest logs publishes 0"; else bad "missing dir gave '$v', want 0"; fi

printf 'garbage WARN: dropped WAL file x\n' > "$WORK/clean/bad-archive-push.log"
v="$(run "$WORK/clean")"
if [ "$v" = "rc=1" ]; then ok "an unparseable drop line fails the run instead of publishing 0"; else bad "unparseable drop line gave '$v', want rc=1"; fi

echo
echo "pgbackrest-wal-drop-probe-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
