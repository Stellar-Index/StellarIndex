---
title: ADR-0033 data-recovery runbook
last_verified: 2026-09-02
status: current
---

# ADR-0033 data-recovery runbook

The data-completion half of ADR-0033: recover the **historical losses** the
verification tooling found, populate the **substrate**, and compute
**truthful completeness watermarks**. Tooling and forward fixes are shipped;
this is the bulk recovery.

## What was found (fixed forward; historical ranges lossy until re-backfilled)

1. **`soroban_events` dropped ~55% of events** in multi-event operations:
   `event_index` was hardcoded `0`, so events in one op collided on the PK
   and `ON CONFLICT DO NOTHING` kept the first (Phoenix: 8 events per swap →
   1 kept). r1 [62847626,62848626]: 575,266-event census vs 256,854 rows.
2. **`trades` dropped multi-trade-per-op trades** for aquarius, comet,
   soroswap, phoenix: rows keyed on raw `op_index` collided on
   `(source, ledger, tx_hash, op_index, ts)` (ledger 62848858: 5 aquarius
   trade events → 2 rows). Fixed by `canonical.FanoutOpIndex(op, event_index)`.

## Hard constraint: box capacity (READ FIRST)

r1 (**20 cores**, also running live ingest + aggregator) runs **exactly one
heavy backfill at a time**; a second stalls live ingest (the ledgerstream
cursor stops advancing).

- **Never** launch a recovery while another heavy job (`backfill-router`,
  another `backfill`, a census run) is active:
  `pgrep -af "stellarindex-ops (backfill|census-backfill)"`.
- **Cap CPU per worker**: `GOMAXPROCS=2 nice -n 19`. Go defaults
  `GOMAXPROCS` to the core count, so N unbounded workers oversubscribe ~N×20
  (this spiked load to 60).
- **≤ 2–4 workers**; check the live ledgerstream cursor every ~30s. It must
  keep ~network rate (~1 ledger / 5s); reduce workers if it lags. A
  sustained stall is not acceptable.
- Postgres `max_connections` is 200; each `Store` pool is 25 but a serial
  backfill uses ~1–2. Watch `SELECT count(*) FROM pg_stat_activity`.

## Recovery sequence (run in order, one heavy job at a time)

On r1 with `set -a; . /etc/default/stellarindex; set +a`
(`$STELLARINDEX_POSTGRES_DSN` + S3 creds). Historical ledgers live in the
**`galexie-archive`** bucket (full mirror), not `galexie-live`.

### 0. Deploy the committed forward fixes (if not already live)

soroswap/phoenix `op_index` fanout (commits `f7397cc2`, `7c017dac`) need a
stellarindex-indexer + ops redeploy + indexer restart.

### 1. Substrate: census-backfill → `ledger_ingest_log`

```
GOMAXPROCS=2 nice -n 19 stellarindex-ops census-backfill \
  -config /etc/stellarindex.toml -from 50457424 -to <tip> \
  -bucket galexie-archive -resume
```

Soroban era `[50457424, tip]` first; pre-Soroban `[2, 50457424]` (sdex only)
after. Split into ≤4 chunks if parallelizing, monitoring live ingest.
Resumable per-chunk cursor (`source='census-backfill'`). Prerequisite for
truthful watermarks and sdex reconciliation.

**The exit code is load-bearing**: `census-backfill` exits non-zero unless it
persisted a substrate row for EVERY ledger walked (`censusCoverage`).

- Keep `-bucket galexie-archive` explicit; the default LIVE bucket holds no
  historic range.
- Set `-to` to the **archive** tip, not the live tip: `galexie-archive`
  mirrors only complete 64,000-ledger partitions, so a live-tip `-to` reports
  the newest partition short. Re-run later: the cursor resumes at the first
  gap and `UpsertLedgerIngestLog` is `ON CONFLICT DO UPDATE`, so re-runs
  converge.

### 2. `soroban_events` re-backfill (recover the ~55% loss)

```
GOMAXPROCS=2 nice -n 19 stellarindex-ops backfill -config /etc/stellarindex.toml -write -source soroban-events \
  -from 50457424 -to <tip> -bucket galexie-archive -parallel <2-4>
```

With the `event_index` fix, previously collided events get distinct PKs and
insert (~55% more rows). **Heavy**: never concurrent with step 1.

### 3. `trades` re-backfill (recover the op_index-collision losses)

The fix changed the `op_index` encoding (raw → `op<<16|event_index`), so
replaying without deleting first duplicates trades. **Delete-then-replay**,
per source + range.

`ledger` is the correctness predicate. The `ts` bound exists only for chunk
exclusion (`trades` is partitioned on `ts`, migrations/0001; a `ledger`-only
DELETE decompresses every chunk). Paste the `ts` bounds as literals: a
`ledger_ingest_log` subquery returns NULL on a missing endpoint row, and the
DELETE then matches nothing and still reports success.

```sql
-- one source + bounded range at a time; verify the range first.
-- 1. Resolve the ts bounds. Expect exactly 2 rows; stop if either is missing.
SELECT ledger_seq, ledger_close_time
  FROM ledger_ingest_log
 WHERE ledger_seq IN (<from>, <to>);

-- 2. Count what the DELETE must remove (ledger predicate only).
SELECT count(*) FROM trades
 WHERE source IN ('aquarius','comet','soroswap','phoenix')
   AND ledger BETWEEN <from> AND <to>;

-- 3. Delete. trades.ts is the ledger close time, so the inclusive
--    bounds from step 1 cover every row in the ledger range.
BEGIN;
DELETE FROM trades
 WHERE source IN ('aquarius','comet','soroswap','phoenix')
   AND ledger BETWEEN <from> AND <to>
   AND ts >= '<from_close_time>'::timestamptz
   AND ts <= '<to_close_time>'::timestamptz;
-- DELETE n must equal step 2's count. If it does not, ROLLBACK.
COMMIT;
```

Replay with **`projector-replay`**, never `stellarindex-ops backfill -source
aquarius,comet,soroswap,phoenix`. These are projected (ADR-0031/0032) and
ADR-0035-gated sources; `backfill` builds gated decoders with an EMPTY
identity registry, so on an older binary it decodes nothing and **exits 0
after your DELETE**. Current binaries refuse projector-owned sources at flag
parse and name `projector-replay`, which reads the lake through the real
gated registry. For a rewind larger than about **1M ledgers** use
`stellarindex-ops projected-rebuild … -write` instead (`projector-replay` is
bound by the live projector's 5 s tick and 60 s per-source timeout; see
[runbooks/projector.md#stellarindex_projector_replay_stalled](runbooks/projector.md#stellarindex_projector_replay_stalled)). Never run the
two concurrently over the same source's history.

```sh
# Dry-run first — prints the dirty window and cursor rewind it would do.
# (Dry run is the DEFAULT; -dry-run is a documented no-op alias.)
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
  -source aquarius -from <from> -dry-run

# Then for real, one source at a time, under the heavy-job wrapper.
# -write is MANDATORY — see the warning below.
/usr/local/sbin/run-heavy-job.sh projector-replay-aquarius \
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source aquarius -from <from> -write
```

> ⚠️ **`projector-replay` is fail-closed: without `-write` it writes nothing
> and exits 0** (the shared write gate, `opsutil.RegisterWriteGate`, makes
> dry run the default). A `-write`-less run after the `DELETE` prints
> `═══ DRY RUN — no writes; pass -write to apply ═══` on **stderr**, buried in
> the wrapper's preamble, rewinds no cursor, and leaves the deleted range
> permanently empty. Confirm `═══ WRITING — applying changes ═══` and a
> non-zero row count for the replayed range **before deleting the next one**.

Only delete rows you are about to replay, and never a range you cannot
replay. soroswap requires the pair-registry seed for token identities.

### 4. Truthful watermarks + verification

```
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch
stellarindex-ops verify-recognition   -config /etc/stellarindex.toml -from 50457424 -to <tip>
stellarindex-ops verify-reconciliation -config /etc/stellarindex.toml -from <from> -to <to>
```

`compute-completeness` writes `completeness_snapshots`; the API overlays
`completeness_pct` onto `/v1/diagnostics/ingestion` and the status page uses
it as the headline instead of `gap_free_pct`. A source whose watermark is
below tip names the exact ledger to investigate. Done when every watermark
reaches tip and `verify-recognition` / `verify-reconciliation` are clean.

## Why `gap_free_pct` is not the headline

`gap_free_pct` counts only the largest *interior* gap between present rows.
It is blind to the **leading gap** from genesis to first data and to **empty
tables**, so a sparse source reads 100% with almost no data (phoenix-liquidity:
18 distinct ledgers / 11.3M, shown as 100%). Counting the leading gap would
fire its alert for every incomplete source, so that is the
wrong fix. The honest signal is the watermark, which needs the step-1
substrate; without it, coverage figures overstate completeness.
