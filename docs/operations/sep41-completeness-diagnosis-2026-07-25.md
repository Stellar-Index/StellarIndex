---
title: SEP-41 completeness diagnosis — why sep41_supply / sep41_transfers sit at complete=false
last_verified: 2026-07-25
status: living
---

# SEP-41 completeness diagnosis (2026-07-25)

`sep41_supply` and `sep41_transfers` report `complete=false` with
`lake_complete=true` on `/v1/coverage`. Triage named three candidate
drivers. This document re-derives the verdict computation against the
code as it stands at `main`, **rules two of the three candidates out
structurally** (no live data needed), names the exact predicate that
must be failing, and gives the operator the discriminating queries for
the one candidate family that remains.

It also retires
[`sep41-watched-set-decision.md`](sep41-watched-set-decision.md), whose
premise (`watched_sep41_contracts = []`) went stale on 2026-07-05.

> **No tool was built.** The triage's standing instruction — do not
> build the topic-recovery tool on an unconfirmed cause — is upheld:
> the cause it would address is ruled out below, so the tool is not the
> remedy and remains correctly absent (migration 0114's corrected
> header already says so).

---

## 0. TL;DR — the verdict per source

| Source | `lake_complete` | `complete` | Failing predicate | Driver |
| ------ | --------------- | ---------- | ----------------- | ------ |
| `sep41_supply` | true | **false** | `projOK == false` — `compute_completeness.go:377` + `:476` | candidate (c), reconcile-scope semantics. One of five sub-predicates; **the persisted `detail` column says which.** |
| `sep41_transfers` | true | **false** | identical | identical |

**Both sources fail on the same axis and it can only be the projection
axis.** `complete` and `lake_complete` are computed from disjoint
inputs, so `lake_complete=true ∧ complete=false` has exactly one
solution. No query is needed to establish this — see §1.

**Candidate (a), the C2-11 >4-topic truncation: RULED OUT** (§2.1) —
twice over, structurally.
**Candidate (b), the missing pre-Soroban genesis baseline: RULED OUT**
(§2.2) — it writes a different table than the one the verdict reads.
It *is* a real, currently-live defect with a different symptom, and it
still needs running (§5, step 0).
**Candidate (c), reconcile-scope semantics: the only survivor** (§3),
decomposed into five sub-predicates, of which one — the INV-5 latch —
is **provably engaged right now on the hourly timer** and is why the
verdict cannot clear itself.

---

## 1. The failing predicate, exactly

`stellarindex-ops compute-completeness` writes both axes from the same
loop iteration
(`internal/ops/chops/compute_completeness.go:399-408`):

```go
lakeComplete := srW.Complete            // :325  substrate ∧ recognition ONLY
...
w = combineWatermark(srW, projOK)       // :377
...
Watermark: w.Ledger, ..., Complete: w.Complete,   // :401
LakeComplete: lakeComplete,                       // :402
```

and `combineWatermark` (`:474-478`) is:

```go
w.Complete = srW.Complete && projOK
```

So `complete = lake_complete ∧ projOK`. Given `lake_complete = true`:

> **`complete=false` ⟺ `projOK == false`. There is no other term.**

Substrate (Claim 1) and recognition (Claim 2a) are therefore both
**passing** for these two sources. Anything that would have to act
through substrate or recognition is excluded before we look at a single
row. That is what kills candidates (a) and (b).

`projOK` is set in exactly two places for a `-ch` run:

| # | Site | Condition | `detail` text it writes |
| - | ---- | --------- | ----------------------- |
| P1 | `projectionClaim` `:770` | this run's reconcile found a real mismatch (`delta != 0`) | `projection: <table>: N mismatched ledger(s), Σ|Δ|=…, first: ledger=… expected=… served=…` |
| P2 | `projectionClaim` `:779` | run was incremental **and** no prior verdict exists | `…no prior verdict exists to carry…` |
| P3 | `projectionClaim` `:781` | run was incremental **and** the prior verdict's projection was FAILING | `…the prior verdict's projection was FAILING — refusing to upgrade without evidence (re-run without -from)` |
| P4 | `projectionClaim` `:783` | run was incremental **and** the prior clean verdict is not contiguous | `…leaving [a,b] verified by nobody…` |
| P5 | `:367-370` | `detectFloorLoss` found served rows below the durable floor (migration 0116) | `projection: <table> now begins at ledger N but was previously verified from M…` |

**The `detail` column already contains the answer.** It is the single
most valuable artifact here and it is one query away (§4, Q1).

### 1.1 The latch — why this verdict cannot clear itself

`scripts/ops/completeness-incremental.sh:31` computes

```sh
FROM=$(psql -tAc "SELECT COALESCE(min(watermark),0) FROM completeness_snapshots WHERE source <> 'recognition'")
```

and passes it as `-from`. But `watermark_ledger` is the **lake** axis —
`combineWatermark` copies `srW.Ledger` through untouched and only
narrows `Complete` — so when the lake is clean every source's watermark
sits **at tip**. The hourly run therefore reconciles roughly
`[tip, tip]`, and for every source `runFrom > servedFrom`.

For a source whose prior verdict has `projection_ok = true`, rule 3
carries it forward and it stays green. For a source whose prior verdict
has `projection_ok = false` — our two — **P3 fires unconditionally,
every hour, forever.**

> **Consequence, and it is the operationally important one:** even if
> the underlying data defect were fully repaired right now, the hourly
> timer *structurally cannot* flip these two sources back to
> `complete=true`. Only a **full** run (`compute-completeness -ch`
> without `-from`, so `runFrom <= servedFrom` and rule 2 applies) can
> clear a failing projection verdict. This is deliberate INV-5 design,
> not a bug — but it means "still red" carries **zero** information
> about current data health until a full run has been done.

> **Second consequence, and it is the diagnostic one:** each hourly run
> **overwrites `detail`**. If the latch has been engaged for a while,
> `detail` now reads the P3 text and the ORIGINAL trip cause (the P1
> first-mismatch ledger + expected/served counts) has been erased. §4
> Q1 tells you which state you are in; §5 recovers the original cause.

---

## 2. The two candidates that are ruled out structurally

### 2.1 Candidate (a) — the C2-11 >4-topic truncation era: RULED OUT

Two independent reasons, either sufficient.

**(i) SEP-41 events never had a topic to lose.** The truncation lost
topics at index **≥ 4** only (migration 0114 header; the pre-fix
`decodeTopics` filled `topic_0_xdr .. topic_3_xdr`). The SEP-41
decoders read at most `topic[2]`:

- `internal/sources/sep41_supply/decode.go:136-153` — `decodeCounterparty`
  reads `topic[1]`, and `topic[2]` only as the legacy-vs-CAP-67
  discriminator.
- `internal/sources/sep41_transfers/decode.go:45-140` — `decodeTransfer`
  and `decodeApprove` read `topic[1]`, `topic[2]`; `decodeSetAdmin` and
  `decodeSetAuthorized` read at most `topic[1]`.

The widest SEP-41 shape on the wire is the CAP-67 4-topic form
(`["transfer", from, to, sep0011_asset]`) — indices 0..3, all four of
which the pre-0114 schema retained, and the 4th of which no decoder
even reads. **Nothing was ever truncated for these two sources.**

**(ii) The verdict never reads `soroban_events` anyway.** The deployed
completeness path is `-ch` (`completeness-incremental.sh:35`). In that
path:

- the *expected* side comes from `clickhouse.ReconcileEventStreamer`
  (`compute_completeness.go:336`) →
  `StreamContractEventsFiltered` → `SELECT … topics_xdr … FROM
  stellar.contract_events` (`event_reader.go:189-196`);
- the *served* side comes from `store.CountRowsByLedger` over
  `sep41_transfers` / `sep41_supply_events`.

`soroban_events` — the table 0114 fixed — appears in **neither** leg.
And the ClickHouse lake was never truncated in the first place:
`extract.go:eventRow` writes the complete `TopicsXDR` array, and 0114
itself says the lake "is topic-complete to genesis (ADR-0034)".

> **Therefore the topic-recovery tool would not move this verdict.**
> Do not build it for this reason. (It may still be wanted for the
> genuine 5+-topic Aquarius rows in `soroban_events`; that is a
> separate, narrow, non-SEP-41 concern.)

### 2.2 Candidate (b) — the missing pre-Soroban genesis baseline: RULED OUT

`stellarindex-ops supply seed-sep41-genesis` writes **`sep41_supply_rollup`**
— the `genesis_mint_total` / `genesis_burn_total` /
`genesis_clawback_total` / `genesis_baseline_ledger` columns
(`internal/ops/supply/supply_sep41_genesis_seed.go:166` →
`timescale.UpsertSEP41GenesisBaseline`,
`internal/storage/timescale/sep41_supply_events.go:353-364`; migration
0088).

The projection reconcile counts **rows in `sep41_supply_events` and
`sep41_transfers`**
(`internal/ops/chops/reconciliation_catalogue.go:370, 375`).

**The two tables are disjoint.** The genesis baseline is a per-contract
opening-balance row; it adds no event rows and removes none. It cannot
move a per-ledger row-count delta in either direction, and it cannot
touch substrate or recognition either.

What the missing baseline *does* drive is a **different symptom on a
different surface**: `supply.ErrNegativeTotalMissingBaseline`
(`internal/supply/sep41.go:131`) and the refresher's
`OutcomeKindMissingBaseline` (`internal/supply/refresher.go:53`) — a
SAC wrapper issued before Soroban reads `Σburn > Σmint` over the
Soroban-era-only window, the negative-total guard rejects the snapshot,
and the supply-refresh error names the command. That is the alert
operators keep seeing, and it is real and still open — it is simply
**not** the completeness driver. §5 step 0 sequences it anyway,
because it is cheap, idempotent, and independently needed.

---

## 3. Candidate (c) — reconcile-scope semantics: the surviving family

Five sub-predicates, P1–P5 from §1. P3 (the latch) is provably engaged
under the hourly timer once *anything* has tripped P1/P2/P4/P5 even
once. So the question splits in two:

1. **Is the latch the only thing still holding it red?** → run a full
   reconcile (§5 step 2).
2. **If the full reconcile also fails (P1), where is the first
   mismatch?** → the ledger discriminates the underlying cause:

| First-mismatch ledger | Most likely cause | Confirming query |
| --------------------- | ----------------- | ---------------- |
| ≈ 63,420,000 and above, contiguous to a later ledger | **Truncate-boundary hole.** The 2026-07-11 `ch-rebuild -sep41 -write` re-derived windows 50.0M→63.42M *after* a TRUNCATE. If the live tip was above 63.42M at truncate time, rows in `(63.42M, tip_at_truncate]` were destroyed and never re-derived. | Q4 |
| scattered, `served > expected` | **Rows for contracts outside the current watched set.** The reconcile targets carry `whereFilter: ""` (`reconciliation_catalogue.go:370, 375`) — "the whole table belongs to this source" — while the expected side is prefiltered to `cfg.Supply.WatchedSEP41Contracts` (`:369, 374`). Any served row for an un-watched contract is an unmatched surplus, permanently. | Q3 |
| scattered, `expected > served` | **Failed-transaction events counted as expected.** `dispatcher.go:598` and `census.go:94` both skip `!tx.Result.Successful()`; `clickhouse/extract.go:117` calls `extractEvents` with **no success gate** and hard-codes `InSuccessfulCall: 1` (`extract.go:317`), and the reconcile stream applies no `in_successful_call` predicate. Would affect every event source, so treat a non-zero Q5 as significant only if other sources are also red. | Q5 |
| at/near a source's `MIN(ledger)` with a P5 detail line | **Durable-floor loss** (migration 0116). | Q6 |

### 3.1 A note on the watched set, and why the decision doc is stale

[`sep41-watched-set-decision.md`](sep41-watched-set-decision.md) opens
with "`watched_sep41_contracts = []` on r1 — the sep41_transfers +
sep41_supply sources are config-disabled". **That has been false since
2026-07-05.** `configs/ansible/roles/archival-node/defaults/main.yml:771`
now carries **39** contracts (`4290aae6` seeded the curated SAC-wrapper
set on 2026-07-05; `bba4f1f9` + `332016e0` added FxDAO FXG on
2026-07-10). Every recommendation in that doc has either been executed
or superseded. It is retired by this document — see the banner added to
it.

Timeline that matters for §3's first row: the watched set last changed
**2026-07-10**, and the full-history truncate+re-derive ran
**2026-07-11** over 50.0M→63.42M. So the re-derive *did* cover all 39
contracts, which is why "a contract was added after the re-derive and
its history was never captured" is **not** on the candidate list — the
git history rules it out. The unresolved question is the *upper* edge
of that rebuild window, not the contract set.

---

## 4. Discriminating queries — OPERATOR-VERIFY

None of these can be answered from the repo. Run them on r1 and record
the output back into this document.

### Q1 — the current verdict + the detail string (**run this first**)

```sql
-- Postgres (STELLARINDEX_POSTGRES_DSN)
SELECT source, complete, lake_complete,
       substrate_ok, recognition_ok, projection_ok,
       genesis, watermark, tip, first_problem, updated_at,
       detail
  FROM completeness_snapshots
 WHERE source IN ('sep41_supply', 'sep41_transfers')
 ORDER BY source;
```

Read `detail` against the P1–P5 table in §1:

- contains `the prior verdict's projection was FAILING` → **P3, the
  latch is engaged**; the original cause has been overwritten. Go to §5
  step 2 — a full run is both the diagnosis and the only possible cure.
- contains `mismatched ledger(s), Σ|Δ|=` → **P1**, a live mismatch.
  Record `first: ledger=… expected=… served=…` and take it to §3's
  discriminator table.
- contains `now begins at ledger … but was previously verified from …`
  → **P5**, served-tier loss. Escalate: that is data deletion, not a
  scope artifact.
- contains `no prior verdict exists` → **P2**, first-run state.

Confirm the axes agree with this diagnosis: `substrate_ok` and
`recognition_ok` must both be `true` and `projection_ok` must be
`false`. If not, this whole document's premise is wrong and the
`lake_complete=true` report was stale — stop and re-triage.

### Q2 — is the incremental timer the only thing that has run?

```sql
SELECT source, projection_ok, tip, updated_at
  FROM completeness_snapshots
 ORDER BY updated_at DESC;
```

If every row's `updated_at` is within the last hour and only the two
sep41 rows have `projection_ok = false`, the latch (§1.1) is the
operative state.

### Q3 — served rows for contracts NOT in the current watched set

The `whereFilter: ""` asymmetry. Substitute the 39 C-strkeys from
`stellarindex_watched_sep41_contracts` (or read them from the deployed
`/etc/stellarindex.toml`, `[supply] watched_sep41_contracts`).

```sql
-- expect ZERO rows; any row is a permanent served > expected surplus
SELECT 'sep41_transfers' AS tbl, contract_id, count(*) AS rows,
       min(ledger) AS min_ledger, max(ledger) AS max_ledger
  FROM sep41_transfers
 WHERE contract_id <> ALL (ARRAY[ '<C1>', '<C2>', ... ]::text[])
 GROUP BY 1, 2
UNION ALL
SELECT 'sep41_supply_events', contract_id, count(*),
       min(ledger), max(ledger)
  FROM sep41_supply_events
 WHERE contract_id <> ALL (ARRAY[ '<C1>', '<C2>', ... ]::text[])
 GROUP BY 1, 2
 ORDER BY 3 DESC;
```

### Q4 — the 2026-07-11 truncate/re-derive upper boundary

Look for a coverage cliff just above 63,420,000. `sep41_transfers` is
the higher-volume table and shows it more clearly.

```sql
-- per-100k-ledger row counts across the suspected boundary
SELECT (ledger / 100000) * 100000 AS bucket, count(*) AS rows
  FROM sep41_transfers
 WHERE ledger BETWEEN 63000000 AND 64500000
 GROUP BY 1
 ORDER BY 1;

-- same for the supply table
SELECT (ledger / 100000) * 100000 AS bucket, count(*) AS rows
  FROM sep41_supply_events
 WHERE ledger BETWEEN 63000000 AND 64500000
 GROUP BY 1
 ORDER BY 1;
```

A run of zero/near-zero buckets starting at ~63.4M and resuming at the
ledger where live capture restarted is the signature. Cross-check the
lake, which is authoritative and was never truncated:

```sql
-- ClickHouse (clickhouse-client --port 9300)
SELECT intDiv(ledger_seq, 100000) * 100000 AS bucket, count() AS events
  FROM stellar.contract_events
 WHERE ledger_seq BETWEEN 63000000 AND 64500000
   AND contract_id IN ( '<C1>', '<C2>', ... )
   AND topic_0_sym IN ('transfer','approve','set_admin','set_authorized')
 GROUP BY bucket
 ORDER BY bucket;
```

Lake non-zero where Postgres is zero ⟹ the hole is real and is a
served-tier gap, not an absence of activity.

### Q5 — failed-transaction events inflating the expected side

```sql
-- ClickHouse. Expect 0. Non-zero = the expected side over-counts by
-- exactly this many rows, because the live dispatcher skipped these
-- txs (dispatcher.go:598) but extract.go did not.
SELECT count() AS failed_tx_sep41_events
  FROM stellar.contract_events AS e
 INNER JOIN (
       SELECT ledger_seq, tx_hash
         FROM stellar.transactions
        WHERE successful = 0
          AND ledger_seq BETWEEN 50457424 AND <tip>
 ) AS f USING (ledger_seq, tx_hash)
 WHERE e.ledger_seq BETWEEN 50457424 AND <tip>
   AND e.contract_id IN ( '<C1>', '<C2>', ... )
   AND e.topic_0_sym IN ('transfer','approve','set_admin','set_authorized','mint','burn','clawback');
```

### Q6 — durable projection floors for these targets (migration 0116)

```sql
SELECT source, target_table, target_filter,
       projection_verified_from, first_recorded_at, updated_at
  FROM completeness_target_floors
 WHERE source IN ('sep41_supply', 'sep41_transfers');
```

No rows ⟹ no floor has ever been recorded, so P5 cannot be the cause
(`detectFloorLoss` skips unknown targets by design) — and note that a
floor is only recorded after a **clean** reconcile
(`compute_completeness.go:356-360`), so an absence here is itself
evidence these two have never had one.

Compare against the live bottom edges:

```sql
SELECT 'sep41_transfers' AS tbl, min(ledger), max(ledger), count(*) FROM sep41_transfers
UNION ALL
SELECT 'sep41_supply_events', min(ledger), max(ledger), count(*) FROM sep41_supply_events;
```

### Q7 — genesis-baseline state (candidate (b): different symptom, still needs fixing)

```sql
SELECT contract_id, genesis_baseline_ledger, genesis_seeded_at,
       genesis_mint_total, genesis_burn_total, genesis_clawback_total
  FROM sep41_supply_rollup
 ORDER BY (genesis_baseline_ledger IS NULL) DESC, contract_id;
```

`genesis_baseline_ledger IS NULL` ⟹ never seeded for that contract.

---

## 5. Remedy sequence

Ordered so each step is independently safe, and so no step depends on
an unproven cause.

### Step 0 — run the genesis seed (independent of everything above)

It is idempotent, read-mostly, cheap, and it fixes a real live defect
(the `missing_baseline` refresher outcome + the negative-total guard).
It does **not** affect the completeness verdict (§2.2) and must not be
credited with doing so.

```sh
# dry-run first — prints the per-contract pre-Soroban baselines
stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -dry-run
# then, if the numbers look sane:
stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml
```

Verify with Q7: every watched contract should have a non-NULL
`genesis_baseline_ledger` (= 50457424 by default).

### Step 1 — capture the current verdict BEFORE anything overwrites it

Run Q1 and paste the two `detail` strings into this document. The
hourly timer rewrites them; if you skip this you lose the only record
of the pre-remediation state.

Optionally mask the timer for the duration:
`systemctl stop stellarindex-completeness.timer`.

### Step 2 — full, source-scoped projection re-verify (**the diagnostic AND the only cure for the latch**)

This is the load-bearing step. It is the only run that can satisfy
`runFrom <= servedFrom` and therefore the only run that can either
clear the latch or produce an honest P1 first-mismatch ledger.

```sh
# one source at a time; each streams the watched contracts' events from
# Soroban genesis to tip in 250k-ledger windows.
nice -n 15 ionice -c2 -n7 stellarindex-ops compute-completeness \
    -config /etc/stellarindex.toml -ch \
    -source sep41_supply \
    -skip-substrate -skip-recognition

nice -n 15 ionice -c2 -n7 stellarindex-ops compute-completeness \
    -config /etc/stellarindex.toml -ch \
    -source sep41_transfers \
    -skip-substrate -skip-recognition
```

Why each flag:

- **no `-from`** — mandatory. With `-from` you get P3 again and learn
  nothing.
- `-source <name>` — scopes the write to one snapshot row; the other
  sources' verdicts are untouched. `validateSourceFilter`
  (`reconciliation_catalogue.go:320`) fails closed on a typo, so a
  misspelling errors rather than silently verifying nothing.
- `-skip-substrate -skip-recognition` — these two axes are already
  `true` (§1) and the global `DistinctTopicShapes` recognition scan is
  the load-heaviest step in the whole command. Skipping sets
  `substrate_ok`/`recognition_ok` to `true` **on trust, not evidence**
  (`compute_completeness.go:207-208, 456-459`) — acceptable here
  precisely because they are the axes we are not investigating, but
  note it: the resulting snapshot's substrate/recognition claim is
  carried, not re-proven. Drop both flags for a from-scratch
  certification.
- `nice`/`ionice` — mirrors `completeness-incremental.sh:35`. The
  full-history sep41 reconcile was one leg of the 2026-07-08 OOM
  series; the fix (250k windowing in
  `clickhouse/completeness.go:25-32`, plus `NeedOpArgs=false`) is in
  place, but stay gentle on the shared host.

Then re-run Q1. Two outcomes:

- **`complete=true`** ⟹ the data was already fine and the latch was the
  whole story. Nothing further to fix; restart the timer. Record this —
  it means the alert was stale, and it is the most likely outcome given
  the 2026-07-11 re-derive completed `rc=0`.
- **`complete=false` with a P1 detail** ⟹ a real gap. Take the
  `first: ledger=… expected=… served=…` triple to §3's discriminator
  table and run the matching query (Q3/Q4/Q5/Q6).

### Step 3 — repair, only once step 2 has named a ledger range

Do **not** run any of these speculatively.

- **Truncate-boundary hole (Q4 confirms):** a **scoped, additive**
  re-derive over exactly the missing range. No TRUNCATE — the write is
  `ON CONFLICT` corrective (migration 0110 generation-guarded), so it
  only adds the missing rows.

  ```sh
  stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
      -sep41 -write -from <hole_start> -to <hole_end>
  ```

  Mind the buffer-pass range guard (`ch_rebuild.go:245`): window the
  invocation and loop externally. `-write` on the supply source
  auto-resets the affected contracts' `sep41_supply_rollup` fold
  checkpoint (genesis baseline preserved), so the aggregator re-folds
  rather than double-counting — the KALE 2× bug.

- **Un-watched-contract surplus (Q3 confirms):** this is a **code**
  defect in the reconcile spec, not a data defect. The fix is to give
  the two `reconTarget`s a `whereFilter` scoping the served side to the
  watched set, matching how `trades` is split by
  `source = 'soroswap'`. That file is
  `internal/ops/chops/reconciliation_catalogue.go` — **out of this
  document's fence; see §7.** Do not "fix" it by deleting rows.

- **Failed-tx inflation (Q5 confirms):** also a code defect, in
  `internal/storage/clickhouse/extract.go` and/or the reconcile
  streamer — **out of fence; see §7.** Note it would affect every event
  source, so confirm the blast radius before touching it.

- **Floor loss (Q6 + a P5 detail):** served-tier rows were deleted.
  Escalate; do not re-record the floor (`recordFloors` deliberately
  refuses to, `compute_completeness.go:356`, so the evidence survives).

### Step 4 — restore normal operation

```sh
systemctl start stellarindex-completeness.timer
```

The hourly incremental run will now carry the clean verdict forward
(rule 3) instead of latching. Confirm on the next tick with Q1, and on
`/v1/coverage`.

---

## 6. What this diagnosis did NOT establish

Stated explicitly so nothing here is over-read:

- **Which sub-predicate is live right now.** §1 proves it is one of
  P1–P5 and §1.1 proves P3 is engaged under the hourly timer, but which
  one *originally* tripped is recorded only in a `detail` string this
  document cannot read. Q1 + step 2 settle it.
- **Whether a truncate-boundary hole exists.** It is the
  highest-prior candidate on the timeline evidence, but it is a
  hypothesis until Q4 returns.
- **Whether failed-tx events appear in `OperationEvents` at all.**
  The code asymmetry between `extract.go` (no success gate) and
  `dispatcher.go:598` / `census.go:94` (gated) is real and verified in
  source. Whether it has any *effect* depends on whether protocol-23
  meta emits operation events for failed transactions, which the repo
  does not settle. Q5 measures it directly; a zero result closes it.
- **Anything about served supply VALUES.** Completeness is a row-count
  claim. The supply numbers these tables feed are governed separately
  (the rollup, the genesis baseline, the cross-check).

---

## 7. Out-of-fence items for coordination

Both potential code fixes identified above live in packages this
document's author was fenced out of. Neither was changed.

| Item | File | Why it is out of fence |
| ---- | ---- | ---------------------- |
| `reconTarget.whereFilter == ""` on the two sep41 targets (Q3) | `internal/ops/chops/reconciliation_catalogue.go:370, 375` | `internal/ops/**` |
| No `in_successful_call` / tx-success gate on the reconcile's expected side (Q5) | `internal/storage/clickhouse/extract.go:117`, `internal/storage/clickhouse/completeness.go:44` | not in the granted fence |
| The remedy commands themselves are ops-side | `internal/ops/chops/ch_rebuild.go` | `internal/ops/**` |

---

## References

- `internal/ops/chops/compute_completeness.go` — the verdict computor
- `internal/ops/chops/reconciliation_catalogue.go` — the sep41 reconcile spec
- `scripts/ops/completeness-incremental.sh` — the hourly timer
- `migrations/0114_soroban_events_topics_overflow.up.sql` — candidate (a)'s forward fix + the "no tool exists" note
- `migrations/0088_sep41_supply_rollup_genesis_baseline.up.sql` — candidate (b)'s schema
- `migrations/0116_completeness_target_floors.up.sql` — P5's durable floor
- [`sep41-mint-recovery.md`](sep41-mint-recovery.md) — the scoped, additive recovery procedure
- [`sep41-watched-set-decision.md`](sep41-watched-set-decision.md) — RETIRED by this document
- [`runbooks/completeness-incomplete.md`](runbooks/completeness-incomplete.md) — the generic incomplete-source runbook
