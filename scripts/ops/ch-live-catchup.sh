#!/usr/bin/env bash
# ch-live-catchup.sh — keep the ClickHouse Tier-1 lake COMPLETE up to the live
#
#   1. HOLE HEALING: the in-dispatcher dual-sink (clickhouse.LiveSink) is
#      best-effort. It DROPS whole ledgers under buffer pressure and a flush
#      can partly fail, leaving holes BELOW the lake's max ledger. A tip-only
#      catch-up ([CH_max+1, tip]) can never re-fill them, so gap-scan
#      stellar.ledgers over [LIVE_ERA_FROM, tip] and ch-backfill every gap.
#   2. TIP EXTENSION: backfill [CH_max+1, tip] for what the dual-sink has not
#      reached yet (between flushes, or while the sink was down).
#
# ch-backfill is idempotent (ReplacingMergeTree dedups), so this is safe on a
# short timer. The projector (clickhouse_projector_source) reads CH only up to
# the contiguous-completeness watermark, so a hole stalls it until this script
# heals it: never silent loss.
set -uo pipefail
# Read a systemd EnvironmentFile VERBATIM, never `.`/source it: its values are
# unquoted (as systemd wants), so the shell would expand `$`, split on `;&|`
# and whitespace, and eat quotes inside a secret, mangling the DSN while the
# services keep working. Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
load_env_file /etc/default/stellarindex-ops export
# Optional ops-user credentials (from /etc/default/stellarindex-ops), handed to
# clickhouse-client via CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env, never argv,
# which ps and the journal would show. Unset: the default user.
if [ -n "${STELLARINDEX_CLICKHOUSE_OPS_USER:-}" ]; then
  export CLICKHOUSE_USER="$STELLARINDEX_CLICKHOUSE_OPS_USER"
  export CLICKHOUSE_PASSWORD="${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD:-}"
fi
OPS=${OPS:-/usr/local/bin/stellarindex-ops-ch}
CFG=${CFG:-/etc/stellarindex.toml}
DSN="$STELLARINDEX_POSTGRES_DSN"
PAR=${PAR:-4}
# ops_batch's -C file once the role renders it. It must win over root's
# ~/.clickhouse-client/config.xml (ops_admin); absent, the env pair applies.
CH_CFG=/etc/clickhouse-client/ops-batch.xml
CH_AUTH=(-C "$CH_CFG")
if [[ ! -r "$CH_CFG" ]]; then
  echo "ch-live-catchup.sh: credential file $CH_CFG unreadable; using ClickHouse default user" >&2
  CH_AUTH=()
fi
CH() { clickhouse-client ${CH_AUTH[@]+"${CH_AUTH[@]}"} --port "${CH_PORT:-9300}" "$@"; }
# LIVE_ERA_FROM is the lowest ledger the in-dispatcher dual-sink is responsible
# for: one past the ceiling of the certified bulk backfill. Below it ch-backfill
# wrote contiguously; at or above it a sink-shaped hole can form, so the gap
# scan starts there.
#
# It is a per-DEPLOYMENT fact with no defensible default (a literal such as
# r1's backfill ceiling would ship via the ansible role to every host), and
# both wrong directions are SILENT:
#   too high: the gap scan matches nothing and the only self-healer becomes a
#             no-op (the projector then stalls behind the first unhealed hole);
#   too low:  a 10-minute timer becomes a full-history DISTINCT scan against
#             the ClickHouse that also serves the explorer.
# So refuse to guess. Set `stellarindex_ch_live_era_from` in the host's
# inventory; roles/archival-node/tasks/09-minio.yml templates it into
# /etc/default/stellarindex-ops. Derive it as the `-to` of the last
# ch-backfill range certified complete, plus one.
if [ -z "${LIVE_ERA_FROM:-}" ]; then
  echo "$(date -u) ch-live-catchup: LIVE_ERA_FROM is unset — refusing to guess the live-era floor (a wrong floor fails silently in both directions). Set stellarindex_ch_live_era_from in this host's inventory and re-apply the archival-node role with --tags minio." >&2
  exit 1
fi
# Digits only: the value is interpolated into the gap-scan SQL, so this is
# both a typo guard and why no operator text reaches the query.
case "$LIVE_ERA_FROM" in
  *[!0-9]*|'')
    echo "$(date -u) ch-live-catchup: LIVE_ERA_FROM='${LIVE_ERA_FROM}' is not a ledger sequence (digits only)." >&2
    exit 1
    ;;
esac

CH_MAX=$(CH -q "SELECT max(ledger_seq) FROM stellar.ledgers" 2>/dev/null)
TIP=$(psql "$DSN" -tAc "SELECT max(last_ledger) FROM ingestion_cursors" 2>/dev/null | tr -d '[:space:]')
if [ -z "${CH_MAX:-}" ] || [ -z "${TIP:-}" ]; then
  echo "$(date -u) ch-live-catchup: could not resolve CH_MAX=$CH_MAX / TIP=$TIP" >&2
  exit 1
fi

rc=0

# 1. Heal holes below CH_MAX in two arms that together mirror the Go
#    ContiguousWatermark (internal/storage/clickhouse/completeness.go) the
#    projector clamps on: any hole it can stall at must be found here.
#
#  1a. The FLOOR hole. The interior scan only sees a jump between two present
#      ledgers, so a hole starting AT LIVE_ERA_FROM is invisible to it.
#      MIN_PRESENT is ContiguousWatermark's min_present arm; 0 means nothing at
#      or above the floor is present yet, which the tip-extend covers.
MIN_PRESENT=$(CH -q "SELECT ifNull(min(ledger_seq), 0) FROM stellar.ledgers WHERE ledger_seq >= ${LIVE_ERA_FROM}")
FLOOR_GAP=
case "$MIN_PRESENT" in
  *[!0-9]*|'')
    echo "$(date -u) ch-live-catchup: could not resolve the lowest ledger >= $LIVE_ERA_FROM (got '$MIN_PRESENT'); a floor hole would go unhealed" >&2
    rc=1
    ;;
  *)
    if [ "$MIN_PRESENT" -gt "$LIVE_ERA_FROM" ]; then
      FLOOR_GAP=$(printf '%s\t%s' "$LIVE_ERA_FROM" "$((MIN_PRESENT - 1))")
    fi
    ;;
esac

#  1b. INTERIOR holes. leadInFrame's (CURRENT ROW .. 1 FOLLOWING) frame returns
#      the last row's own value, so there is no spurious trailing gap. stderr
#      stays on the journal and the exit status is checked: a failed scan also
#      yields no rows and must not read as "no holes".
GAPS=$(CH -q "
  SELECT gap_start, gap_end FROM (
    SELECT ledger_seq + 1 AS gap_start, nxt - 1 AS gap_end
    FROM (
      SELECT ledger_seq,
             leadInFrame(ledger_seq) OVER (
                 ORDER BY ledger_seq ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING
             ) AS nxt
      FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= ${LIVE_ERA_FROM})
    )
    WHERE nxt > ledger_seq + 1
  )
  ORDER BY gap_start
  FORMAT TSV")
scan_rc=$?
if [ "$scan_rc" -ne 0 ]; then
  echo "$(date -u) ch-live-catchup: gap scan FAILED (clickhouse-client exit $scan_rc) — holes in [$LIVE_ERA_FROM,$CH_MAX] NOT checked or healed" >&2
  rc=1
  GAPS=
fi
if [ -n "$FLOOR_GAP" ]; then
  GAPS=$(printf '%s\n%s' "$FLOOR_GAP" "$GAPS")
fi
if [ -n "$GAPS" ]; then
  NGAPS=$(printf '%s\n' "$GAPS" | wc -l | tr -d '[:space:]')
  echo "$(date -u) ch-live-catchup: healing $NGAPS hole(s) in [$LIVE_ERA_FROM,$CH_MAX]"
  while IFS=$'\t' read -r gstart gend; do
    [ -z "$gstart" ] && continue
    echo "$(date -u) ch-live-catchup: heal [$gstart,$gend] ($((gend - gstart + 1)) ledgers)"
    "$OPS" ch-backfill -write -config "$CFG" -from "$gstart" -to "$gend" -parallel "$PAR" || rc=1
  done <<< "$GAPS"
elif [ "$scan_rc" -eq 0 ]; then
  echo "$(date -u) ch-live-catchup: no holes in [$LIVE_ERA_FROM,$CH_MAX]"
fi

# 2. Extend the tip.
if [ "$TIP" -gt "$CH_MAX" ]; then
  FROM=$((CH_MAX + 1))
  echo "$(date -u) ch-live-catchup: tip-extend [$FROM,$TIP] ($((TIP - CH_MAX)) ledgers)"
  "$OPS" ch-backfill -write -config "$CFG" -from "$FROM" -to "$TIP" -parallel "$PAR" || rc=1
else
  echo "$(date -u) ch-live-catchup: tip current (max=$CH_MAX tip=$TIP)"
fi

exit $rc
