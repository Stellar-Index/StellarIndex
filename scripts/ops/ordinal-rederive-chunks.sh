#!/usr/bin/env bash
# ordinal-rederive-chunks.sh — re-derive ledger_entry_changes across the
# un-ordinaled band so `intra_ledger_seq` is populated, which is the
# MANDATORY pre-step before D3.
#
# Why this exists, and why it is chunked + tuned the way it is:
#
#   * D3's composite version is (ledger_seq << 32) | intra_ledger_seq. In
#     [63.0M, 63.55M) BOTH the `state` before-image and its `updated`
#     after-image carry intra_ledger_seq = 0, so that version still TIES
#     and ReplacingMergeTree keeps an arbitrary row — which is how ~38%
#     of sampled accounts came to serve a stale pre-transaction balance.
#     D3 alone cannot fix them; the ordinals must exist first.
#
#   * NOT d2-ordinal-reproject.sh, which is retired and refuses to run:
#     its SQL ranking is the EntryWalkVersion-1 order, not the ledger-wide
#     three-phase walk the writer uses. ch-backfill re-derives through
#     ExtractLedger -> extractLedgerEntryChanges (the current walk) and
#     writes idempotent RMT rows that supersede by ingested_at. No
#     partition swap, safe against live ingest. START/BAND_END select any
#     range, including partitions 39-53, which D2 left in version-1 order.
#
#   * CHUNKED because ch-backfill has no resume: one long run that dies
#     loses all progress. Each ~110k-ledger chunk is durable on its own,
#     and re-running any chunk is free (idempotent).
#
#   * parallel=3 / flush-every=100 chosen from MEASURED memory, not a
#     guess: 1 worker at flush-every=100 held 2.8 GB, so 3 workers ≈
#     8.4 GB against run-heavy-job.sh's 20 G cap. The first attempt used
#     parallel=4 / flush-every=500 (the default flush) and was OOM-killed
#     in 22 seconds — 4x500 buffered Soroban-era ledgers exceed 20 G.
#
# Contract:
#   * Chunks cover [START, BAND_END): BAND_END is EXCLUSIVE. ch-backfill's -to
#     is inclusive, so each chunk [lo,hi) runs -from lo -to hi-1.
#   * The first line printed is RUN_START_EPOCH=<unix seconds - 60>: a
#     timestamp safely before any row this run writes, for ingested_at
#     predicates in the follow-up steps (ClickHouse shares this host's clock).
#   * Disk gates on the ZFS pool (POOL, default data), read as
#     `zfs list -Hp -o avail`: START_MIN_AVAIL_BYTES (default 2 TiB) once
#     before chunk 1, MIN_AVAIL_BYTES (default 1.25 TiB, which keeps raw free
#     above zfs-snapshot's guard) before every chunk. Below a floor: exit 3.
#     Unreadable avail: exit 4 (fail closed). A failed chunk: exit 1.
#   * BUCKET (default galexie-archive) is passed as -bucket unless empty;
#     EXTRA_FLAGS is word-split onto every ch-backfill call (e.g.
#     EXTRA_FLAGS=-changes-only to skip rewriting the non-entry tables).
#
# Run under the heavy-job wrapper:
#   run-heavy-job.sh ord-chunks /usr/local/sbin/ordinal-rederive-chunks.sh
set -uo pipefail

CONFIG="${CONFIG_PATH:-/etc/stellarindex.toml}"
CH_ADDR="${CH_ADDR:-127.0.0.1:9300}"
OPS="${OPS:-/usr/local/bin/stellarindex-ops}"
BAND_END="${BAND_END:-63550000}"
CHUNK="${CHUNK:-110000}"
START="${START:-63000000}"
# r1 has no live seam, so without -bucket ch-backfill reads the trimmed live
# bucket and the first historical chunk finds zero ledgers.
BUCKET="${BUCKET-galexie-archive}"
EXTRA_FLAGS="${EXTRA_FLAGS:-}"
POOL="${POOL:-data}"
START_MIN_AVAIL_BYTES="${START_MIN_AVAIL_BYTES:-2199023255552}"
MIN_AVAIL_BYTES="${MIN_AVAIL_BYTES:-1374389534720}"

echo "RUN_START_EPOCH=$(( $(date +%s) - 60 ))"

for v in BAND_END CHUNK START START_MIN_AVAIL_BYTES MIN_AVAIL_BYTES; do
  case "${!v}" in
    '' | *[!0-9]*) echo "ABORT: $v='${!v}' is not a non-negative integer" >&2; exit 2 ;;
  esac
done
[ "$CHUNK" -gt 0 ] || { echo "ABORT: CHUNK must be > 0" >&2; exit 2; }

bucket_args=()
[ -n "$BUCKET" ] && bucket_args=(-bucket "$BUCKET")
extra=()
read -r -a extra <<<"$EXTRA_FLAGS"

# require_avail <floor_bytes> <label> — exit 4 if the pool's avail cannot be
# read, exit 3 if it is below the floor.
require_avail() {
  local avail
  avail="$(zfs list -Hp -o avail "$POOL" 2>/dev/null)" || avail=""
  case "$avail" in
    '' | *[!0-9]*)
      echo "ABORT: cannot read zfs avail for pool '$POOL' (got '$avail') — failing closed" >&2
      exit 4
      ;;
  esac
  if [ "$avail" -lt "$1" ]; then
    echo "STOP: pool '$POOL' avail $avail B is below the $2 floor $1 B — no further chunks written" >&2
    exit 3
  fi
  echo "pool '$POOL' avail $avail B >= $2 floor $1 B"
}

require_avail "$START_MIN_AVAIL_BYTES" START_MIN_AVAIL_BYTES

for (( lo=START; lo<BAND_END; lo+=CHUNK )); do
  hi=$(( lo + CHUNK ))
  [ "$hi" -gt "$BAND_END" ] && hi=$BAND_END
  require_avail "$MIN_AVAIL_BYTES" MIN_AVAIL_BYTES
  echo "=== chunk [$lo,$hi) -from $lo -to $(( hi - 1 )) $(date -u +%H:%M:%SZ) ==="
  "$OPS" ch-backfill -write -config "$CONFIG" -ch-addr "$CH_ADDR" \
        ${bucket_args[@]+"${bucket_args[@]}"} -from "$lo" -to "$(( hi - 1 ))" \
        -parallel 3 -flush-every 100 ${extra[@]+"${extra[@]}"}
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "CHUNK [$lo,$hi) FAILED rc=$rc — stopping so the failure is visible"
    exit 1
  fi
done
echo "ALL CHUNKS DONE $(date -u +%H:%M:%SZ)"
