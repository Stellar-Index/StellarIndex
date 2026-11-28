#!/usr/bin/env bash
# ch-system-log-leftovers.sh — drop the renamed leftovers of ClickHouse system
# log tables (system.<base>_<digits>) that a changed log-table definition
# leaves behind on restart.
#
#   ch-system-log-leftovers.sh [--apply] BASE...      list (default) or drop
#   ch-system-log-leftovers.sh --match NAME BASE...   exit 0 iff NAME is <BASE>_<digits>
#
# Only the `system` database and only names that are exactly an allowlisted BASE plus
# `_` plus digits are ever touched. Prints one `DROPPED system.<name>` per drop.
# A table over max_table_size_to_drop gets the one-shot force_drop_table flag
# (docs/operations/clickhouse-destructive-ddl.md) for that single statement and the
# flag is removed afterwards either way.
set -uo pipefail

ch="${CLICKHOUSE_CLIENT:-clickhouse-client}"
port="${CLICKHOUSE_PORT:-9300}"
flag_dir="${CLICKHOUSE_FLAG_DIR:-/var/lib/clickhouse/flags}"

is_leftover() { # name base...
  local name="$1" base
  shift
  for base in "$@"; do
    [[ "$name" =~ ^${base}_[0-9]+$ ]] && return 0
  done
  return 1
}

mode=list
if [[ "${1:-}" == "--match" ]]; then
  shift
  name="${1:-}"
  shift
  is_leftover "$name" "$@"
  exit $?
fi
if [[ "${1:-}" == "--apply" ]]; then
  mode=apply
  shift
fi
if [[ $# -eq 0 ]]; then
  echo "usage: $0 [--apply] BASE..." >&2
  exit 2
fi
for base in "$@"; do
  if [[ ! "$base" =~ ^[a-z][a-z_]*$ ]]; then
    echo "ch-system-log-leftovers: refusing base name '$base'" >&2
    exit 2
  fi
done

alt="$(IFS='|'; echo "$*")"
rows="$("$ch" --port "$port" -q "SELECT name, total_bytes FROM system.tables \
WHERE database = 'system' AND match(name, '^(${alt})_[0-9]+\$') ORDER BY name FORMAT TSV")" || exit 1
limit="$("$ch" --port "$port" -q "SELECT value FROM system.server_settings \
WHERE name = 'max_table_size_to_drop' FORMAT TSV")" || exit 1

while IFS=$'\t' read -r name bytes; do
  [[ -z "$name" ]] && continue
  is_leftover "$name" "$@" || continue
  [[ "$bytes" =~ ^[0-9]+$ ]] || bytes=0
  if [[ "$mode" == list ]]; then
    echo "LEFTOVER system.$name ${bytes:-0} bytes"
    continue
  fi
  flag="$flag_dir/force_drop_table"
  trap 'rm -f "$flag"' EXIT
  if [[ "${limit:-0}" != 0 && "${bytes:-0}" -gt "${limit:-0}" ]]; then
    touch "$flag" || exit 1
  fi
  "$ch" --port "$port" -q "DROP TABLE IF EXISTS system.\`$name\` SYNC" || exit 1
  rm -f "$flag"
  echo "DROPPED system.$name"
done <<<"$rows"
