#!/bin/bash
# galexie-archive-tip-lag — defense-in-depth alert source for #26.
#
# WHY: #26 was a 23-day silent stall of galexie-archive (the
# ADR-0016 R1 durable full-mirror). The recurrence fix is the
# hourly galexie-archive-fill.timer; this script is the
# defense-in-depth — if that timer itself silently breaks (mc
# alias rotated, aws-public IAM change, MinIO mtime-poison
# deadlock per "mc mirror gotcha"), we want to PAGE within hours,
# not weeks. The metric is the lag in ledgers between the newest
# object in galexie-live and the newest in galexie-archive; the
# Prometheus alert sits on top.
#
# Mechanics:
#   - List the newest partition in each bucket (mc ls default sort
#     surfaces it first — partition names are reverse-hex prefixed,
#     so lexically largest = oldest ledgers; lexically smallest =
#     newest ledger range. The first row of `mc ls bucket/` is the
#     newest partition.)
#   - List the newest object inside that partition; object names
#     are `<reverse-hex>--<ledger>.xdr.zst` (ledgers_per_file=1) or
#     `<reverse-hex>--<start>-<end>.xdr.zst` (ledgers_per_file>1),
#     so again the first row is the newest ledger / ledger range. A
#     range object contributes its <end> ledger.
#
# WHY both name forms (2026-08-28): the parser originally matched
# only the single-ledger name. Testnet is moving to
# ledgers_per_file=64, under which EVERY object is a range name —
# the old regex would match nothing, report archive=0, and latch the
# tip-lag alert forever (a permanently-firing alert is as blind as
# no alert). The galexie SDK schema also spells the suffix
# `.xdr.zstd`, so both `.zst` and `.zstd` are accepted.
#
# Self-test (no mc alias needed):
#   galexie-archive-tip-lag.sh --self-test
#   - Compute lag = live_tip - archive_tip (ledgers).
#   - Write the textfile-collector .prom atomically.
#
# Runtime: ~2-3 seconds (two `mc ls bucket/` + two `mc ls
# bucket/partition/` calls — small listings, not the whole bucket).

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
  # $1 = bucket name (e.g. local/galexie-live).
  #
  # Implementation note: buffer `mc ls` into a variable BEFORE
  # parsing. Streaming `mc ls | awk '...; exit'` causes awk to
  # close the pipe on its first match; mc ls gets SIGPIPE (status
  # 141) and under `set -o pipefail` the whole script aborts. The
  # listings here are tiny (one bucket's partition list, or one
  # partition's first page of objects), so buffering is free.
  local bucket="$1" parts_raw objs_raw part name ledger
  parts_raw=$(mc ls "$bucket/" 2>/dev/null) || true
  part=$(printf '%s\n' "$parts_raw" | awk '/\/$/{print $NF; exit}' | sed 's:/$::') || true
  if [ -z "$part" ]; then
    echo "0"
    return
  fi
  objs_raw=$(mc ls "$bucket/$part/" 2>/dev/null) || true
  # First row whose last column parses as an object name wins (mc ls
  # sorts newest-first here, same as before).
  while read -r name; do
    ledger=$(ledger_from_object_name "$name")
    if [ -n "$ledger" ]; then
      echo "$ledger"
      return
    fi
  done < <(printf '%s\n' "$objs_raw" | awk 'NF{print $NF}')
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

live=$(newest_ledger local/galexie-live)
archive=$(newest_ledger local/galexie-archive)

# Guard: numeric.
[[ "$live" =~ ^[0-9]+$ ]] || live=0
[[ "$archive" =~ ^[0-9]+$ ]] || archive=0

if [ "$live" -gt "$archive" ]; then
  lag=$((live - archive))
else
  lag=0
fi

mkdir -p "$TEXTFILE_DIR"
cat > "$TMP" <<EOF
# HELP galexie_archive_tip_ledger Newest ledger sequence present in galexie-archive (R1 durable full-mirror).
# TYPE galexie_archive_tip_ledger gauge
galexie_archive_tip_ledger $archive
# HELP galexie_live_tip_ledger Newest ledger sequence present in galexie-live (rolling appender).
# TYPE galexie_live_tip_ledger gauge
galexie_live_tip_ledger $live
# HELP galexie_archive_tip_lag_ledgers Ledger lag of galexie-archive behind galexie-live (live - archive). Defense-in-depth for #26: hourly catch-up timer should keep this near zero; sustained drift = the timer / its dependencies have broken.
# TYPE galexie_archive_tip_lag_ledgers gauge
galexie_archive_tip_lag_ledgers $lag
# HELP galexie_archive_tip_lag_updated_seconds Unix time of the most recent successful tip-lag computation.
# TYPE galexie_archive_tip_lag_updated_seconds gauge
galexie_archive_tip_lag_updated_seconds $(date +%s)
EOF
chmod 644 "$TMP"
mv "$TMP" "$OUT"
