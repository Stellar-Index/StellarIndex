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
# F-0158 (2026-05-27) auto-partial detection: the partition-level set
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
set -euo pipefail

LOG=/var/log/galexie-mirror.log
PARALLEL="${PARALLEL:-8}"

# Hot floor (2026-07-25, ADR-0027 trim): partitions whose ledger range
# ends BELOW this are deliberately trimmed from local storage — the cold
# tier (aws-public-blockchain) serves them. Without this filter, Phase 2
# sees every trimmed partition as "missing" and re-downloads the lot,
# which would make trim and fill adversaries (~3.7 TB re-pulled).
# Sourced from /etc/default/galexie-archive-fill (ansible-templated from
# stellarindex_archive_hot_floor); 0 = no floor, mirror everything.
[ -f /etc/default/galexie-archive-fill ] && . /etc/default/galexie-archive-fill
ARCHIVE_HOT_FLOOR="${ARCHIVE_HOT_FLOOR:-0}"
PARTIAL_CHECK_WINDOW="${PARTIAL_CHECK_WINDOW:-4}"

# Known partials: pass via env var (newline- or space-separated), e.g.
#   PARTIALS=$'FC49CDFF--62272000-62335999\nXYZ--...' galexie-archive-fill
# Partition names should NOT have trailing slashes.
PARTIALS_INPUT="${PARTIALS:-}"

# aws_ls — list a prefix on the public AWS bucket with bounded retries (#475,
# 2026-09-02). The S3 listing intermittently answers PermanentRedirect
# ("must be addressed using the specified endpoint") even against the
# regional endpoint; under `set -e` one such reply killed the whole run with
# no output — the unit alternated clean/failed hourly and a REAL failure was
# indistinguishable from AWS hiccups. Three attempts with backoff, then fail
# LOUDLY with the reason on stderr so journald carries it.
aws_ls() {  # $1=prefix (after aws-public/), remaining args passed to mc ls
  local prefix="$1"; shift
  local attempt out
  for attempt in 1 2 3; do
    if out=$(mc ls "$@" "aws-public/${prefix}" 2>/tmp/galexie-fill.awsls.err); then
      printf '%s\n' "$out"; return 0
    fi
    echo "galexie-archive-fill: aws listing attempt ${attempt}/3 failed for ${prefix}: $(tail -1 /tmp/galexie-fill.awsls.err | cut -c1-160)" >&2
    sleep $((attempt * 5))
  done
  echo "galexie-archive-fill: FATAL — AWS listing of ${prefix} failed 3 times; not a local fault" >&2
  return 1
}

if [ -n "$PARTIALS_INPUT" ]; then
  echo "=== $(date -Iseconds) Phase 1: delete known partials ===" | tee -a "$LOG"
  echo "$PARTIALS_INPUT" | tr ' ' '\n' | grep -v '^$' | while read -r p; do
    echo "  rm: $p" | tee -a "$LOG"
    mc rm --recursive --force "local/galexie-archive/$p/" >/dev/null 2>&1 || true
  done
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
  : > /tmp/galexie-fill.incomplete.txt
  # Galexie partitions are named with a DESCENDING-hex prefix so that
  # alphabetical sort puts the most recent (highest-ledger) partition
  # FIRST. e.g. FC42F7FF--62720000-... sorts BEFORE FFFFFFFF--0-63999
  # (genesis). `head -N` therefore gives us the latest N partitions —
  # filter `.config.json` (the bucket marker file) out first.
  aws_ls aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ \
    | awk '{print $NF}' | sed 's:/$::' | { grep -v '^\.' || true; } | sort \
    | head -n "$PARTIAL_CHECK_WINDOW" \
    > /tmp/galexie-fill.tail.txt
  while read -r p; do
    [ -z "$p" ] && continue
    aws_n=$(aws_ls "aws-public-blockchain/v1.1/stellar/ledgers/pubnet/$p/" --recursive | wc -l)
    local_n=$(mc ls --recursive "local/galexie-archive/$p/" 2>/dev/null | wc -l)
    if [ "$local_n" -gt 0 ] && [ "$local_n" -lt "$aws_n" ]; then
      # Queue it for Phase 3 instead of DELETING it. `mc mirror` is
      # already incremental — it copies only the objects absent from the
      # destination — so the delete bought nothing and cost everything.
      #
      # 2026-07-25: the delete made this pathological. The TIP partition
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
      echo "$p" >> /tmp/galexie-fill.incomplete.txt
    else
      echo "  ok: $p  local=$local_n  aws=$aws_n" | tee -a "$LOG"
    fi
  done < /tmp/galexie-fill.tail.txt
fi

echo "=== $(date -Iseconds) Phase 2: build needs-work list ===" | tee -a "$LOG"
aws_ls aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ \
  | awk '{print $NF}' | sed 's:/$::' | sort > /tmp/galexie-fill.aws.txt
mc ls local/galexie-archive/ \
  | awk '{print $NF}' | sed 's:/$::' | sort > /tmp/galexie-fill.local.txt
comm -23 /tmp/galexie-fill.aws.txt /tmp/galexie-fill.local.txt \
  > /tmp/galexie-fill.missing.txt
# needs-work = MISSING (never mirrored) + INCOMPLETE (present but short,
# from Phase 1b). Before 2026-07-25 the incomplete set was handled by
# deleting those partitions so they showed up as missing here; they are
# now unioned in directly and mirrored incrementally.
touch /tmp/galexie-fill.incomplete.txt
sort -u /tmp/galexie-fill.missing.txt /tmp/galexie-fill.incomplete.txt \
  > /tmp/galexie-fill.needs-work.unfloored.txt
# Drop partitions entirely below the hot floor — those are trimmed on
# purpose, not missing. A partition STRADDLING the floor stays eligible.
below=0
: > /tmp/galexie-fill.needs-work.txt
while read -r p; do
  [ -z "$p" ] && continue
  end=${p##*-}
  if [[ "$end" =~ ^[0-9]+$ ]] && [ "$end" -lt "$ARCHIVE_HOT_FLOOR" ]; then
    below=$((below+1)); continue
  fi
  echo "$p" >> /tmp/galexie-fill.needs-work.txt
done < /tmp/galexie-fill.needs-work.unfloored.txt
echo "  below hot floor ($ARCHIVE_HOT_FLOOR), intentionally not mirrored: $below" | tee -a "$LOG"
echo "  AWS partitions: $(wc -l < /tmp/galexie-fill.aws.txt)" | tee -a "$LOG"
echo "  local partitions present: $(wc -l < /tmp/galexie-fill.local.txt)" | tee -a "$LOG"
echo "  missing entirely: $(wc -l < /tmp/galexie-fill.missing.txt)" | tee -a "$LOG"
echo "  incomplete (queued by Phase 1b): $(wc -l < /tmp/galexie-fill.incomplete.txt)" | tee -a "$LOG"
echo "  needs work (total): $(wc -l < /tmp/galexie-fill.needs-work.txt)" | tee -a "$LOG"

echo "=== $(date -Iseconds) Phase 3: mirror per-partition (parallel=$PARALLEL) ===" | tee -a "$LOG"
# Partitions here are either fully missing or incomplete; `mc mirror`
# copies only the objects absent from the destination in both cases, so
# an incomplete partition costs one listing plus the genuinely-missing
# objects rather than a full re-download. --skip-errors is belt-and-braces.
# Parallel=8 is conservative — 100 MB/s observed link saturation, so
# more workers won't help.
xargs -a /tmp/galexie-fill.needs-work.txt -P "$PARALLEL" -I {} bash -c '
  echo "==> $(date -Iseconds) {}" >> "'"$LOG"'"
  mc mirror --skip-errors \
    "aws-public/aws-public-blockchain/v1.1/stellar/ledgers/pubnet/{}/" \
    "local/galexie-archive/{}/" >> "'"$LOG"'" 2>&1
  echo "<== $(date -Iseconds) {}" >> "'"$LOG"'"
'

echo "=== $(date -Iseconds) Done ===" | tee -a "$LOG"
echo "Next: stellarindex-ops verify-archive -tier all -from 2 -to <last-mirrored-ledger>" | tee -a "$LOG"
