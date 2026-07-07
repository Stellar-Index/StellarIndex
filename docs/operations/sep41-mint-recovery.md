# SEP-41 dropped-mint recovery (scoped)

Recover a handful of SEP-41 supply rows (mint / burn / clawback) that a
**decoder bug dropped** — WITHOUT a full-history `ch-rebuild -sep41`
re-derive. The full re-derive processes ~370M events over hours and is
heavy enough to risk the kind of latency incident it is meant to avoid;
recovering a few hundred dropped rows does not need it.

This is the SEP-41 analogue of the SDEX targeted paths (`-sdex-gaps` /
`-sdex-reconcile`): a **scoped, additive** recovery that reads only the
affected contracts' events and idempotently ADDS the missing rows.

## When to use this (vs. the full re-derive)

Use the **scoped** path here when:

- The current `sep41_supply_events` data is **post-migration-0057 clean**
  (no pre-0057 collapsed rows in the affected range), AND
- A forward decoder fix has already landed, AND
- Only a bounded set of contracts / rows is missing.

Use the **full** `ch-rebuild -sep41` truncate+re-derive (see its flag help
and `docs/operations/adr-0033-data-recovery.md`) instead when you must
rebuild whole history or purge pre-0057 collapsed rows — that path
TRUNCATEs `sep41_transfers` + `sep41_supply_events` first.

## Why it is safe (idempotent + additive)

The write path is `store.CopyMergeSEP41SupplyEvents` → `INSERT … ON
CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at) DO
NOTHING`. Re-writing a row that already exists is a **no-op**; only the
rows the bug dropped are inserted. `-contracts` narrows only the *read*
(the CH `contract_id` prefilter) — it never changes decode logic, so
i128 discipline (ADR-0003: NUMERIC / `*big.Int`, never `int64`) is
preserved. Running the recovery twice produces the same result as running
it once.

## Worked example (2026-07-06 dropped-mints)

Decoder bug (fixed in `632d06f9`) dropped **336 CAP-67 map-shaped mint
events** across **8 of 15 watched contracts** — those contracts read
`mint_total = 0`, so `burn_total > mint_total` tripped the aggregator's
dominant-burn guard (`stellarindex_aggregator_supply_refresh_error_dominant`,
firing ×15). The steps below recover those rows.

---

## 0. Preconditions

- The **forward decoder fix is deployed** to r1 (otherwise the re-derive
  re-drops the same rows — verify the running `stellarindex-indexer` /
  `stellarindex-ops` binary post-dates the fix).
- One heavy job at a time (CLAUDE.md): confirm nothing else heavy is
  running — `pgrep -af "stellarindex-ops (backfill|ch-rebuild|census)"`.
- Source the runtime env on r1 (provides `$STELLARINDEX_POSTGRES_DSN` +
  the S3 / ClickHouse creds):

  ```sh
  set -a; . /etc/default/stellarindex; set +a
  ```

## 1. Identify the affected contracts

The dominant-burn guard fires exactly when a contract's rollup shows
`burn_total > mint_total`. List them from the served tier:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -F, -c "
  SELECT contract_id
    FROM sep41_supply_rollup
   WHERE burn_total > mint_total
   ORDER BY contract_id;"
```

Capture the output as a comma-separated list, e.g.:

```sh
AFFECTED="CBH4M45T...OCKF,CDLZFC3S...YSC,CCW67TSZ...MI75"   # the 8 contracts
```

> These MUST be a subset of `[supply] watched_sep41_contracts`. The
> sep41 decoders gate `Matches()` on the full watched set, so a contract
> outside it is read but decoded to nothing (ch-rebuild prints a WARNING
> for any such entry). If a contract shows `burn>mint` but is NOT watched,
> that is a different problem — do not force it through this path.

## 2. Scoped, windowed re-derive (under the heavy-job wrapper)

Read ONLY the affected contracts' **supply** events from the lake and
write the missing rows. `-contracts` narrows the CH `contract_id`
prefilter to the 8 contracts; `-sep41-supply-only` narrows the read to
the supply topics (`mint`/`burn`/`clawback`), skipping the transfer
firehose at the SQL layer so a high-transfer-volume contract's few mints
do not drag in millions of transfer rows; `-sources sep41_supply`
restricts the pass to the supply source (required by `-sep41-supply-only`).

Window each invocation to **≤ 2,000,000 ledgers** (the tool enforces this
— the event/sep41 passes buffer a window in-process; a 2026-07-05 unwindowed
run swapped galexie's captive core into a wedge). The Soroban era starts
at **50,457,424**; loop from there to the current tip. Run every
invocation under `run-heavy-job.sh` (mandatory on r1):

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

Loop the windows (dry-run first — omit `-write` — to eyeball the buffered
counts):

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

For the 2026-07-06 case the dropped mints all sit at/above the re-derive
coverage boundary (≈ ledger 50,457,424+), so a single 2M window covering
that boundary — or a couple of windows — recovers all 336 rows; you do
not need to sweep to the tip if you know the affected range.

## 3. Re-seed the supply rollup

`sep41_supply_rollup` (migration 0085) is an INCREMENTAL checkpoint: the
aggregator's rollup worker only folds rows with `ledger > last_ledger`.
The recovery adds rows BELOW the existing checkpoint, so the worker will
**not** pick them up on its own — the rollup must be re-seeded so it
re-folds from zero (documented on `AdvanceSEP41SupplyRollup` +
migration 0085).

**Scoped (recommended)** — drop only the affected rows; the worker
re-folds each from zero on its next pass, and meanwhile the reader falls
back to the exact full-sum (so correctness is restored immediately, the
fast path is restored shortly after):

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -c "
  DELETE FROM sep41_supply_rollup
   WHERE contract_id IN ('CBH4M45T...OCKF','CDLZFC3S...YSC','CCW67TSZ...MI75');"
```

**Broad (simpler, matches the migration-0085 contract)** — truncate the
whole rollup; every watched contract briefly uses the full-sum fallback
until the worker re-folds. Prefer the scoped DELETE unless most contracts
are affected. TRUNCATE may need the table owner:

```sh
sudo -u postgres psql -d stellarindex -c "TRUNCATE sep41_supply_rollup;"
```

The aggregator's `runSEP41SupplyRollup` worker re-folds on its next
cadence (`[supply] aggregator_refresh_cadence`) — sequentially, one
contract at a time, to avoid the concurrent full-scans that caused the
2026-07-06 p95 incident. No restart is required; to force an immediate
re-fold, restart `stellarindex-aggregator` (its first pass warms the
checkpoint before the refresher reads it).

## 4. Verify

Confirm the affected contracts now read `mint_total > 0` and
`mint_total ≥ burn_total`:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -c "
  SELECT contract_id, mint_total, burn_total, clawback_total, last_ledger
    FROM sep41_supply_rollup
   WHERE contract_id IN ('CBH4M45T...OCKF','CDLZFC3S...YSC','CCW67TSZ...MI75')
   ORDER BY contract_id;"
```

Cross-check the row was actually written (per-contract mint count in the
served events table):

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -c "
  SELECT contract_id, count(*)
    FROM sep41_supply_events
   WHERE event_kind = 'mint'
     AND contract_id IN ('CBH4M45T...OCKF','CDLZFC3S...YSC','CCW67TSZ...MI75')
   GROUP BY contract_id ORDER BY contract_id;"
```

Then confirm the alert clears: `stellarindex_aggregator_supply_refresh_error_dominant`
should stop firing (all 15 instances resolve) within a couple of
aggregator refresh cadences once no watched contract reports
`burn_total > mint_total`. Watch the aggregator log for
`sep41 supply rollup advanced` lines for the affected contracts.

## Rollback / safety notes

- Nothing here is destructive to `sep41_supply_events` — the write is
  purely additive (ON CONFLICT DO NOTHING). The only DELETE/TRUNCATE is
  against `sep41_supply_rollup`, which is a **derived** checkpoint the
  worker rebuilds from `sep41_supply_events`; losing it costs only a
  re-fold, never source data.
- If you passed a contract outside the watched set, ch-rebuild prints a
  WARNING and recovers nothing for it — fix the list and re-run (still
  idempotent).
- Keep to one heavy job at a time; window every invocation ≤ 2M ledgers
  under `run-heavy-job.sh`.
