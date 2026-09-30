#!/usr/bin/env bash
# ansible-pg-logrotate-cap-test.sh — pins that the hourly Postgres log
# rotation actually enforces a size cap on a freshly built host.
#   - pg-logrotate.service must read a config the role installs: the stock
#     /etc/logrotate.d/postgresql-common has no `maxsize`, so running it
#     hourly enforces nothing.
#   - That config must live outside /etc/logrotate.d, or the daily run sees
#     the glob twice and refuses both as "duplicate log entry".
#   - It must carry `maxsize` and `copytruncate` (postgres never reopens).
#
# Structural only (awk/grep over the real repo files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-configs/ansible/roles/archival-node}"
UNIT="${UNIT:-deploy/systemd/pg-logrotate.service}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

conf="$(sed -nE 's|^ExecStart=/usr/sbin/logrotate[[:space:]]+(.*[[:space:]])?(/[^[:space:]]+)[[:space:]]*$|\2|p' "$UNIT")"
if [ -z "$conf" ]; then
  echo "ansible-pg-logrotate-cap-test: FAIL — no logrotate ExecStart in $UNIT" >&2
  exit 1
fi

case "$conf" in
  /etc/logrotate.d/*) bad "$UNIT reads $conf — under /etc/logrotate.d (dpkg-owned or duplicate-glob)" ;;
  *) ok "$UNIT reads $conf, outside /etc/logrotate.d" ;;
esac

# The src of the first uncommented copy task whose dest is $conf.
src="$(awk -v dest="$conf" '
  function flush() { if (hit && s != "" && !found) { print s; found = 1 }; hit = 0; s = "" }
  /^- name:/ { flush() }
  /^[ \t]*#/ { next }
  { t = $0; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t); gsub(/"/, "", t) }
  t == "dest: " dest { hit = 1 }
  t ~ /^src: / { s = substr(t, 6) }
  END { flush() }' "$ROLE"/tasks/*.yml)"
if [ -z "$src" ] || [ ! -f "$ROLE/files/$src" ]; then
  bad "no $ROLE task copies a files/ src to $conf — a rebuilt host has no capped policy"
else
  ok "installed from $ROLE/files/$src"
  body="$(grep -vE '^[[:space:]]*#' "$ROLE/files/$src")"
  for want in '^/var/log/postgresql/\*\.log[[:space:]]*\{' '^[[:space:]]*maxsize[[:space:]]+[0-9]+[kMG]?[[:space:]]*$' '^[[:space:]]*copytruncate[[:space:]]*$'; do
    if grep -qE "$want" <<<"$body"; then ok "$src matches $want"; else bad "$src lacks $want"; fi
  done
fi

echo "ansible-pg-logrotate-cap-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
