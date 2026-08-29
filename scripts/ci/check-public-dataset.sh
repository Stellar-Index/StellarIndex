#!/usr/bin/env bash
# check-public-dataset.sh — AWS Public Blockchain dataset drift tripwire
# (backup-restore-6, audit 2026-08-29; ADR-0043 amendment 2026-08-29).
#
# r1's own galexie-archive was capacity-trimmed below ledger 49,984,000
# on 2026-07-26 (ADR-0027 hot floor; galexie-archive-trim deletes only
# after the AWS object HEADs OK). So the "second independent raw-LCM
# archive" ADR-0043 relies on for deep-history re-derive is
# s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ — a dataset
# published by the AWS Open Data Sponsorship program that we do NOT
# control. Nothing watched it. This script is the decision core for
# .github/workflows/public-dataset-check.yml: it takes the bucket's
# top-level partition listing + its .config.json manifest and asserts
# the dataset still has the shape every consumer in this repo assumes
# (galexie-archive-fill.sh, galexie-archive-trim, ADR-0027 cold reads).
#
# Assertions (any failure → exit 1, all are reported, first failure does
# not short-circuit the rest):
#   1. manifest unchanged — networkPassphrase pubnet, ledgersPerBatch 1,
#      batchesPerPartition 64000, compression zstd.
#   2. every partition prefix is `HEX--start-end` with end-start+1 ==
#      64000, start aligned to 64000, and HEX == 0xFFFFFFFF - start
#      (the descending-hex sort key galexie names partitions with).
#   3. contiguous coverage from ledger 0 to the last partition's end
#      (no gap, no overlap, no duplicate start).
#   4. our trimmed range [TRIM_LO, TRIM_HI] is fully inside that
#      contiguous coverage — this is the range r1 can no longer serve
#      from its own storage.
#   5. the last partition's end >= tip - 2 partitions, where tip comes
#      from PUBLIC_DATASET_TIP (or Horizon's history_latest_ledger when
#      unset). An unreachable tip source SKIPS this assertion with a
#      warning — a Horizon outage is not dataset drift.
#
# Inputs (all optional; the workflow sets the FIXTURE-less live path):
#   PUBLIC_DATASET_LISTING  path to a file of partition prefixes, one per
#                           line — either bare (`FFFFFFFF--0-63999`) or
#                           raw `aws s3 ls` lines (`PRE FFFFFFFF--0-63999/`).
#                           Unset → the script runs `aws s3 ls
#                           --no-sign-request` itself.
#   PUBLIC_DATASET_MANIFEST path to the .config.json. Unset → fetched.
#   PUBLIC_DATASET_TIP      integer ledger tip. Unset → Horizon.
#   TRIM_LO / TRIM_HI       trimmed range (default 64000 / 49983999 —
#                           the [genesis-chunk, hot-floor) window from
#                           docs/architecture/multi-region-ha.md §5).
#   GITHUB_OUTPUT           when set, partition_count= / last_partition=
#                           / tip= / verdict= are appended for the
#                           workflow's step summary.
#
# Exit code: 0 = dataset intact, 1 = DRIFT. Report on stdout either way.
set -euo pipefail

BUCKET_PREFIX="s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
PARTITION_LEDGERS=64000
TRIM_LO="${TRIM_LO:-64000}"
TRIM_HI="${TRIM_HI:-49983999}"
TIP_SLACK_PARTITIONS=2
EXPECT_PASSPHRASE="Public Global Stellar Network ; September 2015"
HORIZON_URL="${HORIZON_URL:-https://horizon.stellar.org/}"

problems=0
problem() {
  echo "public-dataset: DRIFT — $*"
  problems=$((problems + 1))
}

# ── 1. Manifest ──
if [ -n "${PUBLIC_DATASET_MANIFEST:-}" ]; then
  manifest="$(cat "$PUBLIC_DATASET_MANIFEST")"
else
  manifest="$(aws s3 cp --no-sign-request "$BUCKET_PREFIX/.config.json" -)"
fi
if ! printf '%s' "$manifest" | jq -e . >/dev/null 2>&1; then
  problem "manifest .config.json is not valid JSON: $(printf '%s' "$manifest" | head -c 200)"
else
  m_pass="$(printf '%s' "$manifest" | jq -r '.networkPassphrase // ""')"
  m_lpb="$(printf '%s' "$manifest" | jq -r '.ledgersPerBatch // ""')"
  m_bpp="$(printf '%s' "$manifest" | jq -r '.batchesPerPartition // ""')"
  m_comp="$(printf '%s' "$manifest" | jq -r '.compression // ""')"
  [ "$m_pass" = "$EXPECT_PASSPHRASE" ] || problem "manifest networkPassphrase='$m_pass' (want '$EXPECT_PASSPHRASE')"
  [ "$m_lpb" = "1" ] || problem "manifest ledgersPerBatch=$m_lpb (want 1)"
  [ "$m_bpp" = "$PARTITION_LEDGERS" ] || problem "manifest batchesPerPartition=$m_bpp (want $PARTITION_LEDGERS)"
  [ "$m_comp" = "zstd" ] || problem "manifest compression='$m_comp' (want zstd)"
fi

# ── Listing ──
if [ -n "${PUBLIC_DATASET_LISTING:-}" ]; then
  raw="$(cat "$PUBLIC_DATASET_LISTING")"
else
  raw="$(aws s3 ls --no-sign-request "$BUCKET_PREFIX/")"
fi
# Normalise: last whitespace-separated field, strip trailing '/', drop
# dotfiles (.config.json) and blanks. Sort numerically by start ledger.
prefixes="$(printf '%s\n' "$raw" | awk 'NF{print $NF}' | sed 's:/$::' | grep -v '^\.' | grep -v '^$' || true)"
count="$(printf '%s\n' "$prefixes" | grep -c . || true)"
if [ "$count" -eq 0 ]; then
  problem "listing is EMPTY — no partition prefixes under $BUCKET_PREFIX/"
fi

# sort_by_start — order prefixes by their start ledger. Prefixes that do
# not parse keep a key of -1 so they reach the shape check (and are
# reported as malformed) instead of being silently dropped here.
sort_by_start() {
  awk '{
    key = -1
    if (match($0, /^[0-9A-F]{8}--[0-9]+-[0-9]+$/)) {
      split($0, a, "--"); split(a[2], r, "-"); key = r[1]
    }
    print key "\t" $0
  }' | sort -n | cut -f2-
}

# ── 2 + 3. Per-prefix shape, then contiguity over the start-sorted set ──
expected_start=0
last_end=-1
last_prefix=""
malformed=0
while IFS= read -r p; do
  [ -z "$p" ] && continue
  if ! [[ "$p" =~ ^([0-9A-F]{8})--([0-9]+)-([0-9]+)$ ]]; then
    problem "malformed partition prefix '$p' (want HEX--start-end)"
    malformed=$((malformed + 1)); continue
  fi
  hex="${BASH_REMATCH[1]}"; start="${BASH_REMATCH[2]}"; end="${BASH_REMATCH[3]}"
  if [ $((end - start + 1)) -ne "$PARTITION_LEDGERS" ] || [ $((start % PARTITION_LEDGERS)) -ne 0 ]; then
    problem "partition '$p' is not a 64000-aligned $PARTITION_LEDGERS-ledger range"
    malformed=$((malformed + 1)); continue
  fi
  want_hex="$(printf '%08X' $((0xFFFFFFFF - start)))"
  if [ "$hex" != "$want_hex" ]; then
    problem "partition '$p' hex prefix != 0xFFFFFFFF-start ($want_hex)"
    malformed=$((malformed + 1)); continue
  fi
  if [ "$start" -lt "$expected_start" ]; then
    problem "partition '$p' overlaps/duplicates the previous partition (ending $last_end)"
  elif [ "$start" -gt "$expected_start" ]; then
    problem "GAP: ledgers $expected_start..$((start - 1)) have no partition (next is '$p')"
  fi
  expected_start=$((end + 1))
  last_end="$end"
  last_prefix="$p"
done < <(printf '%s\n' "$prefixes" | sort_by_start)

# ── 4. Trimmed range must be inside contiguous coverage ──
# Coverage is contiguous 0..first_gap-1; compute that bound directly so
# a gap ABOVE the trimmed range does not mask a hole inside it.
contig_end=-1
prev_end=-1
while IFS= read -r p; do
  [ -z "$p" ] && continue
  [[ "$p" =~ ^[0-9A-F]{8}--([0-9]+)-([0-9]+)$ ]] || continue
  s="${BASH_REMATCH[1]}"; e="${BASH_REMATCH[2]}"
  [ "$s" -eq $((prev_end + 1)) ] || break
  prev_end="$e"; contig_end="$e"
done < <(printf '%s\n' "$prefixes" | sort_by_start)
if [ "$contig_end" -lt "$TRIM_HI" ]; then
  problem "trimmed range [$TRIM_LO, $TRIM_HI] is NOT fully covered — contiguous coverage from 0 ends at $contig_end; r1 holds no local copy of that range"
fi

# ── 5. Tip freshness ──
tip="${PUBLIC_DATASET_TIP:-}"
tip_note=""
if [ -z "$tip" ]; then
  tip="$(curl -sS -m 20 -H 'Accept: application/json' "$HORIZON_URL" 2>/dev/null | jq -r '.history_latest_ledger // empty' 2>/dev/null || true)"
fi
if [[ "$tip" =~ ^[0-9]+$ ]]; then
  floor=$((tip - TIP_SLACK_PARTITIONS * PARTITION_LEDGERS))
  if [ "$last_end" -lt "$floor" ]; then
    problem "last partition ends at $last_end but tip is $tip — dataset is > $TIP_SLACK_PARTITIONS partitions behind (publication stalled?)"
  fi
else
  tip="unknown"
  tip_note=" (tip source unreachable — freshness assertion SKIPPED, not counted as drift)"
  echo "public-dataset: WARN — could not determine ledger tip from $HORIZON_URL; skipping freshness assertion."
fi

# ── Report ──
echo "public-dataset: partitions=$count last=$last_prefix contiguous_to=$contig_end tip=$tip${tip_note}"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "partition_count=$count"
    echo "last_partition=$last_prefix"
    echo "contiguous_to=$contig_end"
    echo "tip=$tip"
  } >> "$GITHUB_OUTPUT"
fi

if [ "$problems" -gt 0 ]; then
  echo "public-dataset: DRIFT — $problems problem(s) in $BUCKET_PREFIX (see above)."
  exit 1
fi
echo "public-dataset: OK — $count contiguous $PARTITION_LEDGERS-ledger partitions covering 0..$last_end; manifest unchanged; trimmed range [$TRIM_LO, $TRIM_HI] present."
exit 0
