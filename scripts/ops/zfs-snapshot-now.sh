#!/usr/bin/env bash
# zfs-snapshot-now.sh — take ONE fresh rolling snapshot of a managed dataset,
# right now: the "fresh snapshot" precondition of the ClickHouse destructive-DDL
# runbook (docs/operations/clickhouse-destructive-ddl.md) and of any nervous
# Postgres migration.
#
#   sudo scripts/ops/zfs-snapshot-now.sh data/clickhouse
#   sudo scripts/ops/zfs-snapshot-now.sh data/postgres --keep pre-0123-migration
#
# Without --keep the snapshot is `auto-YYYYMMDD-HHMM`: it rides normal
# retention (3 d ClickHouse / 7 d Postgres) and the min-free guard, so it does
# not become a forgotten 250 GB/day pin. With --keep it is `manual-<label>` and
# is NEVER auto-destroyed; you own its deletion.
#
# Recursive: data/postgres includes data/postgres/wal, so data and WAL come
# from one instant.
#
# The min-free guard applies either way: under the floor it prunes older auto
# snapshots first and REFUSES (exit 1) if that is not enough. Do not work
# around it; free space is the fix.
#
# Runs the installed /usr/local/bin/zfs-snapshot.sh (archival-node role tag
# `zfs-snapshots`) with /etc/default/zfs-snapshot, so `now` sees the timer's
# dataset list and floor; falls back to the sibling script in this checkout on
# a host without the role.
set -euo pipefail

[[ $# -ge 1 ]] || { echo "usage: $0 <dataset> [--keep <label>]" >&2; exit 2; }

ENV_FILE="${ZFS_SNAPSHOT_ENV:-/etc/default/zfs-snapshot}"
if [[ -r "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090  # operator-rendered KEY=VALUE file, no secrets
  . "$ENV_FILE"
  set +a
fi

SCRIPT="${ZFS_SNAPSHOT_SCRIPT:-/usr/local/bin/zfs-snapshot.sh}"
if [[ ! -x "$SCRIPT" ]]; then
  SCRIPT="$(cd "$(dirname "$0")" && pwd)/zfs-snapshot.sh"
fi

exec "$SCRIPT" now "$@"
