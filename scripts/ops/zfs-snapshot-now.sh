#!/usr/bin/env bash
# zfs-snapshot-now.sh — take ONE fresh rolling snapshot of a managed
# dataset, right now. This is the "fresh snapshot" precondition the
# ClickHouse destructive-DDL runbook (docs/operations/clickhouse-destructive-ddl.md)
# and any Postgres migration you are nervous about ask for.
#
#   sudo scripts/ops/zfs-snapshot-now.sh data/clickhouse
#   sudo scripts/ops/zfs-snapshot-now.sh data/postgres --keep pre-0123-migration
#
# Without --keep the snapshot is `auto-YYYYMMDD-HHMM`: it rides the
# normal retention (3 d ClickHouse / 7 d Postgres) and the min-free
# guard, so a pre-DDL snapshot does not become a forgotten 250 GB/day
# pin. With --keep it is `manual-<label>` and is NEVER auto-destroyed —
# you own its deletion.
#
# The min-free guard applies either way: on a pool under the floor
# this prunes older auto snapshots first and REFUSES (exit 1) if that
# is not enough. Do not work around it; free space is the fix.
#
# Runs the installed copy (/usr/local/bin/zfs-snapshot.sh, shipped by
# the archival-node role tag `zfs-snapshots`) with its own
# /etc/default/zfs-snapshot config so `now` sees the same dataset list
# and floor the timer does; falls back to the sibling script in this
# checkout when run on a host without the role applied.
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
