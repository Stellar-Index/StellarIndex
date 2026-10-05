---
title: SEP-41 dropped-mint recovery
last_verified: 2026-09-02
status: current
---

# SEP-41 dropped-mint recovery (scoped)

Recover a few SEP-41 supply rows (mint / burn / clawback) that a **decoder
bug dropped**, without a full-history `ch-rebuild -sep41` re-derive (~370M
events, hours, risks a latency incident). The SEP-41 analogue of SDEX's
`-sdex-gaps` / `-sdex-reconcile`: a **scoped, additive** lake read of only
the affected contracts.

`sep41_*` is a **projected** domain (AGENTS.md invariant [7]). Its normal
catch-up is `stellarindex-ops projector-replay -config PATH -source sep41_supply -from <ledger>`.
This page's lake re-derive is a second writer, used only because it can
scope to a contract subset; `ch-rebuild -write` therefore refuses any window
the live projector's cursor is still inside. Lower `-to` if refused; never
pass `-allow-live-overlap`. See
[ingest-pipeline.md §The replay decision rule](../architecture/ingest-pipeline.md#the-replay-decision-rule).

## When to use this (vs. the full re-derive)

Use the scoped path when ALL hold:

- `sep41_supply_events` is **post-migration-0057 clean** in the affected range,
- the forward decoder fix has landed,
- only a bounded set of contracts / rows is missing.

Otherwise (whole history, or purging pre-0057 collapsed rows) use the full
`ch-rebuild -sep41` truncate+re-derive (its flag help and
[adr-0033-data-recovery.md](adr-0033-data-recovery.md)); it TRUNCATEs
`sep41_transfers` + `sep41_supply_events` first.

## Why it is safe (idempotent + additive)

The write is `store.CopyMergeSEP41SupplyEvents` → `INSERT … ON CONFLICT
(contract_id, ledger, tx_hash, op_index, observed_at) DO NOTHING`: existing
rows are no-ops, only dropped rows insert, and a second run changes nothing.
`-contracts` narrows only the *read* (the CH `contract_id` prefilter), never
decode logic, so i128 discipline (ADR-0003) holds.

Worked case: a decoder bug (fixed in `632d06f9`) dropped **336 CAP-67
map-shaped mint events** across **8 of 15 watched contracts**; they read
`mint_total = 0`, so `burn_total > mint_total` tripped the dominant-burn guard
`stellarindex_aggregator_supply_refresh_error_dominant` (firing ×15).

## 0. Preconditions

- The **forward decoder fix is deployed** to r1 (the running
  `stellarindex-indexer` / `stellarindex-ops` binary post-dates it), or the
  re-derive re-drops the same rows.
- One heavy job at a time:
  `pgrep -af "stellarindex-ops (backfill|ch-rebuild|census)"` shows nothing.
- Source the runtime env on r1 (`$STELLARINDEX_POSTGRES_DSN` + S3 / ClickHouse creds):

  ```sh
  set -a; . /etc/default/stellarindex; set +a
  ```

## 1. Identify the affected contracts

The dominant-burn guard fires exactly when a contract's rollup shows
`burn_total > mint_total`:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -F, -c "
  SELECT contract_id
    FROM sep41_supply_rollup
   WHERE burn_total > mint_total
   ORDER BY contract_id;"
```

```sh
AFFECTED="CBH4M45T...OCKF,CDLZFC3S...YSC,CCW67TSZ...MI75"   # the 8 contracts
```

These MUST be a subset of `[supply] watched_sep41_contracts`: the sep41
decoders gate `Matches()` on the full watched set, so an unwatched contract
is read but decodes to nothing (ch-rebuild prints a WARNING). A `burn>mint`
contract that is not watched is a different problem; do not force it here.

## 2. Scoped, windowed re-derive (under the heavy-job wrapper)

- `-contracts` narrows the CH `contract_id` prefilter to the affected contracts.
- `-sep41-supply-only` reads only `mint`/`burn`/`clawback` topics, skipping
  the transfer firehose at the SQL layer; it requires `-sources sep41_supply`.
- `-sources` is mandatory with `-write`: without it the run selects the whole
  event catalogue, and the F050 BackfillSafe gate
  (`checkCHRebuildBackfillSafe` in `internal/ops/chops/ch_rebuild.go`) refuses.
  `sep41_supply` must be named or the fold reset (step 3) does not happen.
- `-config`, `-from`, `-to` are required. Window each run to
  **≤ 2,000,000 ledgers** (the tool enforces it; an unwindowed run once
  wedged galexie's captive core). The Soroban era starts at **50,457,424**.
- Run every invocation under `run-heavy-job.sh` (mandatory on r1), always
  with the same job label so the singleton lock holds.

```sh
# One 2M-ledger window (repeat, advancing FROM/TO, up to the tip):
run-heavy-job.sh sep41-mint-recover \
  stellarindex-ops ch-rebuild \
    -config /etc/stellarindex.toml \
    -ch-addr 127.0.0.1:9300 \
    -sep41 -sources sep41_supply -sep41-supply-only \
    -contracts "$AFFECTED" \
    -from 50457424 -to 52457423 \
    -write
```

Loop the windows (dry-run first: omit `-write` to see buffered counts):

```sh
TIP=$(psql "$STELLARINDEX_POSTGRES_DSN" -At -c "SELECT max(ledger) FROM sep41_supply_events;")
for FROM in $(seq 50457424 2000000 "$TIP"); do
  TO=$(( FROM + 1999999 )); [ "$TO" -gt "$TIP" ] && TO=$TIP
  run-heavy-job.sh sep41-mint-recover \
    stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -ch-addr 127.0.0.1:9300 \
      -sep41 -sources sep41_supply -sep41-supply-only -contracts "$AFFECTED" \
      -from "$FROM" -to "$TO" -write
done
```

If the affected range is known, sweep only the windows covering it.

## 3. Re-seed the supply rollup

`sep41_supply_rollup` (migration 0085) folds only rows with
`ledger > last_ledger`, so rows recovered below the checkpoint need a re-fold.
**Nothing to do by hand:** step 2's `-write` on the supply source already
re-folded exactly those contracts after the events landed (`sep41RollupResetPlan`
in `internal/ops/chops/ch_rebuild.go`; `Store.ResetSEP41SupplyRollupFold` in
`internal/storage/timescale/sep41_supply_events.go`, one transaction per
contract, genesis baseline preserved). Confirm the line:

```
ch-rebuild: reset 3 sep41_supply_rollup fold row(s) [SCOPED — 3 contract(s)],
each re-folded from zero in place (genesis baseline preserved)
```

If a contract's re-fold does not commit (lost the row lock to an aggregator
pass, or cancelled), it alone is zeroed to `last_ledger = 0` and step 2
exits non-zero with:

```
ch-rebuild: sep41 rollup reset: timescale: ResetSEP41SupplyRollupFold:
N contract(s) zeroed instead of re-folded (served exactly via the full-sum
read until the worker re-folds them): …
```

Those N serve the exact full-sum fallback until the aggregator's
`runSEP41SupplyRollup` worker re-folds them on its next
`[supply] aggregator_refresh_cadence` (sequentially, one contract at a time);
restart `stellarindex-aggregator` to do it sooner. If the error says
`the zeroing fallback failed`, those contracts may serve a stale fold: re-run
step 2.

> ⚠️ **Never `DELETE` or `TRUNCATE` `sep41_supply_rollup` — scoped or not.**
> Removing a row destroys migration 0088's `genesis_mint_total`,
> `genesis_burn_total`, `genesis_clawback_total` and `genesis_baseline_ledger`:
> the pre-Soroban totals, seeded once from the lake and not derivable from
> Soroban events. The worker recreates the row with `genesis_mint_total = 0`
> and `genesis_baseline_ledger = NULL`, which the reader
> (`SEP41GenesisBaselineSeeded`) treats as unseeded, so lifetime supply
> silently **under-reports** until someone runs
> `stellarindex-ops supply seed-sep41-genesis`. The `-contracts` flag help
> confirms the tool resets the fold itself.

**If the reset line is missing**, step 2 ran dry or without
`-sources sep41_supply`. Re-run with the supply source and `-write`:

```sh
# One window of <= 2,000,000 ledgers; loop FROM/TO exactly as in step 2.
/usr/local/sbin/run-heavy-job.sh sep41-mint-recover \
  stellarindex-ops ch-rebuild \
    -config /etc/stellarindex.toml \
    -ch-addr 127.0.0.1:9300 \
    -sep41 -sources sep41_supply,sep41_transfers \
    -contracts 'CBH4M45T...OCKF,CDLZFC3S...YSC,CCW67TSZ...MI75' \
    -from 50457424 -to 52457423 \
    -write
```

## 4. Verify

The affected contracts read `mint_total > 0` and `mint_total ≥ burn_total`:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -c "
  SELECT contract_id, mint_total, burn_total, clawback_total, last_ledger
    FROM sep41_supply_rollup
   WHERE contract_id IN ('CBH4M45T...OCKF','CDLZFC3S...YSC','CCW67TSZ...MI75')
   ORDER BY contract_id;"
```

The rows were written:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -c "
  SELECT contract_id, count(*)
    FROM sep41_supply_events
   WHERE event_kind = 'mint'
     AND contract_id IN ('CBH4M45T...OCKF','CDLZFC3S...YSC','CCW67TSZ...MI75')
   GROUP BY contract_id ORDER BY contract_id;"
```

`stellarindex_aggregator_supply_refresh_error_dominant` stops firing (all 15
instances resolve) within a couple of aggregator refresh cadences once no
watched contract reports `burn_total > mint_total`. The aggregator logs
`sep41 supply rollup advanced` for the affected contracts.

## Safety notes

- Nothing here is destructive: the `sep41_supply_events` write is additive
  (ON CONFLICT DO NOTHING) and the rollup reset preserves the genesis baseline.
- An unwatched contract gets a WARNING and recovers nothing: fix the list and
  re-run (idempotent).
- One heavy job at a time; every invocation ≤ 2M ledgers under `run-heavy-job.sh`.
