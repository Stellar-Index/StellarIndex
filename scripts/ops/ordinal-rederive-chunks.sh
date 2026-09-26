#!/usr/bin/env bash
# ordinal-rederive-chunks.sh — re-derive ledger_entry_changes across the
# un-ordinaled band so `intra_ledger_seq` is populated, which is the
# MANDATORY pre-step before D3 (audit C2-4c / CS-021).
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
# Run under the heavy-job wrapper:
#   run-heavy-job.sh ord-chunks /usr/local/sbin/ordinal-rederive-chunks.sh
set -uo pipefail

CONFIG="${CONFIG_PATH:-/etc/stellarindex.toml}"
CH_ADDR="${CH_ADDR:-127.0.0.1:9300}"
OPS="${OPS:-/usr/local/bin/stellarindex-ops}"
BAND_END="${BAND_END:-63550000}"
CHUNK="${CHUNK:-110000}"
START="${START:-63000000}"

for (( lo=START; lo<BAND_END; lo+=CHUNK )); do
  hi=$(( lo + CHUNK ))
  [ "$hi" -gt "$BAND_END" ] && hi=$BAND_END
  echo "=== chunk [$lo,$hi) $(date -u +%H:%M:%SZ) ==="
  "$OPS" ch-backfill -write -config "$CONFIG" -ch-addr "$CH_ADDR" \
        -from "$lo" -to "$hi" -parallel 3 -flush-every 100
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "CHUNK [$lo,$hi) FAILED rc=$rc — stopping so the failure is visible"
    exit 1
  fi
done
echo "ALL CHUNKS DONE $(date -u +%H:%M:%SZ)"
