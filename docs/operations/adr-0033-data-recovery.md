# ADR-0033 data-recovery runbook

This runbook drives the data-completion half of ADR-0033: recovering the
**historical losses** the verification tooling uncovered, populating the
**substrate**, and computing the **truthful completeness watermarks** so
the status page reports real coverage. The verification tooling and the
forward fixes are already shipped; this is the bulk-recovery work that
gets coverage to a *true* (not merely asserted) 100%.

## What was found (and already fixed forward)

1. **`soroban_events` dropped ~55% of events** in multi-event operations.
   `event_index` was hardcoded `0` at capture, so every contract event in
   one op collided on the PK and `ON CONFLICT DO NOTHING` kept only the
   first (Phoenix emits 8 events per swap → 1 kept). Validated on r1:
   ledger range [62847626,62848626] had a 575,266-event census vs 256,854
   rows. **Fixed forward** (real `event_index` threaded); historical
   ranges still lossy until re-backfilled.

2. **`trades` dropped multi-trade-per-op trades** for aquarius, comet,
   soroswap, phoenix. The decoders keyed the row on the raw `op_index`,
   so multiple trades in one op (multi-pool / router multi-hop) collided
   on `(source, ledger, tx_hash, op_index, ts)`. Validated: ledger
   62848858 had 5 aquarius trade events → 2 rows. **Fixed forward**
   (`canonical.FanoutOpIndex(op, event_index)`); historical ranges lossy
   until re-backfilled.

## Hard constraint: box capacity (READ FIRST)

r1 is a **20-core** box also running live ingest + aggregator. It can run
**exactly one heavy backfill at a time** alongside live ingest. Running a
second concurrently **stalls live ingest** (the ledgerstream cursor stops
advancing) — observed twice on 2026-06-02 when a census-backfill was
launched while a `backfill-router` job held capacity.

Rules:

- **Never** launch a recovery backfill while another heavy backfill
  (`backfill-router`, another `backfill`, a census run) is active. Check:
  `pgrep -af "stellaratlas-ops (backfill|census-backfill)"`.
- **Cap CPU per worker**: `GOMAXPROCS=2 nice -n 19`. Go defaults
  `GOMAXPROCS` to the core count (20), so N unbounded workers oversubscribe
  ~N×20 — this is what spiked load to 60 and stalled ingest.
- **≤ 2–4 workers**, and **monitor the live ledgerstream cursor** every
  ~30s. It must keep ~network rate (~1 ledger / 5s). If it lags, reduce
  workers. Live ingest is the priority (r1 has no consumers yet, so brief
  lag is tolerable, but a sustained stall is not).
- Postgres `max_connections` is 200; each `Store` pool is 25 but a
  serial backfill uses ~1–2. Watch `SELECT count(*) FROM pg_stat_activity`.

## Recovery sequence (run in order, one heavy job at a time)

All commands run on r1 with the env sourced:
`set -a; . /etc/default/stellaratlas; set +a` (provides
`$STELLARATLAS_POSTGRES_DSN` + S3 creds). Historical ledgers live in the
**`galexie-archive`** bucket (R1 full mirror), not `galexie-live`.

### 0. Deploy the committed forward fixes (if not already live)

soroswap/phoenix `op_index` fanout (commits `f7397cc2`, `7c017dac`) need a
stellaratlas-indexer + ops redeploy + indexer restart. aquarius/comet +
the `event_index` fix are already deployed.

### 1. Substrate: census-backfill → `ledger_ingest_log`

```
GOMAXPROCS=2 nice -n 19 stellaratlas-ops census-backfill \
  -config /etc/stellaratlas.toml -from 50457424 -to <tip> \
  -bucket galexie-archive -resume
```

Prioritize the Soroban era `[50457424, tip]` (all Soroban protocols);
the pre-Soroban range `[2, 50457424]` (sdex only) can follow. Split into
≤4 chunks if parallelizing, each a separate worker, **monitoring live
ingest**. Resumable per-chunk cursor (`source='census-backfill'`).
This is the prerequisite for truthful watermarks and for sdex
reconciliation.

### 2. `soroban_events` re-backfill (recover the ~55% loss)

```
GOMAXPROCS=2 nice -n 19 stellaratlas-ops backfill -source soroban-events \
  -from 50457424 -to <tip> -bucket galexie-archive -parallel <2-4>
```

With the deployed `event_index` fix, the previously-collided events now
get distinct PKs and insert. Adds ~55% more rows to `soroban_events`
(storage is fine — ~12 TB free on the pool). **Heavy** — do not run
concurrently with step 1.

### 3. `trades` re-backfill (recover the op_index-collision losses)

The forward fix changed the `op_index` encoding (raw → `op<<16|event_index`),
so re-backfilling **without** deleting first would create duplicates (old
raw-op rows + new fanned-out rows for the same trade). So **delete-then-replay**,
scoped per source + range:

```sql
-- one source + bounded range at a time; verify the range first
DELETE FROM trades
 WHERE source IN ('aquarius','comet','soroswap','phoenix')
   AND ledger BETWEEN <from> AND <to>;
```
```
GOMAXPROCS=2 nice -n 19 stellaratlas-ops backfill \
  -source aquarius,comet,soroswap,phoenix \
  -from <from> -to <to> -bucket galexie-archive
```

Safety: only delete rows you are about to immediately re-backfill from
`galexie-archive` (the source of truth); never delete a range you can't
replay. Do it in bounded windows and verify row counts recover before
moving on. soroswap requires the pair-registry seed (RPC) for token
identities — the backfill path seeds it.

### 4. Truthful watermarks + verification

```
stellaratlas-ops compute-completeness -config /etc/stellaratlas.toml
stellaratlas-ops verify-recognition   -config /etc/stellaratlas.toml -from 50457424 -to <tip>
stellaratlas-ops verify-reconciliation -config /etc/stellaratlas.toml -from <from> -to <to>
```

`compute-completeness` writes `completeness_snapshots`; the API overlays
`completeness_pct` onto `/v1/diagnostics/ingestion` and the status page
renders it as the headline (replacing the misleading `gap_free_pct`).
Iterate: any source whose watermark is below tip names the exact ledger
to investigate/backfill. Done when every source's watermark reaches tip
and `verify-recognition` / `verify-reconciliation` are clean.

## Why the headline can't be trusted until this runs

`gap_free_pct` (the current status-page headline) only counts the largest
*interior* gap between present rows — it is blind to the **leading gap**
from genesis to first data and to **empty tables**, so recently-started or
sparse-but-incomplete sources read 100% while having almost no data (e.g.
phoenix-liquidity: 18 distinct ledgers / 11.3M, shown as 100%). Patching
`gap_free_pct` to count the leading gap would make the *alert* it feeds
fire for every incomplete source — wrong fix. The honest signal is the
watermark, which requires the substrate (step 1). Until then, the status
page's coverage figures overstate completeness; that is a known, recorded
limitation, not a truthful 100%.
