#!/usr/bin/env bash
# ch-full-backfill.sh — durable, resumable Phase-3 historic backfill driver
# (ADR-0034). Walks [FROM, TO] in fixed WINDOW-sized slices (aligned to the
# ClickHouse 1M-ledger partition scheme) and runs `stellarindex-ops ch-backfill
# -parallel PAR` on each window from the galexie-archive bucket.
#
# Resume: a window is recorded in $STATE, with its end, only after ch-backfill
# exits 0. A restart skips a window only when the recorded end reaches the
# window's end now AND the lake still holds every ledger in it, so a later TO
# fills the tail it adds and a state file that outlived a lake loss skips
# nothing. ClickHouse writes are idempotent (ReplacingMergeTree), so re-running
# a window is safe.
#
# Run detached on r1, e.g.:
#   set -a; . /etc/default/stellarindex-ops; set +a
#   setsid nice -n 10 bash scripts/ops/ch-full-backfill.sh > /var/log/ch-full-backfill.log 2>&1 < /dev/null &
#
# Env overrides: FROM TO WINDOW PAR FLUSH BUCKET CONFIG OPS CHADDR STATE.
set -uo pipefail

FROM=${FROM:-1}
TO=${TO:?set TO to the target tip ledger}
WINDOW=${WINDOW:-1000000}
PAR=${PAR:-8}
FLUSH=${FLUSH:-200}
BUCKET=${BUCKET:-galexie-archive}
CONFIG=${CONFIG:-/etc/stellarindex.toml}
OPS=${OPS:-/usr/local/bin/stellarindex-ops}
CHADDR=${CHADDR:-127.0.0.1:9300}
STATE=${STATE:-/var/lib/ch-backfill/done-windows.txt}

mkdir -p "$(dirname "$STATE")"
touch "$STATE"

# Optional ops-user credentials for clickhouse-client: env, never argv.
if [ -n "${STELLARINDEX_CLICKHOUSE_OPS_USER:-}" ]; then
  export CLICKHOUSE_USER="$STELLARINDEX_CLICKHOUSE_OPS_USER"
  export CLICKHOUSE_PASSWORD="${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD:-}"
fi

# window_recorded LO HI: $STATE holds LO with an end at or past HI. A line is
# "LO HI". A bare "LO" from an older run names no end, so it counts only for a
# full-width window whose successor is recorded too: that proves it ran to HI.
window_recorded() {
  awk -v lo="$1" -v hi="$2" -v nxt="$(($1 + WINDOW))" '
    $1 == lo && NF >= 2 && $2 + 0 >= hi + 0 { ok = 1 }
    $1 == lo && NF == 1 { bare = 1 }
    $1 == nxt { succ = 1 }
    END { exit !(ok || (bare && succ && hi + 0 == nxt - 1)) }' "$STATE"
}

# lake_ledgers LO HI: distinct ledgers the lake holds in the window.
lake_ledgers() {
  clickhouse-client --host "${CHADDR%:*}" --port "${CHADDR##*:}" \
    -q "SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN $1 AND $2"
}

echo "ch-full-backfill: [$FROM,$TO] window=$WINDOW parallel=$PAR flush=$FLUSH bucket=$BUCKET"
echo "ch-full-backfill: state=$STATE (already $(wc -l < "$STATE") windows done)"

started=$(date +%s)
w=$FROM
while [ "$w" -le "$TO" ]; do
  wto=$((w + WINDOW - 1))
  [ "$wto" -gt "$TO" ] && wto=$TO

  if window_recorded "$w" "$wto"; then
    # Ledger 1 does not exist; the lake starts at genesis ledger 2.
    want=$((wto - (w > 2 ? w : 2) + 1))
    if ! have=$(lake_ledgers "$w" "$wto") || ! [[ "$have" =~ ^[0-9]+$ ]]; then
      echo "ch-full-backfill: cannot read the lake to confirm window $w-$wto — stopping"
      exit 1
    fi
    if [ "$have" -ge "$want" ]; then
      echo "ch-full-backfill: skip window $w-$wto (done)"
      w=$((wto + 1))
      continue
    fi
    echo "ch-full-backfill: window $w-$wto is recorded done but the lake holds $have of $want ledgers — redoing"
  fi

  echo "=== window $w-$wto  ($(date -u +%FT%TZ)) ==="
  if "$OPS" ch-backfill -write -config "$CONFIG" -bucket "$BUCKET" \
        -ch-addr "$CHADDR" -from "$w" -to "$wto" -parallel "$PAR" -flush-every "$FLUSH"; then
    echo "$w $wto" >> "$STATE"
    elapsed=$(( $(date +%s) - started ))
    echo "ch-full-backfill: window $w-$wto DONE (total elapsed ${elapsed}s)"
  else
    rc=$?
    echo "ch-full-backfill: window $w-$wto FAILED (exit $rc) — stopping; re-run to resume"
    exit "$rc"
  fi
  w=$((wto + 1))
done

echo "ch-full-backfill: ALL WINDOWS COMPLETE [$FROM,$TO] in $(( $(date +%s) - started ))s"
