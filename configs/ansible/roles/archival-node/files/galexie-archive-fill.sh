#!/bin/bash
# Fill galexie-archive from the AWS public bucket, mtime-aware.
#
# mc mirror --overwrite=false errors on every object whose mtime differs
# between AWS and the dest (all objects earlier copied via mc cp), then
# deadlocks; it does not skip silently (mc 2025-08-13). So: missing partitions
# = AWS partitions - local partitions, mirrored in parallel with --skip-errors.
# Known partials (PARTIALS env var, newline/space separated) are deleted first.
#
# The partition-level diff is blind to a trailing-edge partial: a partition
# mirrored right after AWS published its first few ledgers counts as present
# and is never revisited. Phase 1b file-counts the latest PARTIAL_CHECK_WINDOW
# partitions and queues any local one with fewer files than AWS. PARTIAL_CHECK_WINDOW=0
# skips it. We do NOT walk the whole 25M-object bucket (slow under contention,
# blocks the fill); run verify-archive (Tier A + B) afterwards for the rest.
# See docs/operations/galexie-backfill.md "mc mirror gotcha".
#
# Identity: reads and the mirror use ARCHIVE_DEST's alias, which ansible sets to
# the bucket-scoped galexie-archive-writer (no delete), so the hourly run holds
# no MinIO admin credential. Only the operator-run PARTIALS delete uses
# ARCHIVE_DELETE_ALIAS, which must carry the delete authority the writer lacks.
set -euo pipefail

LOG=/var/log/galexie-mirror.log
PARALLEL="${PARALLEL:-8}"

# One run at a time (timer via run-heavy-job.sh, or an operator with PARTIALS):
# a second run would mirror from work lists the first is rewriting. The lock is
# this script's own file on an auto-allocated fd, so it does not collide with
# the wrapper's fd-9 lock. 75 = EX_TEMPFAIL, the wrapper's "declined" code.
LOCK=/run/lock/galexie-archive-fill.lock
exec {lock_fd}>"$LOCK"
if ! flock -n "$lock_fd"; then
  echo "galexie-archive-fill: another run holds $LOCK (fuser -v $LOCK); not starting, exit 75" >&2
  exit 75
fi
WORK=$(mktemp -d "${TMPDIR:-/tmp}/galexie-fill.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
: > "$WORK/rejected.txt"

# Hot floor (ADR-0027 trim): partitions ending BELOW it are trimmed on purpose
# (the cold tier serves them). Without this filter Phase 2 sees them as missing
# and re-pulls ~3.7 TB, making trim and fill adversaries. The floor is the higher
# of static ARCHIVE_HOT_FLOOR (/etc/default/galexie-archive-fill; 0 = none) and
# the cutoff compute-trim-cutoff.sh persists to ARCHIVE_HOT_FLOOR_FILE before
# every trim, so it rolls forward with the trim.
# shellcheck source=/dev/null  # ansible-rendered hot floor and mc aliases
[ -f /etc/default/galexie-archive-fill ] && . /etc/default/galexie-archive-fill
ARCHIVE_HOT_FLOOR="${ARCHIVE_HOT_FLOOR:-0}"
ARCHIVE_HOT_FLOOR_FILE=/var/lib/galexie-archive/hot-floor
if [ -e "$ARCHIVE_HOT_FLOOR_FILE" ]; then
  trim_floor=$(tr -d '[:space:]' < "$ARCHIVE_HOT_FLOOR_FILE")
  if ! [[ "$trim_floor" =~ ^[0-9]+$ ]]; then
    # Fail closed: without the floor, every trimmed partition looks missing.
    echo "galexie-archive-fill: FATAL — $ARCHIVE_HOT_FLOOR_FILE holds '$trim_floor', not a ledger" >&2
    exit 1
  fi
  if [ "$trim_floor" -gt "$ARCHIVE_HOT_FLOOR" ]; then
    ARCHIVE_HOT_FLOOR=$trim_floor
  fi
fi
PARTIAL_CHECK_WINDOW="${PARTIAL_CHECK_WINDOW:-4}"

# Destination as <mc-alias>/<bucket>[/<prefix>], e.g. a regional node filling
# a remote object store. It prefixes `mc rm --recursive --force`, so a value
# that could resolve to an alias or bucket root is refused before any mc call.
ARCHIVE_DEST="${ARCHIVE_DEST:-local/galexie-archive}"
if ! [[ "$ARCHIVE_DEST" =~ ^[A-Za-z0-9_-]+(/[A-Za-z0-9_.-]+)+$ ]] || [[ "/$ARCHIVE_DEST/" == */../* || "/$ARCHIVE_DEST/" == */./* ]]; then
  echo "galexie-archive-fill: FATAL — ARCHIVE_DEST='$ARCHIVE_DEST' is not <mc-alias>/<bucket>[/<prefix>]" >&2
  exit 1
fi
# The PARTIALS delete reaches the same bucket path through this alias.
ARCHIVE_DELETE_ALIAS="${ARCHIVE_DELETE_ALIAS:-${ARCHIVE_DEST%%/*}}"
if ! [[ "$ARCHIVE_DELETE_ALIAS" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "galexie-archive-fill: FATAL — ARCHIVE_DELETE_ALIAS='$ARCHIVE_DELETE_ALIAS' is not an mc alias name" >&2
  exit 1
fi

# Known partials: pass via env var (newline- or space-separated), e.g.
#   PARTIALS=$'FC49CDFF--62272000-62335999\nXYZ--...' galexie-archive-fill
# Partition names should NOT have trailing slashes.
PARTIALS_INPUT="${PARTIALS:-}"

# aws_ls: list a prefix on the public AWS bucket, 3 attempts with backoff. S3
# intermittently answers PermanentRedirect even on the regional endpoint; under
# `set -e` that killed the run silently, hiding real failures among AWS hiccups.
# Fails loudly on stderr so journald carries the reason.
aws_ls() {  # $1=prefix (after aws-public/), remaining args passed to mc ls
  local prefix="$1"; shift
  local attempt out
  for attempt in 1 2 3; do
    if out=$(mc ls "$@" "aws-public/${prefix}" 2>"$WORK/awsls.err"); then
      printf '%s\n' "$out"; return 0
    fi
    echo "galexie-archive-fill: aws listing attempt ${attempt}/3 failed for ${prefix}: $(tail -1 "$WORK/awsls.err" | cut -c1-160)" >&2
    sleep $((attempt * 5))
  done
  echo "galexie-archive-fill: FATAL — AWS listing of ${prefix} failed 3 times; not a local fault" >&2
  return 1
}

# Partition names come from the upstream bucket listing or the operator and
# become mc paths in a script run as root, so only this shape is accepted.
PARTITION_RE='^[0-9A-F]{8}--[0-9]+-[0-9]+$'

# valid_partitions: stdin names to stdout, keeping only Galexie partition names.
# Empty lines and the .config.json marker are dropped; others go to
# rejected.txt for report_rejected.
valid_partitions() {
  local p
  while IFS= read -r p; do
    case "$p" in '' | .config.json) continue ;; esac
    if [[ "$p" =~ $PARTITION_RE ]]; then
      printf '%s\n' "$p"
    else
      printf '%q\n' "$p" >> "$WORK/rejected.txt"
    fi
  done
}

# aws_partitions — the validated, sorted partition names on the AWS bucket.
aws_partitions() {
  aws_ls aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ \
    | awk '{print $NF}' | sed 's:/$::' | valid_partitions | sort
}

# report_rejected — exit 1, naming them, if any name was refused so far.
report_rejected() {
  if [ -s "$WORK/rejected.txt" ]; then
    sort -u "$WORK/rejected.txt" > "$WORK/rejected.uniq.txt"
    echo "galexie-archive-fill: FATAL — refused $(wc -l < "$WORK/rejected.uniq.txt") name(s) that are not Galexie partitions:" | tee -a "$LOG" >&2
    tee -a "$LOG" < "$WORK/rejected.uniq.txt" >&2
    exit 1
  fi
}

# A broken writer alias must fail the run: Phase 1b discards listing
# errors and would count every partition as empty.
if ! mc ls "$ARCHIVE_DEST/" >/dev/null 2>&1; then
  echo "galexie-archive-fill: FATAL — cannot list '$ARCHIVE_DEST' (alias unset, wrong secret, or no policy)" | tee -a "$LOG" >&2
  exit 1
fi

if [ -n "$PARTIALS_INPUT" ]; then
  echo "=== $(date -Iseconds) Phase 1: delete known partials ===" | tee -a "$LOG"
  # Validate the whole list before deleting anything.
  printf '%s\n' "$PARTIALS_INPUT" | tr ' ' '\n' | valid_partitions > "$WORK/partials.txt"
  report_rejected
  # The rm below swallows errors, so an unresolvable alias would look like success.
  if ! mc alias list "$ARCHIVE_DELETE_ALIAS" >/dev/null 2>&1; then
    echo "galexie-archive-fill: FATAL — delete alias '$ARCHIVE_DELETE_ALIAS' is not configured; nothing deleted" | tee -a "$LOG" >&2
    exit 1
  fi
  while read -r p; do
    echo "  rm: $p" | tee -a "$LOG"
    mc rm --recursive --force "$ARCHIVE_DELETE_ALIAS/${ARCHIVE_DEST#*/}/$p/" >/dev/null 2>&1 || true
  done < "$WORK/partials.txt"
fi

# Phase 1b: sample the latest PARTIAL_CHECK_WINDOW AWS partitions and compare
# file counts to local; Phase 2's presence-only diff would skip a 416/64000
# partition. One recursive `mc ls` per sampled partition, never the full bucket.
if [ "$PARTIAL_CHECK_WINDOW" -gt 0 ]; then
  echo "=== $(date -Iseconds) Phase 1b: scan latest $PARTIAL_CHECK_WINDOW partitions for partials ===" | tee -a "$LOG"
  : > "$WORK/incomplete.txt"
  # Partition names have a DESCENDING-hex prefix, so alphabetical order puts the
  # latest partition first and `head -N` yields the latest N (aws_partitions drops
  # .config.json first).
  #
  # `sort` writes to a FILE that `head` reads, not `sort | head`: under pipefail
  # head closes the pipe, systemd's IgnoreSIGPIPE=yes makes sort fail with EPIPE
  # (exit 2) once the listing exceeds the 64 KiB pipe buffer, failing the unit on
  # ~1/3 of runs; the retry helper cannot help as the AWS call itself succeeded.
  aws_partitions > "$WORK/partitions.txt"
  head -n "$PARTIAL_CHECK_WINDOW" "$WORK/partitions.txt" \
    > "$WORK/tail.txt"
  while read -r p; do
    [ -z "$p" ] && continue
    aws_n=$(aws_ls "aws-public-blockchain/v1.1/stellar/ledgers/pubnet/$p/" --recursive | wc -l)
    local_n=$(mc ls --recursive "$ARCHIVE_DEST/$p/" 2>/dev/null | wc -l)
    if [ "$local_n" -gt 0 ] && [ "$local_n" -lt "$aws_n" ]; then
      # Queue for Phase 3 instead of deleting: `mc mirror` is incremental. The TIP
      # partition is partial by definition, so deleting made every hourly run
      # re-download it (~85 GiB/day egress). Phase 2's `comm -23` is presence-only, so
      # an incomplete local partition needs this queueing to be revisited.
      echo "  incomplete: $p  local=$local_n  aws=$aws_n  -> queued for incremental mirror" | tee -a "$LOG"
      echo "$p" >> "$WORK/incomplete.txt"
    else
      echo "  ok: $p  local=$local_n  aws=$aws_n" | tee -a "$LOG"
    fi
  done < "$WORK/tail.txt"
fi

echo "=== $(date -Iseconds) Phase 2: build needs-work list ===" | tee -a "$LOG"
aws_partitions > "$WORK/aws.txt"
mc ls "$ARCHIVE_DEST/" \
  | awk '{print $NF}' | sed 's:/$::' | sort > "$WORK/local.txt"
comm -23 "$WORK/aws.txt" "$WORK/local.txt" \
  > "$WORK/missing.txt"
# needs-work = MISSING + INCOMPLETE (Phase 1b), unioned directly and mirrored
# incrementally.
touch "$WORK/incomplete.txt"
sort -u "$WORK/missing.txt" "$WORK/incomplete.txt" \
  > "$WORK/needs-work.unfloored.txt"
# Drop partitions entirely below the hot floor — those are trimmed on
# purpose, not missing. A partition STRADDLING the floor stays eligible.
below=0
: > "$WORK/needs-work.txt"
while read -r p; do
  [ -z "$p" ] && continue
  end=${p##*-}
  if [[ "$end" =~ ^[0-9]+$ ]] && [ "$end" -lt "$ARCHIVE_HOT_FLOOR" ]; then
    below=$((below+1)); continue
  fi
  echo "$p" >> "$WORK/needs-work.txt"
done < "$WORK/needs-work.unfloored.txt"
echo "  below hot floor ($ARCHIVE_HOT_FLOOR), intentionally not mirrored: $below" | tee -a "$LOG"
echo "  AWS partitions: $(wc -l < "$WORK/aws.txt")" | tee -a "$LOG"
echo "  local partitions present: $(wc -l < "$WORK/local.txt")" | tee -a "$LOG"
echo "  missing entirely: $(wc -l < "$WORK/missing.txt")" | tee -a "$LOG"
echo "  incomplete (queued by Phase 1b): $(wc -l < "$WORK/incomplete.txt")" | tee -a "$LOG"
echo "  needs work (total): $(wc -l < "$WORK/needs-work.txt")" | tee -a "$LOG"

echo "=== $(date -Iseconds) Phase 3: mirror per-partition (parallel=$PARALLEL) ===" | tee -a "$LOG"
# Partitions here are missing or incomplete; `mc mirror` copies only absent
# objects either way. --skip-errors is belt-and-braces. Parallel=8 is
# conservative: the link saturates at ~100 MB/s.
# The partition reaches the worker as "$4", never as script text (`xargs -I {}`
# splices it into the source bash). -r: an empty list makes no call, where one
# partition-less call would mirror the whole bucket. The worker re-checks the
# name so a malformed one can never become a bucket-root path.
# shellcheck disable=SC2016  # expanded by the per-partition bash
xargs -r -a "$WORK/needs-work.txt" -d '\n' -P "$PARALLEL" -n 1 bash -c '
  log=$1 re=$2 dest=$3 p=$4
  if ! [[ "$p" =~ $re ]]; then
    echo "galexie-archive-fill: refusing to mirror non-partition name: ${p@Q}" >&2
    exit 1
  fi
  echo "==> $(date -Iseconds) $p" >> "$log"
  mc mirror --skip-errors \
    "aws-public/aws-public-blockchain/v1.1/stellar/ledgers/pubnet/$p/" \
    "$dest/$p/" >> "$log" 2>&1
  echo "<== $(date -Iseconds) $p" >> "$log"
' mirror-partition "$LOG" "$PARTITION_RE" "$ARCHIVE_DEST"

report_rejected

echo "=== $(date -Iseconds) Done ===" | tee -a "$LOG"
echo "Next: stellarindex-ops verify-archive -tier all -from 2 -to <last-mirrored-ledger>" | tee -a "$LOG"
