#!/bin/bash
# galexie-archive-tip-lag - alert source for a silently stalled galexie-archive
# (the ADR-0016 R1 durable full-mirror).
#
# WHY: the hourly galexie-archive-fill.timer does the catch-up; this is the
# defense-in-depth for that timer itself breaking silently (mc alias rotated,
# aws-public IAM change, MinIO mtime-poison deadlock), so we page within hours,
# not weeks. The metric is the ledger lag between the newest object in
# galexie-live and in galexie-archive; the Prometheus alert sits on top.
#
# Mechanics:
#   - Newest partition per bucket is the first row of `mc ls bucket/`: partition
#     names are reverse-hex prefixed, so lexically smallest = newest ledgers.
#   - Newest object in that partition is again the first row. Names are
#     `<reverse-hex>--<ledger>.xdr.zst` (ledgers_per_file=1) or
#     `<reverse-hex>--<start>-<end>.xdr.zst` (>1); a range contributes its <end>.
#   - lag = live_tip - archive_tip (ledgers); the .prom is written atomically.
#
# WHY both name forms: testnet is moving to ledgers_per_file=64, where EVERY
# object is a range name; a single-ledger-only regex would match nothing, report
# archive=0 and latch the alert forever (a permanently firing alert is as blind
# as none). The galexie SDK also spells the suffix `.xdr.zstd`, so `.zst` and
# `.zstd` are both accepted.
#
# Self-test (no mc alias needed): galexie-archive-tip-lag.sh --self-test
#
# Runtime: ~2-3s (two bucket listings + two partition listings, all small).

set -euo pipefail

# TEXTFILE_DIR is env-overridable so the fake-mc harness below can run
# hermetically (same idiom as ch-schema-drift.sh).
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/galexie_archive_tip_lag.prom"
TMP="$OUT.tmp.$$"

# ledger_from_object_name <name> — print the newest ledger held by a
# galexie object, or nothing if <name> is not a galexie object name.
#   <hex>--<ledger>.xdr.zst[d]        -> <ledger>
#   <hex>--<start>-<end>.xdr.zst[d]   -> <end>
# Pure-bash so `--self-test` can pin it without an mc alias.
ledger_from_object_name() {
  local name="$1"
  if [[ "$name" =~ ^[0-9A-Fa-f]+--([0-9]+)(-([0-9]+))?\.xdr\.zstd?$ ]]; then
    if [ -n "${BASH_REMATCH[3]}" ]; then
      echo "${BASH_REMATCH[3]}"
    else
      echo "${BASH_REMATCH[1]}"
    fi
  fi
}

newest_ledger() {
  # $1 = bucket name (e.g. local/galexie-live). Prints the newest ledger and
  # returns 0, or prints nothing and returns 1 when nothing was measured (failed
  # listing, no partition, no parseable object). Both buckets are non-empty on a
  # working node, so "nothing found" is a failed measurement, never ledger 0:
  # a 0 would render an unreadable bucket as "archive exactly at the live tip".
  #
  # Buffer `mc ls` into a variable BEFORE parsing: streaming into `awk ...; exit`
  # SIGPIPEs mc (141) and aborts the script under pipefail. Listings are tiny.
  # mc's stderr is left to the journal: it is the diagnosis.
  local bucket="$1" parts_raw objs_raw part name ledger
  parts_raw=$(mc ls "$bucket/") || return 1
  # Read from the variable, not a pipe: `awk ... exit` stops reading and `printf`
  # would take EPIPE on a listing larger than the 64 KiB pipe buffer.
  part=$(awk '/\/$/{print $NF; exit}' <<<"$parts_raw" | sed 's:/$::') || return 1
  [ -n "$part" ] || return 1
  objs_raw=$(mc ls "$bucket/$part/") || return 1
  # First row whose last column parses as an object name wins (mc ls sorts
  # newest-first here).
  while read -r name; do
    ledger=$(ledger_from_object_name "$name")
    if [ -n "$ledger" ]; then
      echo "$ledger"
      return 0
    fi
  done < <(printf '%s\n' "$objs_raw" | awk 'NF{print $NF}')
  return 1
}

# last_success_stamp — the previous file's success stamp, carried
# forward on a failed run so _metric_stale keeps ageing from the last
# real measurement instead of going absent (absence disarms it).
last_success_stamp() {
  local stamp
  [ -f "$OUT" ] || return 0
  stamp=$(awk '$1 == "galexie_archive_tip_lag_updated_seconds" {print $2; exit}' "$OUT") || return 0
  [[ "$stamp" =~ ^[0-9]+$ ]] && echo "$stamp"
  return 0
}

self_test() {
  # name -> expected ledger ("" = must not parse). Covers both forms,
  # both suffixes, and the non-object rows mc ls can surface.
  local fail=0 pass=0 name want got
  while IFS='|' read -r name want; do
    got=$(ledger_from_object_name "$name")
    if [ "$got" = "$want" ]; then
      pass=$((pass + 1))
      echo "ok   — $name -> '$got'"
    else
      fail=$((fail + 1))
      echo "FAIL — $name -> '$got', want '$want'"
    fi
  done <<'CASES'
FFFFFFFF--0.xdr.zst|0
FFFF05FF--116512.xdr.zst|116512
FFFE0C6E--127889.xdr.zst|127889
FFFFFFFF--448-511.xdr.zstd|511
FFFFFFFF--448-511.xdr.zst|511
FFFF05FF--116480-116543.xdr.zst|116543
FFFF05FF--116512.xdr.zstd|116512
FFFF0000--116512.xdr|
FFFF0000-116512.xdr.zst|
notes.txt|
FFFF0000--/|
CASES
  echo "galexie-archive-tip-lag self-test: ${pass} passed, ${fail} failed"
  [ "$fail" -eq 0 ]
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

mkdir -p "$TEXTFILE_DIR"

live="" archive=""
if ! live=$(newest_ledger local/galexie-live) || ! archive=$(newest_ledger local/galexie-archive); then
  echo "galexie-archive-tip-lag: could not read a tip (live='${live}' archive='${archive}'); publishing probe_success 0" >&2
  stamp=$(last_success_stamp)
  {
    printf '# HELP galexie_archive_tip_lag_probe_success 1 when both bucket tips were read and parsed on the last run, 0 when either was not (lag and tips are then omitted, never reported as 0).\n'
    printf '# TYPE galexie_archive_tip_lag_probe_success gauge\n'
    printf 'galexie_archive_tip_lag_probe_success 0\n'
    if [ -n "$stamp" ]; then
      printf '# HELP galexie_archive_tip_lag_updated_seconds Unix time of the most recent successful tip-lag computation.\n'
      printf '# TYPE galexie_archive_tip_lag_updated_seconds gauge\n'
      printf 'galexie_archive_tip_lag_updated_seconds %s\n' "$stamp"
    fi
  } > "$TMP"
  chmod 644 "$TMP"
  mv "$TMP" "$OUT"
  # A failed oneshot is what systemctl status shows.
  exit 1
fi

if [ "$live" -gt "$archive" ]; then
  lag=$((live - archive))
else
  lag=0
fi

cat > "$TMP" <<EOF
# HELP galexie_archive_tip_lag_probe_success 1 when both bucket tips were read and parsed on the last run, 0 when either was not (lag and tips are then omitted, never reported as 0).
# TYPE galexie_archive_tip_lag_probe_success gauge
galexie_archive_tip_lag_probe_success 1
# HELP galexie_archive_tip_ledger Newest ledger sequence present in galexie-archive (R1 durable full-mirror).
# TYPE galexie_archive_tip_ledger gauge
galexie_archive_tip_ledger $archive
# HELP galexie_live_tip_ledger Newest ledger sequence present in galexie-live (rolling appender).
# TYPE galexie_live_tip_ledger gauge
galexie_live_tip_ledger $live
# HELP galexie_archive_tip_lag_ledgers Ledger lag of galexie-archive behind galexie-live (live - archive). Defense-in-depth for the 23-day archive stall: hourly catch-up timer should keep this near zero; sustained drift = the timer / its dependencies have broken.
# TYPE galexie_archive_tip_lag_ledgers gauge
galexie_archive_tip_lag_ledgers $lag
# HELP galexie_archive_tip_lag_updated_seconds Unix time of the most recent successful tip-lag computation.
# TYPE galexie_archive_tip_lag_updated_seconds gauge
galexie_archive_tip_lag_updated_seconds $(date +%s)
EOF
chmod 644 "$TMP"
mv "$TMP" "$OUT"
