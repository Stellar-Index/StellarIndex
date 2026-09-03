#!/usr/bin/env bash
# ch-live-catchup.sh — keep the ClickHouse Tier-1 lake COMPLETE up to the live
# indexer's confirmed position (ADR-0034 live fan-out). Two responsibilities:
#
#   1. HOLE HEALING — the in-dispatcher dual-sink (clickhouse.LiveSink) is
#      best-effort: it DROPS whole ledgers under buffer pressure and a flush can
#      partially fail, leaving holes BELOW the lake's max ledger. A tip-only
#      catch-up ([CH_max+1, tip]) can never re-fill those — the sink already
#      wrote past them. So we gap-scan stellar.ledgers over [LIVE_ERA_FROM, tip]
#      and ch-backfill every missing range.
#   2. TIP EXTENSION — backfill [CH_max+1, tip] for whatever the dual-sink hasn't
#      reached yet (e.g. between flushes, or while the sink was down).
#
# ch-backfill is idempotent (ReplacingMergeTree dedups), so this is safe on a
# short timer; each run only writes the ranges that are actually missing. The
# real-time projector (clickhouse_projector_source) reads CH only up to the
# contiguous-completeness watermark, so a hole stalls the projector until THIS
# script heals it — never silent loss.
set -uo pipefail
# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
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
# Optional ops-user credentials (STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD,
# from /etc/default/stellarindex-ops). Handed to clickhouse-client via its
# CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env — never argv, which ps and the journal
# would show. Unset ⇒ nothing exported; the default user exactly as before.
if [ -n "${STELLARINDEX_CLICKHOUSE_OPS_USER:-}" ]; then
  export CLICKHOUSE_USER="$STELLARINDEX_CLICKHOUSE_OPS_USER"
  export CLICKHOUSE_PASSWORD="${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD:-}"
fi
OPS=${OPS:-/usr/local/bin/stellarindex-ops-ch}
CFG=${CFG:-/etc/stellarindex.toml}
DSN="$STELLARINDEX_POSTGRES_DSN"
PAR=${PAR:-4}
CH() { clickhouse-client --port "${CH_PORT:-9300}" "$@"; }
# LIVE_ERA_FROM is the lowest ledger the in-dispatcher dual-sink is
# responsible for — one past the ceiling of the certified bulk backfill.
# Everything below it was written by ch-backfill and is already contiguous;
# at or above it a sink-shaped hole can form, so that is where the gap scan
# starts.
#
# It is a per-DEPLOYMENT fact with no defensible default (#371 F10). The
# literal that used to sit here was r1's mainnet backfill ceiling, shipped
# verbatim by the ansible role to every host it provisions — and both ways of
# being wrong are SILENT:
#
#   too high — the gap scan matches nothing, and the lake's only self-healer
#              quietly becomes a no-op (the projector then stalls at the
#              contiguous watermark behind the first unhealed hole);
#   too low  — a 10-minute timer turns into a full-history DISTINCT scan
#              against the ClickHouse that also serves the explorer.
#
# So refuse to guess. Set `stellarindex_ch_live_era_from` in the host's
# inventory; roles/archival-node/tasks/09-minio.yml templates it into
# /etc/default/stellarindex-ops, which this script reads above. Derive it as
# the `-to` of the last ch-backfill range certified complete, plus one.
if [ -z "${LIVE_ERA_FROM:-}" ]; then
  echo "$(date -u) ch-live-catchup: LIVE_ERA_FROM is unset — refusing to guess the live-era floor (a wrong floor fails silently in both directions). Set stellarindex_ch_live_era_from in this host's inventory and re-apply the archival-node role with --tags minio." >&2
  exit 1
fi
# Digits only: the value is interpolated into the gap-scan SQL below, so this
# is both a typo guard and the reason no operator-supplied text reaches the
# query.
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

# 1. Heal holes below CH_MAX. leadInFrame's (CURRENT ROW .. 1 FOLLOWING) frame
#    returns the last row's own value, so there is no spurious trailing gap.
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
  FORMAT TSV" 2>/dev/null)
if [ -n "$GAPS" ]; then
  NGAPS=$(printf '%s\n' "$GAPS" | wc -l | tr -d '[:space:]')
  echo "$(date -u) ch-live-catchup: healing $NGAPS hole(s) in [$LIVE_ERA_FROM,$CH_MAX]"
  while IFS=$'\t' read -r gstart gend; do
    [ -z "$gstart" ] && continue
    echo "$(date -u) ch-live-catchup: heal [$gstart,$gend] ($((gend - gstart + 1)) ledgers)"
    "$OPS" ch-backfill -config "$CFG" -from "$gstart" -to "$gend" -parallel "$PAR" || rc=1
  done <<< "$GAPS"
else
  echo "$(date -u) ch-live-catchup: no holes in [$LIVE_ERA_FROM,$CH_MAX]"
fi

# 2. Extend the tip.
if [ "$TIP" -gt "$CH_MAX" ]; then
  FROM=$((CH_MAX + 1))
  echo "$(date -u) ch-live-catchup: tip-extend [$FROM,$TIP] ($((TIP - CH_MAX)) ledgers)"
  "$OPS" ch-backfill -config "$CFG" -from "$FROM" -to "$TIP" -parallel "$PAR" || rc=1
else
  echo "$(date -u) ch-live-catchup: tip current (max=$CH_MAX tip=$TIP)"
fi

exit $rc
