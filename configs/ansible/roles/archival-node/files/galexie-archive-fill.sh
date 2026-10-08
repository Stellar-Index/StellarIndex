#!/bin/bash
# Fill galexie-archive from AWS public bucket — mtime-aware approach.
#
# WHY: mc mirror --overwrite=false errors on every object whose mtime
# differs between source AWS and dest MinIO (which is every object that
# was previously copied via mc cp), then deadlocks. The runbook claim
# that --overwrite=false skips silently is wrong as of mc 2025-08-13.
#
# Strategy: compute (AWS partitions − local partitions) = missing
# partition set, mirror each in parallel with --skip-errors. For known
# partial partitions (passed via PARTIALS env var or stdin), delete
# them first so mirror sees them as missing and copies cleanly.
#
# Auto-partial detection: the partition-level set
# diff has a trailing-edge blind spot. When AWS first publishes a new
# partition, only the first few ledgers exist; we mirror those, mark
# the partition "present", then never revisit it — leaving it stuck at
# a few hundred of 64,000 files. Phase 1b file-counts the latest
# PARTIAL_CHECK_WINDOW partitions and treats any local partition with
# fewer files than AWS (and AWS itself ≥ partial threshold) as a
# partial. Full-bucket walk is still avoided — only the tail window
# is sampled. Set PARTIAL_CHECK_WINDOW=0 to skip if you ever need the
# old behaviour.
#
# We deliberately do NOT walk the entire 25M-object bucket to detect
# all partial partitions — that listing is slow under contention and
# blocks the actual fill work. Run verify-archive (Tier A + B) after
# this script completes; any remaining partials surface there.
#
# See docs/operations/galexie-backfill.md "mc mirror gotcha" for the
# failure mode this script works around.
#
# Identity: every read and the mirror go through ARCHIVE_DEST's alias, which
# ansible sets to the bucket-scoped galexie-archive-writer (no delete), so the
# hourly run holds no MinIO admin credential. Only the operator-run PARTIALS
# delete uses ARCHIVE_DELETE_ALIAS, named separately because it must carry
# delete authority the writer deliberately lacks.
set -euo pipefail

LOG=/var/log/galexie-mirror.log
PARALLEL="${PARALLEL:-8}"

# One run at a time, whoever the caller is: the timer runs this under
# run-heavy-job.sh, an operator runs it by hand with PARTIALS=..., and a
# second run would mirror from work lists the first is rewriting. The
# lock is this script's own file on an auto-allocated fd, so it neither
# collides with nor closes the wrapper's fd-9 lock. 75 = EX_TEMPFAIL,
# the wrapper's "declined to run" code.
LOCK=/run/lock/galexie-archive-fill.lock
exec {lock_fd}>"$LOCK"
if ! flock -n "$lock_fd"; then
  echo "galexie-archive-fill: another run holds $LOCK (fuser -v $LOCK); not starting, exit 75" >&2
  exit 75
fi
WORK=$(mktemp -d "${TMPDIR:-/tmp}/galexie-fill.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
: > "$WORK/rejected.txt"

# Hot floor (ADR-0027 trim): partitions whose ledger range ends BELOW
# this are deliberately trimmed from local storage — the cold tier
# (aws-public-blockchain) serves them. Without this filter, Phase 2 sees
# every trimmed partition as "missing" and re-downloads the lot, making
# trim and fill adversaries (~3.7 TB re-pulled). The floor is the higher
# of the static ARCHIVE_HOT_FLOOR (/etc/default/galexie-archive-fill,
# ansible's stellarindex_archive_hot_floor; 0 = none) and the cutoff the
# trim itself last used, which compute-trim-cutoff.sh persists to
# ARCHIVE_HOT_FLOOR_FILE before every trim — so the floor rolls
# forward with the trim instead of trailing it.
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

# aws_ls — list a prefix on the public AWS bucket with bounded retries.
# The S3 listing intermittently answers PermanentRedirect
# ("must be addressed using the specified endpoint") even against the
# regional endpoint; under `set -e` one such reply killed the whole run with
# no output — the unit alternated clean/failed hourly and a REAL failure was
# indistinguishable from AWS hiccups. Three attempts with backoff, then fail
# LOUDLY with the reason on stderr so journald carries it.
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

# valid_partitions — stdin names to stdout, keeping only Galexie partition
# names (FC42F7FF--62720000-62783999). Empty lines and the bucket's
# .config.json marker are dropped; anything else is recorded in
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

# Phase 1b — auto-detect trailing-edge partials by sampling the latest
# PARTIAL_CHECK_WINDOW partitions on AWS and comparing file counts to
# local. This is the F-0158 fix: a partition with 416/64000 files
# present locally would otherwise be silently skipped by the Phase 2
# partition-level set diff. The recursive `mc ls` per partition costs
# one round-trip per partition we check — bounded by the window size,
# never the full bucket.
if [ "$PARTIAL_CHECK_WINDOW" -gt 0 ]; then
  echo "=== $(date -Iseconds) Phase 1b: scan latest $PARTIAL_CHECK_WINDOW partitions for partials ===" | tee -a "$LOG"
  : > "$WORK/incomplete.txt"
  # Galexie partitions are named with a DESCENDING-hex prefix so that
  # alphabetical sort puts the most recent (highest-ledger) partition
  # FIRST. e.g. FC42F7FF--62720000-... sorts BEFORE FFFFFFFF--0-63999
  # (genesis). `head -N` therefore gives us the latest N partitions —
  # aws_partitions drops `.config.json` (the bucket marker file) first.
  #
  # `sort` writes to a FILE and `head` reads it back, rather than the
  # obvious `sort | head -n N`. Under `set -euo pipefail` that pipeline
  # is a coin flip: `head` exits after N lines and closes the pipe,
  # systemd's default IgnoreSIGPIPE=yes turns the signal into EPIPE, and
  # `sort` prints "write failed: 'standard output': Broken pipe" and
  # exits 2 — which pipefail promotes to the pipeline's status and set -e
  # turns into a failed unit. It only bites when the listing exceeds the
  # 64 KiB pipe buffer, so it fired on roughly a third of runs and looked
  # random (it persisted past the retry helper — which cannot help,
  # because the AWS call itself succeeded).
  aws_partitions > "$WORK/partitions.txt"
  head -n "$PARTIAL_CHECK_WINDOW" "$WORK/partitions.txt" \
    > "$WORK/tail.txt"
  while read -r p; do
    [ -z "$p" ] && continue
    aws_n=$(aws_ls "aws-public-blockchain/v1.1/stellar/ledgers/pubnet/$p/" --recursive | wc -l)
    local_n=$(mc ls --recursive "$ARCHIVE_DEST/$p/" 2>/dev/null | wc -l)
    if [ "$local_n" -gt 0 ] && [ "$local_n" -lt "$aws_n" ]; then
      # Queue it for Phase 3 instead of DELETING it. `mc mirror` is
      # already incremental — it copies only the objects absent from the
      # destination — so the delete bought nothing and cost everything.
      #
      # The delete made this pathological. The TIP partition
      # is partial BY DEFINITION (it is the one currently filling), so
      # `local_n < aws_n` is permanently true for it and every hourly run
      # deleted and re-downloaded the whole thing. Measured over 62h:
      # 51 of 63 runs hit this branch, ~4.3 GiB re-pulled each time —
      # roughly 85 GiB/day of AWS egress to re-fetch data we already had,
      # rising toward ~11 GiB/run as the partition fills to 64,000 files.
      #
      # The F-0158 bug this branch exists for is REAL and still fixed:
      # Phase 2's `comm -23` is a presence-only set diff, so a partition
      # that exists locally but is incomplete is never revisited. Adding
      # it to the needs-work list closes that hole without the delete.
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
# needs-work = MISSING (never mirrored) + INCOMPLETE (present but short,
# from Phase 1b). The incomplete set is unioned in directly and mirrored
# incrementally rather than deleted so it shows up as missing here.
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
# Partitions here are either fully missing or incomplete; `mc mirror`
# copies only the objects absent from the destination in both cases, so
# an incomplete partition costs one listing plus the genuinely-missing
# objects rather than a full re-download. --skip-errors is belt-and-braces.
# Parallel=8 is conservative — 100 MB/s observed link saturation, so
# more workers won't help.
# The partition reaches the worker as "$4", never as script text: `xargs -I
# {}` splices it into the source bash then parses. -r: an empty list makes
# no call, where one call with no partition would mirror the whole bucket.
# The worker re-checks the name so an empty or malformed one can never
# become a bucket-root path.
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
