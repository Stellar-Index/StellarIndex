---
title: Runbook — entry-walk renumbering (intra_ledger_seq walk-version bump)
last_verified: 2026-07-26
status: draft
severity: P2
---

# Runbook — entry-walk renumbering

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | None — this is a **deploy-time procedure**, not a paging condition. It runs once per `dispatcher.EntryWalkVersion` bump. |
| Trigger | A release changes the order in which `dispatcher.walkLedgerEntryChanges` / `clickhouse.extractLedgerEntryChanges` emit changes, i.e. `EntryWalkVersion` is incremented. Current value: **2** (C2-032 / C2-023 / C2-040 / R-A01-1, audit-2026-07-23). |
| Typical MTTR | Not an incident. Budget the re-derive time for the affected range. |
| Impact if skipped | Balance observations and `ledger_entries_current_v2` rows written by the OLD walk keep a position from a numbering that no longer exists. A corrective re-derive is **silently discarded** by the `intra_ledger_seq` guard — no error, no metric, no retry that helps. |
| Impact if done WRONG | Worse than skipping. The seed writes at `MaxUint32`, which nothing can outrank; seeding from an un-repaired source makes a stale balance **permanently unfixable**. Read the repair path in order. |

## What this is

`intra_ledger_seq` is a position within one ledger's entry-change walk. Two
guards compare it **across binary versions**:

- Postgres — `account_observations` and its four siblings:
  `... ON CONFLICT (...) DO UPDATE SET ... WHERE <t>.intra_ledger_seq <= EXCLUDED.intra_ledger_seq`
  (migration 0111);
- ClickHouse — `ledger_entries_current_v2`'s ReplacingMergeTree version
  `(ledger_seq << 32) | intra_ledger_seq`.

Both assume the two positions are drawn from the **same numbering**. Bumping
`EntryWalkVersion` renumbers every ledger, so that assumption breaks and a
legacy row can outrank its own correction.

### The concrete failure

Walk v1 (per-transaction: fee, apply, next tx's fee) gave account A's
ledger-final balance position **6** — that *was* the C2-032 corruption: a
fee-phase change sorted last. Walk v2 (ledger-wide: all fees, all apply-phase,
all post-apply fees) correctly places that same final balance at position
**3**.

A corrective re-derive writes 3. The guard evaluates `6 <= 3` → **false**. The
correction is dropped. Re-running changes nothing: the walk is deterministic
and recomputes 3 every time.

The ClickHouse side fails the same way — a version built from a *lower*
`intra_ledger_seq` never displaces a higher one under `FINAL`.

## The repair path

**Reconstruct-final-then-seed. Not replay-the-changes.**

> **The single most dangerous mistake on this page is doing step 2 before
> step 1 has genuinely finished.** The seed writes at `MaxUint32`, which by
> construction nothing can outrank. Seeding a *stale* balance there makes it
> permanently unbeatable — strictly worse than the corruption you started
> with, and not fixable by any later re-derive. Do not start step 2 until
> step 1 verifies.

### Why the obvious moves do not work

Two ClickHouse facts drive the whole procedure:

| Table | Engine | Partitioning |
| --- | --- | --- |
| `stellar.ledger_entry_changes` (append log) | `ReplacingMergeTree(ingested_at)` | `PARTITION BY intDiv(ledger_seq, 1000000)` |
| `stellar.ledger_entries_current` / `_v2` (current-state projection) | `ReplacingMergeTree(version)`, `version = (ledger_seq << 32) \| intra_ledger_seq` | **none — `ORDER BY (entry_type, key_xdr)` only** |

- **"Drop the affected partitions" is not available for the projection.** It
  is unpartitioned. `DROP PARTITION` reaches only the append log.
- **The append log does not need a drop.** Its RMT version is `ingested_at`,
  so a later replay wins on its own — a re-derive there is
  idempotent-corrective with no truncate.
- **Reprojecting alone does NOT repair the projection.** This is the trap.
  The projection's version embeds `intra_ledger_seq`, and the whole premise
  of a walk-version bump is that the corrected position is *lower*. A
  reproject inserts `(L << 32) | 3`, the stale row is `(L << 32) | 6`, and
  `FINAL` keeps 6. The corrected row is there and loses, silently — the same
  failure as the Postgres guard, one layer down.

So the projection's stale rows must be **removed**, not merely out-written.

### 1. Repair the append log (`ledger_entry_changes`)

```
stellarindex-ops ch-backfill -config /etc/stellarindex.toml -from {N} -to {M}
```

`ch-backfill` is the writer for the Tier-1 `stellar.*` tables including
`ledger_entry_changes`. (Not `ch-rebuild` — that rebuilds the
contract-calls / SEP-41 projections and does not touch this table.) Windowed,
under `run-heavy-job.sh`, off-peak. Idempotent-corrective, so overlapping
windows are safe.

### 2. Rebuild the current-state projection

Required only if you intend to run the **accounts** seed (step 3b) or serve
`account_state` / asset-holder reads over the range. Pick one:

**Option A — targeted delete + reproject.** For a bounded range:

```sql
ALTER TABLE stellar.ledger_entries_current_v2
  DELETE WHERE ledger_seq BETWEEN {N} AND {M};
```

then re-run the Step-2 windowed `INSERT … SELECT` from
`deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`:

```sql
INSERT INTO stellar.ledger_entries_current_v2
    (entry_type, key_xdr, account_id, asset, balance, change_type,
     ledger_seq, close_time, entry_xdr, intra_ledger_seq)
SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
       ledger_seq, close_time, entry_xdr, intra_ledger_seq
FROM stellar.ledger_entry_changes
WHERE ledger_seq >= {window_start} AND ledger_seq < {window_end};
```

`ALTER … DELETE` is a mutation — heavyweight, asynchronous. Watch
`system.mutations` to completion **before** the INSERT, or the delete will
eat rows the reproject just wrote. Keys whose latest change falls outside
the window are untouched, which is correct.

**Option B — full side-by-side rebuild.** For a large or
whole-history range, follow
`deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` end to end
(build `_v3` + MV, windowed reproject, verify, cutover). That file is the
codified pattern; it exists because ClickHouse has no
`ALTER TABLE … MODIFY ENGINE` and it is the cheaper path once the mutation
would rewrite most of the table.

### 3. Reconstruct the FINAL state and seed it

Write **one row per key per ledger** stamped `timescale.SeedIntraLedgerSeq`
(`4294967295` = `math.MaxUint32`). The `<=` guard always admits the
sentinel, and a re-run re-admits it, so the repair is
**idempotent-corrective**.

**3a. SAC balances — use the full-history variant:**

```
stellarindex-ops supply-seed-sac -config /etc/stellarindex.toml -full-history
```

`-full-history` switches the read to
`clickhouse.StreamSACBalanceSeedsFullHistory`, which reduces
`stellar.ledger_entry_changes` directly with an `argMax` over
`(ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)`. **It never
reads the projection**, so it is correct as soon as step 1 verifies — step 2
is not a prerequisite for this one.

**Without** `-full-history` the seed reads `ledger_entries_current FINAL`,
i.e. the un-repaired projection. Do not use the default variant here.

**3b. Account balances — projection-dependent, no escape hatch:**

```
stellarindex-ops supply-seed -config /etc/stellarindex.toml
```

`LatestAccountEntrySeed` reads `stellar.ledger_entries_current FINAL` and
has **no full-history variant**. It is therefore only as correct as step 2
left the projection. **Running it against an un-rebuilt projection stamps a
stale balance at `MaxUint32`, permanently.** Do not run 3b until step 2 has
verified.

Both seeds write current state (one row per account at its latest change's
ledger), which is what the served supply consumes. A per-ledger *historical*
repair needs the same rule — final state per `(key, ledger)`, one row,
sentinel-stamped — and no tool emits that today; see Residual risk.

### The one way to get this wrong

**Do not stamp the sentinel on a change-by-change replay.** It is sound only
for a reconstructed FINAL state — exactly one row per `(key, ledger)`. If
every change in a ledger carries `MaxUint32`, the `<=` guard admits any of
them in any order, and the C2-6 out-of-order last-writer-wins bug this column
exists to close is re-opened.

## Verification

**Verify against the append log, never against the projection.** The
projection is what fed the accounts seed; comparing an observation to its own
source proves nothing and will agree even when both are stale.

The reference reduction is the same one `StreamSACBalanceSeedsFullHistory`
uses (`internal/storage/clickhouse/sac_balance_seed.go`) — the `argMax`
tuple is the load-bearing part and must match exactly:

```sql
SELECT key_xdr,
       argMax(entry_xdr,   (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_entry_xdr,
       argMax(change_type, (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_change_type,
       argMax(ledger_seq,  (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_ledger_seq
FROM stellar.ledger_entry_changes
WHERE entry_type = 'account'
  AND ledger_seq BETWEEN {N} AND {M}
GROUP BY key_xdr;
```

1. **Step 1 verified?** Pick a key whose ledger has both a fee-phase and an
   apply-phase change (a Soroban fee-source account on a P23 ledger has all
   three phases). Its `intra_ledger_seq` values in `ledger_entry_changes`
   must be dense and phase-ordered, and non-zero — a legacy row reads
   `intra_ledger_seq = 0`, which is the tell that `ch-backfill` has not
   covered it yet.
2. **Step 2 verified?** For the same keys, `ledger_entries_current_v2 FINAL`
   must agree with the `argMax` reduction above. Disagreement means stale
   projection rows survived — the mutation did not complete, or the reproject
   ran before it did.
3. **Step 3 verified?** `account_observations.balance_stroops` for the
   `(account, ledger)` must equal the `argMax` reduction's winner. Compare to
   the reduction, **not** to `ledger_entries_current`.
4. `SELECT count(*) FROM account_observations WHERE intra_ledger_seq = 4294967295`
   should equal the number of `(key, ledger)` pairs you seeded — not more.
5. Run `stellarindex-ops compute-completeness` for the range; a residual
   substrate/projection mismatch means step 1 did not fully replay.

## Do NOT

- **Do not widen the guard to `<` or drop it** to let corrections through.
  That re-opens C2-6 (an out-of-order `PersistEvents` worker persisting a
  stale intra-ledger balance) — a strictly worse bug, and one that fires
  continuously rather than once per walk-version bump.
- **Do not re-run the change-replay re-derive "harder".** It is deterministic;
  the second run computes the same position the guard already rejected.
- **Do not run `supply-seed` (accounts) before the projection rebuild has
  verified**, and **do not run `supply-seed-sac` without `-full-history`**
  here. Both read `ledger_entries_current FINAL`; seeding a stale value at
  `MaxUint32` is permanent.
- **Do not treat a reproject as a projection repair.** The projection is
  unpartitioned and its RMT version embeds `intra_ledger_seq`, so a corrected
  (lower) version loses to the stale row. The stale rows must be deleted or
  the table rebuilt.
- **Do not verify against `ledger_entries_current`.** It is the projection the
  accounts seed read; it will agree with the observation it produced whether
  or not either is correct. Reduce `ledger_entry_changes` with the `argMax`
  tuple instead.
- **Do not collapse `derive_generation` and `intra_ledger_seq` into one
  column** if generation is ever added to these tables. Migration 0111 and
  0120 both specify the lexicographic `(generation, seq)` ROW comparison, with
  generation as the OUTER key.

## Related

- [Migration 0120](../../../migrations/0120_intra_ledger_seq_walk_version.up.sql)
  — the amendment that states this invariant on the live schema.
- [supply-divergence](supply-divergence.md) — where a missed repair surfaces:
  the classic-vs-SAC cross-check widens when an observation component is
  stale.
- [projector-lag](projector-lag.md) — the re-derive itself runs through the
  projector path; watch it while a range replays.
- `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` — the
  codified side-by-side projection rebuild (Option B) and the Step-2
  windowed reproject this page cites.
- `internal/storage/clickhouse/sac_balance_seed.go` —
  `StreamSACBalanceSeedsFullHistory`, the append-log `argMax` reduction the
  verification queries mirror.

## Residual risk

`supply-seed` / `supply-seed-sac` seed CURRENT state only — one row per key
at its latest change's ledger. There is no tool today that emits a per-ledger
historical final state for a range, so a historical observation repair over a
window is specified here but not yet executable end to end. The served-supply
surface (what the money path reads) IS covered by the current-state seeds.
