#!/usr/bin/env bash
# D2 — in-CH intra_ledger_seq reproject for stellar.ledger_entry_changes.
#
# Restores the per-ledger ordinal on full-fidelity-but-un-ordinaled rows so
# ledger_entries_current's ReplacingMergeTree dedup picks the LAST intra-ledger
# change to a key rather than an arbitrary one (audit C2-4c). See
# docs/operations/d2-ordinal-reproject.md for the validated formula, the
# census-row data-loss guard, and why chunking is mandatory.
#
# Per partition: build a staging table chunk-by-chunk, VERIFY it, then
# REPLACE PARTITION atomically. Resumable — completed partitions are recorded
# and skipped. Idempotent — recomputing a correct partition yields identical
# ordinals.
#
# Usage:
#   d2-ordinal-reproject.sh <first_partition> <last_partition> [chunk_ledgers]
#   d2-ordinal-reproject.sh 45 45          # canary a single partition
#   d2-ordinal-reproject.sh 39 53          # the full D2 range
#
# Run it under the heavy-job wrapper so the data-pool watchdog can stop it:
#   run-heavy-job.sh d2-reproject /usr/local/sbin/d2-ordinal-reproject.sh 45 45
set -euo pipefail

CH="${CH:-clickhouse-client --port 9300}"
STATE="${D2_STATE:-/var/lib/ch-backfill/d2-done-partitions.txt}"
FIRST="${1:?first partition}"
LAST="${2:?last partition}"
CHUNK="${3:-25000}"          # ledgers per INSERT; ~25k kept the window inside CH's memory cap
SEED=4294967295              # MaxUint32 — ledger_entries_current seed rows, if any
COLS_NO_ORD="ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, ingested_at, account_id, asset, balance"

mkdir -p "$(dirname "$STATE")"; touch "$STATE"
log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) d2: $*"; }
q()   { $CH --max_execution_time 3600 -q "$1"; }

for P in $(seq "$FIRST" "$LAST"); do
  if grep -qx "$P" "$STATE"; then log "partition $P already done — skipping"; continue; fi

  LO=$(( P * 1000000 )); HI=$(( LO + 999999 ))
  STAGE="stellar.lec_stage_${P}"
  log "=== partition $P  ledgers [$LO,$HI] ==="

  SRC_TOTAL=$(q "SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN $LO AND $HI")
  SRC_CENSUS=$(q "SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN $LO AND $HI AND tx_hash = ''")
  SRC_SEED=$(q "SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN $LO AND $HI AND intra_ledger_seq = $SEED")
  log "source rows=$SRC_TOTAL census=$SRC_CENSUS seed=$SRC_SEED"
  if [ "$SRC_TOTAL" = "0" ]; then log "partition $P empty — recording done"; echo "$P" >> "$STATE"; continue; fi
  # Seed rows would need to be carried through the window untouched. None were
  # measured in 38-54; refuse rather than silently mishandle them if that changes.
  if [ "$SRC_SEED" != "0" ]; then log "ABORT: $SRC_SEED seed rows in partition $P — not handled by this script"; exit 1; fi

  q "DROP TABLE IF EXISTS $STAGE"
  q "CREATE TABLE $STAGE AS stellar.ledger_entry_changes"

  for (( CLO=LO; CLO<=HI; CLO+=CHUNK )); do
    CHI=$(( CLO + CHUNK - 1 )); [ "$CHI" -gt "$HI" ] && CHI=$HI
    # (1) real rows — ordinal recomputed in the canonical walk order.
    #     ORDER BY tx_index, change_index ONLY. op_index must NOT appear:
    #     it is -1 for fee/TxChangesBefore/After, which would interleave
    #     tx-level and per-op changes wrongly (the update->remove bug).
    # (2) census rows (tx_hash='') — NO transaction to join, so they are
    #     preserved verbatim and EXCLUDED from the window. An inner join
    #     alone would drop them, and REPLACE PARTITION would then delete
    #     them permanently (~9M rows across the D2 range). They are removed
    #     deliberately later, by the cleanup phase.
    q "INSERT INTO $STAGE
       SELECT lec.ledger_seq, lec.close_time, lec.tx_hash, lec.op_index, lec.change_index,
              lec.change_type, lec.entry_type, lec.key_xdr, lec.entry_xdr, lec.ingested_at,
              lec.account_id, lec.asset, lec.balance,
              toUInt32(row_number() OVER (PARTITION BY lec.ledger_seq
                                          ORDER BY t.tx_index, lec.change_index) - 1)
       FROM stellar.ledger_entry_changes AS lec
       INNER JOIN (
         -- DEDUP THE JOIN SIDE. stellar.transactions is a ReplacingMergeTree and
         -- the backfill re-ran ranges, so old partitions carry UNMERGED duplicate
         -- parts — measured exactly 2x for ledger 45000000 (320 rows / 160 hashes).
         -- Joining raw multiplies every lec row by the duplicate factor, so
         -- row_number() numbers ~2x as many positions and the ordinals come out
         -- silently DOUBLED. The staging RMT then collapses the duplicate rows on
         -- merge, hiding the inflation from a naive row-count check — the counts
         -- reconcile while every ordinal is wrong. argMax on the sort key gives
         -- exactly one tx_index per (ledger_seq, tx_hash).
         SELECT ledger_seq, tx_hash, argMax(tx_index, ingested_at) AS tx_index
         FROM stellar.transactions
         WHERE ledger_seq BETWEEN $CLO AND $CHI
         GROUP BY ledger_seq, tx_hash
       ) AS t
         ON t.ledger_seq = lec.ledger_seq AND t.tx_hash = lec.tx_hash
       WHERE lec.ledger_seq BETWEEN $CLO AND $CHI AND lec.tx_hash != ''
       UNION ALL
       SELECT $COLS_NO_ORD, intra_ledger_seq
       FROM stellar.ledger_entry_changes
       WHERE ledger_seq BETWEEN $CLO AND $CHI AND tx_hash = ''"
    log "  chunk [$CLO,$CHI] inserted"
  done

  # ---- verification gate: every check must pass before REPLACE ----
  STG_TOTAL=$(q "SELECT count() FROM $STAGE")
  STG_CENSUS=$(q "SELECT count() FROM $STAGE WHERE tx_hash = ''")
  # per-ledger ordinals over non-census rows must be a contiguous 0..N-1 set
  BAD_LEDGERS=$(q "SELECT count() FROM (
      SELECT ledger_seq FROM $STAGE WHERE tx_hash != ''
      GROUP BY ledger_seq
      HAVING max(intra_ledger_seq) + 1 != count() OR uniqExact(intra_ledger_seq) != count())")
  log "verify: stage rows=$STG_TOTAL (src $SRC_TOTAL) census=$STG_CENSUS (src $SRC_CENSUS) bad_ledgers=$BAD_LEDGERS"

  if [ "$STG_TOTAL" != "$SRC_TOTAL" ]; then log "ABORT p$P: row count mismatch — REFUSING to replace"; exit 1; fi
  if [ "$STG_CENSUS" != "$SRC_CENSUS" ]; then log "ABORT p$P: census rows lost — REFUSING to replace"; exit 1; fi
  if [ "$BAD_LEDGERS" != "0" ]; then log "ABORT p$P: $BAD_LEDGERS ledgers with non-contiguous ordinals — REFUSING"; exit 1; fi

  q "ALTER TABLE stellar.ledger_entry_changes REPLACE PARTITION $P FROM $STAGE"
  q "DROP TABLE $STAGE"
  echo "$P" >> "$STATE"
  log "partition $P DONE + verified"
done

log "all partitions [$FIRST,$LAST] complete"
