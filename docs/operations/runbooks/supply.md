---
title: Runbook — supply
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — supply alerts

Supply alerts: cross-check, divergence, refresh, snapshot and verify-rollup. Merged from seven former per-alert pages; each `##` section below keeps one page's content under its alert name.

## At a glance

- [`stellarindex_supply_cross_check_divergence`](#stellarindex_supply_cross_check_divergence)
- [`stellarindex_supply_cross_check_unevaluable`](#stellarindex_supply_cross_check_unevaluable)
- [`stellarindex_supply_divergence_high`](#stellarindex_supply_divergence_high)
- [`stellarindex_aggregator_supply_refresh_stalled`](#stellarindex_aggregator_supply_refresh_stalled)
- [`stellarindex_aggregator_supply_refresh_never_initialized`](#stellarindex_aggregator_supply_refresh_never_initialized)
- [`stellarindex_aggregator_supply_refresh_error_dominant`](#stellarindex_aggregator_supply_refresh_error_dominant)
- [`stellarindex_aggregator_supply_refresh_dormant_fleet`](#stellarindex_aggregator_supply_refresh_dormant_fleet)
- [`stellarindex_sep41_supply_rollup_no_cursor`](#stellarindex_sep41_supply_rollup_no_cursor)
- [`stellarindex_ch_supply_gapfill_failed`](#stellarindex_ch_supply_gapfill_failed)
- [`stellarindex_supply_snapshot_unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert)
- [`stellarindex_supply_snapshot_stale`](#stellarindex_supply_snapshot_stale)
- [`stellarindex_supply_snapshot_critical_stale`](#stellarindex_supply_snapshot_critical_stale)
- [`stellarindex_supply_snapshot_never_initialized`](#stellarindex_supply_snapshot_never_initialized)
- [`stellarindex_supply_snapshot_circulating_zero`](#stellarindex_supply_snapshot_circulating_zero)
- [`stellarindex_supply_verify_rollup_stale`](#stellarindex_supply_verify_rollup_stale)
- [`stellarindex_supply_verify_rollup_unit_failed_alert`](#stellarindex_supply_verify_rollup_unit_failed_alert)

## stellarindex_supply_cross_check_divergence

_Source page `supply.md#stellarindex_supply_cross_check_divergence`: status living, severity P3, last verified 2026-09-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_cross_check_divergence` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml` |
| Typical MTTR | 1 – 4 hours (RCA-driven; not user-impacting on its own) |
| Impact | The asset's `total_supply` / `circulating_supply` / `market_cap_usd` / `fdv_usd` on `/v1/assets/{id}` will be wrong by the divergence amount until reconciled. Customer-visible only on the affected asset's detail page; aggregate price endpoints are unaffected. |

### 2026-07-08 category-error fix (BACKLOG #59) — read this first

Until 2026-07-08, this alert compared Algorithm 2's classic **total**
supply against Algorithm 3's SAC-wrapped **total** supply for
**equality** (1-stroop tolerance), for every configured
`sac_wrappers` pair. That equality is only a true invariant when a
classic asset's ENTIRE economic supply is represented through its
SAC (a genuinely SAC-issued token). It does NOT hold for the common
case — a classic asset that merely HAS a SAC wrapper but is mostly
held classically (trustlines / claimables / LP): Algorithm 2 sums
the total classic supply, Algorithm 3 sums only the SEP-41-minted
(wrapped) amount, and for a partially-wrapped asset those
legitimately diverge by ~the whole supply. Example: AQUA — Algorithm
2 ≈ 86.4B, Algorithm 3 ≈ 0. This produced **8 standing false
positives**. Served supply was never wrong; only the comparison was.

The fix (see `internal/supply/crosscheck.go`'s `WrapClass` type) is a
subset-compare with a completeness fallback, decided 2026-07-08:

- The real subset compare — Algorithm 2's `SACWrapped` component
  (`ClassicSupplyComponents.SACWrapped`, summed from
  `sac_balance_observations`) vs Algorithm 3's `total_supply`, which
  per ADR-0011/0022's own math measures the same wrapped amount via
  independent data paths (a ledger-entry snapshot sum vs an
  event-flow sum) — was **not available at this compare site** in
  2026-07-08, because `ClassicComputer.Compute` folded `SACWrapped`
  into the classic `TotalSupply` before returning a `Supply` and only
  that folded total was persisted to `asset_supply_history` (the table
  `CrossCheckRefresher` reads). **BUILT 2026-07-25** (audit E4/N-F3(b)):
  migration 0117 adds `asset_supply_history.sac_wrapped_stroops`,
  `supply.Supply` gains `SACWrappedStroops`, and `Compute` carries the
  component out alongside the fold. See
  [§What is now two-sided, and what still is not](#what-is-now-two-sided-and-what-still-is-not).
- What IS available, with zero new plumbing, is a mathematically
  true **subset bound**: `SACWrapped` is one of Algorithm 2's own
  non-negative addends (`total = Trustline + Claimable + LPReserve +
  SACWrapped`), so the SAC's `total_supply` (Algorithm 3) can
  **never legitimately exceed** the classic asset's `total_supply`
  (Algorithm 2). `sac_total > classic_total` is impossible under
  correct accounting and IS a genuine "escrow != minted" corruption
  signal; `sac_total ≤ classic_total` is the expected, unremarkable
  state for a partially-wrapped asset and must not page.

This is now the DEFAULT (`wrap_class="partial_wrap"`) for every
cross-check pair. An operator can attest a specific pair is 100%
SAC-represented via `[supply].fully_wrapped_sacs` (a SAC contract
C-strkey, must also be a `sac_wrappers` key) — that pair gets
`wrap_class="full_wrap"` and keeps the ORIGINAL equality compare, so
a genuinely fully-wrapped asset's drift still pages. **No pair is
flagged `full_wrap` as of 2026-07-08** — real Stellar classic assets
are essentially never 100% wrapped.

### If this is firing on BLND / EURC / KALE / PHO — read this second

> **Since 2026-08-05 this case can no longer fire the alert.** It shows
> as `sac_total > classic_total` — leg 1, which is diagnostic-only (see
> §What is now two-sided). A `partial_wrap` alert on these assets now
> means a leg-2 breach and follows that path. The dormant-balance
> understatement below is still real and still served; it surfaces only
> as a positive `over_mint_stroops` on the aggregator's log line and a
> missing `full_history` row in `sac_balance_seed_provenance`.

**This is the one standing cause of a `partial_wrap` alert, it is a
REAL served-supply error, and the fix is an operator command, not a
code change.** If you are here because the alert has been open for days
or weeks on one of these four assets, go straight to
[§Mitigation → dormant pool-held SAC balances](#mitigation--60-min).

Mechanism (verified end-to-end, incident 2026-07-06 "PHO/BLND
VERDICT"; full write-up in
[`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
§"Dormant contract-held SAC balances"):

- Algorithm 3 (the SAC's Σmint−burn−clawback) was independently
  verified correct to the stroop against the raw ClickHouse lake and
  stellar.expert. **It is not the wrong side.**
- Algorithm 2 under-counts. These assets' largest holders are Phoenix /
  Blend **pool contracts** that received the SAC-wrapped token via an
  ordinary SEP-41 `transfer` years ago and have been dormant since. The
  default `supply seed-sac-balances` reads
  `stellar.ledger_entries_current`, a ClickHouse MV that only ever
  processed rows inserted after it was created (~ledger 62,000,000), so
  a Balance entry last written below that floor is invisible to it.
- So `SACWrapped` — one of Algorithm 2's four addends — is short by the
  pool's holding, the classic total is short by the same amount, and
  `sac_total > classic_total` becomes arithmetically possible even
  though the subset bound is a true invariant. The alert is correct;
  the data is wrong.
- **Customer impact is real while this is open:** `total_supply`,
  `circulating_supply`, `market_cap_usd` and `fdv_usd` on
  `/v1/assets/{id}` are understated by the dormant balance for these
  four assets.

> **Superseded hypothesis — do not chase it.** Until 2026-07-06 this
> runbook (and `internal/supply/crosscheck.go`) said the missing
> balances lived in Phoenix/Blend's *own internal accounting*:
> non-standard `contract_data` keys needing a new protocol-specific
> decoder. That was wrong. Rollup-vs-lake reconciliation traced the
> shortfall to ordinary `Vec(Symbol("Balance"), Address(pool))` entries
> on the **SAC's own** storage — the exact shape
> `sac_balance_observations` already models. No new reader is needed
> and none should be written.

### What is now two-sided, and what still is not

Until 2026-07-25 the `partial_wrap` check was ONE-SIDED by
construction: it fired only on `sac_total > classic_total` and was
structurally blind to a mint / escrow SHORTFALL, because a shortfall is
indistinguishable from a partially-wrapped asset's normal state when
all you can compare is the two folded totals.

Audit **E4/N-F3(b)** (2026-07-25) closed that direction.
`CrossCheckSubsetBound` computes **two** legs, but since 2026-08-05
(ADR-0011 amendment) only leg 2 feeds the
`stellarindex_supply_cross_check_divergence_stroops` gauge:
`divergence = escrow_excess_stroops`, or 0 when the leg is unchecked.
Leg 1 is still computed for triage and **cannot page**:

| Leg | Bound | Breach means | Result field | Pages? |
| --- | ----- | ------------ | ------------ | ------ |
| 1 — over-mint (diagnostic) | `sac_total` vs `classic_total` | the SAC's cumulative net mint exceeds classic outstanding — legitimate after a classic-side retirement (BLND) or a one-time SAC mint distributed classically (PHO); otherwise a hint that the classic side is under-observed | `OverMintStroops` | no |
| 2 — escrow-exceeds-minted (2026-07-25) | `SACWrapped ≤ sac_total` | more is escrowed inside the SAC than it ever net-minted; **mints the indexer never captured, or burns it double-counted** | `EscrowExcessStroops` | yes |

So a firing `partial_wrap` alert is always a leg-2 breach: the SEP-41
event capture for that SAC is incomplete — see
[`sep41-mint-recovery.md`](../sep41-mint-recovery.md). The
`over_mint_stroops` field on the aggregator's
`cross-check: divergence over tolerance` WARN line is context only.

**Leg 2 is CS-087-gated and can report UNCHECKED.** A classic snapshot
written before migration 0117 carries no `sac_wrapped_stroops`, so the
leg is not evaluated and `subset_bound_checked=false`. It is
deliberately NOT defaulted to zero: `0 ≤ sac_total` holds vacuously, so
a zero default would publish a green check that verified nothing. The
flag self-clears within one aggregator refresh cycle of the 0117
deploy; the aggregator logs a Debug line naming the pair until it does.
**A green check with `subset_bound_checked=false` checked nothing:
leg 2 is the only leg that feeds the gauge, so its divergence is 0 by
construction.**

**What is STILL not covered.** Two things, and neither is closed by
E4/N-F3(b):

1. **The non-SAC half of Algorithm 2.** Trustline / claimable /
   LP-reserve balances have no independent second observation to
   reconcile against. An undercount there is still invisible — it
   merely widens the benign `classic > sac` gap that leg 1 is required
   to ignore.
2. **An UNDER-counted `SACWrapped`.** Leg 2 is an upper bound on
   escrow, not a proof that escrow was fully observed. The documented
   BLND/EURC/KALE/PHO dormant-pool-balance case sits *below*
   `sac_total` and passes leg 2 quietly. Since leg 1 no longer pages,
   nothing alerts on it: it shows only as a positive
   `over_mint_stroops` on the WARN/Debug line. It is cured by
   `supply seed-sac-balances -full-history` (§Mitigation). Per-contract
   seed provenance in `sac_balance_seed_provenance` (migration 0102) is
   how you tell "expected, never full-history seeded" from "actually
   anomalous".

### Symptoms

- `stellarindex_supply_cross_check_divergence_stroops{classic_key="...",wrap_class="..."} > 1` for ≥ 5 min.
- The labelled `classic_key` identifies the affected asset (format `CODE:ISSUER`); `wrap_class` (`partial_wrap` | `full_wrap`) identifies which invariant fired — see above.
- `stellarindex_supply_cross_check_total{outcome="over",wrap_class="..."}` rate non-zero.

### Background — what each algorithm measures, and what "agree" means now

A SAC-wrapped classic asset is observable two ways:

1. **Algorithm 2 (classic):** total = Σ trustline + Σ claimable + Σ LP-reserve + Σ SAC-wrapped contract balance, all reconstructed from `TrustLineEntry` / `ClaimableBalanceEntry` / `LiquidityPoolEntry` / `ContractData` ledger meta.
2. **Algorithm 3 (SEP-41):** total = Σ mint − Σ burn − Σ clawback over the contract's lifetime, summed off the SAC contract's events.

Both observe the same underlying state, but they are NOT the same
quantity for a partially-wrapped asset: Algorithm 2's total includes
supply that never touched the SAC at all. Per the 2026-07-08 fix
above, "agree" now means:

- `wrap_class="partial_wrap"` (default): Algorithm 3's total must not
  exceed Algorithm 2's total (a subset bound, not equality).
- `wrap_class="full_wrap"` (operator-attested): the two totals must
  be equal within 1 stroop — honest indexer math may differ by 1
  stroop due to NUMERIC truncation at write time; anything larger
  means one indexer dropped events / mis-summed.

### Quick diagnosis (≤ 15 min)

```sh
# 1) Confirm the divergence is real and which asset.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_cross_check_divergence_stroops' \
  | awk '$NF != "0" && $NF != "1"'

# 2) Look at both readings + their basis.
psql -d stellarindex -c \
  "SELECT asset_key, time, total_supply::text, basis, ledger_sequence
     FROM asset_supply_history
    WHERE asset_key IN ('USDC:GA5Z...', 'CCW6...')
    ORDER BY time DESC LIMIT 4;"
```

The SAC contract id for a classic asset is deterministic — derive it once and confirm it matches the row in `asset_supply_history` you'd expect.

> **Periodic gauge emission is wired into the aggregator's supply-refresh loop** when `[supply].aggregator_refresh_enabled = true`. Every `aggregator_refresh_cadence` tick, `supply.CrossCheckRefresher` (built in `cmd/stellarindex-aggregator/main.go::buildCrossCheckRefresher`) loads the latest classic + SAC snapshots for every classic asset that's both in `watched_classic_assets` AND has its SAC contract id declared in `sac_wrappers` AND that contract id is also in `watched_sep41_contracts`. The intersection is the cross-check pair set; a `sac_wrappers` value may use the `CODE-ISSUER` or `CODE:ISSUER` form. Each wrapper outside it is logged at WARN on startup (`sac_wrappers entry NOT cross-checked`). A wrapper whose contract id is not the classic asset's derived SAC fails aggregator startup. A pair the refresher cannot evaluate has its gauge series deleted, and [`supply-cross-check-unevaluable`](supply.md#stellarindex_supply_cross_check_unevaluable) alerts on it. The CLI `stellarindex-ops supply audit <asset> -config /etc/stellarindex.toml -cross-check <counterpart>` path remains available for ad-hoc operator inspection but is no longer the gauge-emission path.

Decision tree (2026-07-08: read `wrap_class` off the firing series first — it changes which row applies):

| wrap_class | Direction | Likely cause | Mitigation |
| ---------- | --------- | ------------ | ---------- |
| `partial_wrap` (any pair) | SACWrapped > SAC (leg 2, the ONLY leg that can fire for this class) | Algorithm 3 missed mint events for the SAC, or double-counted a burn | Follow [`sep41-mint-recovery.md`](../sep41-mint-recovery.md) for that contract |
| `partial_wrap` | SAC > Classic only (`escrow_excess_stroops` 0) | **Not possible — leg 1 is diagnostic-only and does not feed the gauge.** If you see this, the metric computation itself regressed; check `internal/supply/crosscheck.go`'s `CrossCheckSubsetBound` first, not the data |
| `full_wrap` (operator-attested; none configured as of 2026-07-08) | Classic > SAC | Algorithm 3 missed mint events (rare — events are durable) | Replay the SAC contract's event range from Galexie; rerun Algorithm 3 |
| `full_wrap` | Classic < SAC | Algorithm 2 missed a trustline / claimable / LP entry change | Replay the affected ledger range through the trustline-delta indexer; rerun Algorithm 2 |
| either | Both readings stale | Aggregator orchestrator stalled; cross-check is comparing old data | Check `stellarindex_aggregator_silent` runbook first |

### Mitigation (≤ 60 min)

#### Dormant pool-held SAC balances (BLND / EURC / KALE / PHO)

Do this first for those four; it is the standing cause and everything
below is the generic path.

- [ ] **Check whether the fix has ever been applied to this pair.** A
      pair with no `full_history` row has not been fixed yet, and the
      alert is expected until it is:
      ```sql
      SELECT contract_id, asset_key, source, holders_seeded,
             holders_retracted, lake_verified_through,
             min_ledger_seen, max_ledger_seen, seeded_at
        FROM sac_balance_seed_provenance
       WHERE asset_key LIKE 'BLND:%' OR asset_key LIKE 'EURC:%'
          OR asset_key LIKE 'KALE:%' OR asset_key LIKE 'PHO:%'
       ORDER BY asset_key;
      ```
      **A `full_history` row with `lake_verified_through` NULL has not
      been fixed**, whatever its `min_ledger_seen` says: it was written
      before migration 0183, by a binary that neither verified the lake
      nor, before 2026-07-28, filtered TTL-archived balances (PHO's
      +157 %). Re-seed it. Only a row with `lake_verified_through` set is
      evidence: the pass proved `stellar.ledgers` contiguous and
      hash-linked through that ledger before emitting anything, resolved
      the TTL of every watched key, and wrote `holders_retracted`
      tombstones for the removed and archived holders. On such a row,
      `min_ledger_seen` well below 62,000,000 is the evidence the floor
      was reached; a high `min_ledger_seen` means the scan found nothing
      old, which is a different (real) finding. The binary does not stamp
      a wrapper's row when it could not check the TTL of every watched
      key, or when the wrapper matched no Balance entry; the pass exits
      non-zero instead.
- [ ] **Re-seed from the complete append-log.** Dry-run first, per the
      convention; the full-history scan reads
      `stellar.ledger_entry_changes` (complete to genesis) instead of
      the floor-limited current-state projection, so it is heavy and
      **must** run under the heavy-job wrapper:
      ```sh
      run-heavy-job.sh supply-seed-sac-balances \
        stellarindex-ops supply seed-sac-balances \
        -config /etc/stellarindex.toml -full-history -dry-run
      run-heavy-job.sh supply-seed-sac-balances \
        stellarindex-ops supply seed-sac-balances \
        -config /etc/stellarindex.toml -full-history -write
      ```
      To re-seed only the wrappers you are fixing, add
      `-contracts <id>,<id>` (ids from `[supply.sac_wrappers]`); only
      their provenance rows change, and the pass prints a `PARTIAL`
      banner. The walk is silent for about an hour but reports ledgers
      reduced to the `ops_job="supply-seed-sac-balances"` heartbeat.
      The printed `sum=<stroops>` per contract should rise by the
      dormant pool holding; that delta is what the gauge was showing.
      An error naming `no stellar.ttl_live_until row` means this host's
      TTL projection is not backfilled: run Step 2 of
      `deploy/clickhouse/ttl_live_until.sql`, then re-run. Do not work
      around it, because keeping those keys would re-seed archived
      balances as live. An error naming `lake is not intact over the
      range it would reduce` names the first missing or mis-linked
      ledger: nothing was written. Heal it (`ch-live-catchup` near the
      tip, `ch-backfill` below it) and re-run; a hole lets the seed elect
      a superseded balance as current. `matched no Balance entry` names a
      `[supply.sac_wrappers]` contract id that found nothing; check it.
      A holder whose entry was removed or TTL-archived is written as a
      zero-balance tombstone at the removal or archival ledger and is
      counted as `retracted=`, not as a holder. The tombstone supersedes
      any row an earlier pass seeded for that holder, so a re-seed clears
      phantom rows without a DELETE. If an earlier pass of the same
      source seeded more holders than this pass re-seeded and retracted
      together, the pass leaves that contract's provenance row unchanged
      and exits non-zero (`neither re-seeded nor retracted`). The old rows
      for those holders are still served; escalate and do not re-run.
- [ ] **Verify convergence** after the aggregator's next refresher tick
      (`aggregator_refresh_cadence`, default 5m) — the gauge should
      drop to 0 for the re-seeded pair. If it does NOT drop and
      provenance shows a genuine `full_history` seed (non-NULL
      `lake_verified_through`) reaching below the floor, then this pair
      really is anomalous: escalate to the
      generic path below and treat it as fresh corruption.
- [ ] **Record the outcome** in
      [`audit-remediation-operator-actions.md`](../audit-remediation-operator-actions.md)
      — per-asset, since the four are independent.

#### Generic path

- [ ] **Identify which side is wrong** with the audit subcommand
      (2ae729c61). Pass the classic asset; supply the SAC counterpart
      via `-cross-check`; optionally include `-history-hours 24`
      to spot whether divergence is fresh or chronic:
      ```sh
      stellarindex-ops supply audit USDC-GA5Z... \
          -config /etc/stellarindex.toml \
          -cross-check CCW6... \
          -history-hours 24
      ```
      Output prints both snapshots + the cross-check delta. Exit
      code is non-zero on out-of-tolerance; chain
      `|| operator-escalate` if scripting. It is also non-zero with
      `status: UNCHECKED` when a partial-wrap pair's classic snapshot
      carries no `sac_wrapped_stroops`: the escrow leg did not run, so
      divergence 0 verifies nothing. That is not a divergence; re-run
      once a newer classic snapshot has been recorded. And it is
      non-zero with `status: MISALIGNED` when the two snapshots were
      computed more than 1000 ledgers apart (`CrossCheckLedgerTolerance`)
      — the same refusal the aggregator reports as `misaligned` on
      [`supply-cross-check-unevaluable`](supply.md#stellarindex_supply_cross_check_unevaluable).
      There is no verdict to read; the `primary_ledger` /
      `counterpart_ledger` lines name the stalled side, whose supply
      refresh must catch up before the comparison means anything.

- [ ] **Replay the affected range** on the side the audit shows wrong.
      Each algorithm's writer has its own catch-up; restarting the
      indexer re-reads nothing (it has no ledger-window flag).
      - **Algorithm 3** (SEP-41 event sum, `sep41_supply_events`) is a
        projected source. Rewind it to the first diverging ledger; the
        running indexer re-projects from there, and the same command
        resets the `sep41_supply_rollup` fold so the aggregator re-sums
        the corrected rows. Decompress the window first
        ([projector-replay](projector.md#stellarindex_projector_replay_stalled) pre-flight):
        ```sh
        stellarindex-ops projector-replay -config /etc/stellarindex.toml \
            -source sep41_supply -from <first-diverging-ledger> -write
        ```
        Once the cursor is back at tip, confirm the fold with
        `stellarindex-ops supply verify-rollup -config /etc/stellarindex.toml -contracts <C…>`
        (under `run-heavy-job.sh` on r1).
      - **Algorithm 2** (classic components). The SAC-wrapped leg
        re-seeds with `stellarindex-ops supply seed-sac-balances` and the
        claimable leg with `stellarindex-ops supply seed-claimable-balances`
        (both `-write`, under `run-heavy-job.sh`; the dormant-holder path
        above shows the flags). The trustline and liquidity-pool legs
        have no lake re-seed: only the live indexer's observers write
        them, so a fault there is a code defect to escalate, not a replay.

- [ ] **Verify** the divergence gauge drops below 2 within 10 min of
      the replay completing. The gauge updates once per aggregator
      tick; allow ≤ 60 s post-replay before considering the alert
      stale.

- [ ] **Pause** consumer reliance on the affected asset's
      `/v1/assets/{id}` F2 fields if the divergence is large enough
      to materially mislead (>0.1% of total). This repo snapshot
      does not ship a `supply_basis=no_metadata` override or an
      in-tree supply writer; the safe path is to treat the current
      F2 output as advisory until the snapshot writer/reconciliation
      path lands or to suppress the field downstream at the caller.

### Root cause analysis

Capture for the postmortem:

- The first ledger at which the two readings diverged. (Walk
  `asset_supply_history` backward from the alert-firing time.)
- The replay-range commands you ran + the divergence-after value.
- Which indexer was at fault (Algorithm 2's trustline-delta vs.
  Algorithm 3's event-sum).
- If the corruption was caused by a recent code change: the PR diff
  + the audit log.

### Known false-positive patterns

- **Partially-wrapped classic asset (FIXED 2026-07-08, was 8 standing
  false positives)**: a classic asset whose supply mostly lives
  outside its SAC wrapper (e.g. AQUA: Algorithm 2 ≈ 86.4B, Algorithm
  3 ≈ 0). This is now structurally impossible to alert on for
  `wrap_class="partial_wrap"` pairs — `CrossCheckSubsetBound` reports
  zero divergence whenever `sac_total ≤ classic_total`, which is
  every partially-wrapped asset's normal state. If you see this
  pattern again, the fix regressed (check `internal/supply/crosscheck.go`).
- **First 5 minutes after a new asset's SAC is deployed**: the two
  indexers ingest the deployment slightly out of sync. Normally
  resolves within a single aggregator tick. The `for: 5m` clause on
  the alert covers this.
- **Backfill catch-up**: if Algorithm 2 is replaying a historical
  range while Algorithm 3 has already advanced past that range,
  divergence reads as the catch-up gap. Suppress the alert during
  active backfills (operator action) — the gauge label is
  per-asset, so you can `ALERTMANAGER silence` just the affected
  `classic_key`.
- **Clock skew between processes**: each snapshot is the LATEST
  independently-computed reading for its own `asset_key`, written by
  its own per-asset refresher — nothing pins the two reads to the same
  ledger (`internal/supply/crosscheck_refresher.go`: "they can
  describe wildly different ledgers"). A pair whose snapshots are more
  than `CrossCheckLedgerTolerance` (1000 ledgers, ~1.4h) apart is
  refused as `status: misaligned` rather than compared — no divergence
  is published for it; it surfaces on
  [`supply-cross-check-unevaluable`](supply.md#stellarindex_supply_cross_check_unevaluable)
  instead. Inside the tolerance, a mint that lands between the two
  reads can still show up as a small, non-chronic `escrow_excess`
  reading; it clears on the next tick once both sides have advanced
  past it.

### Changelog

- 2026-09-23 — Pairing and staleness. `sac_wrappers` values are now
  normalised before the watched-set lookup (a dash-form value used to
  drop the pair silently), a wrapper that is not the asset's derived SAC
  fails startup, and non-evaluable outcomes delete the gauge series
  instead of freezing it. Added the sibling
  `supply-cross-check-unevaluable` alert.
- 2026-07-25 — **Corrected the standing-cause section (E4/N-F3).** The
  "Known limitation" paragraph still carried the pre-2026-07-06
  hypothesis — that BLND/PHO's missing balances lived in pool-internal
  `contract_data` keys needing a new protocol-specific decoder — which
  the final verdict superseded, and it offered no mitigation at all.
  An operator triaging the P3 that has been open on BLND/EURC/KALE/PHO
  was therefore sent toward unbuilt, upgrade-brittle work instead of
  the one command that fixes it (`supply seed-sac-balances
  -full-history`, shipped 2026-07-10 with the
  `sac_balance_seed_provenance` audit trail). Added the real mechanism,
  the per-asset triage query, the mitigation, and an explicit "do not
  chase the superseded hypothesis" note. No code change: the alert is
  correct and the served supply for those four assets really is
  understated until the re-seed runs.
- 2026-07-08 — **Category-error fix (BACKLOG #59).** Replaced the
  blanket total-vs-total equality compare with a `WrapClass`-aware
  comparison: `partial_wrap` (default) checks the subset bound
  `sac_total ≤ classic_total`, closing 8 standing false positives on
  partially-wrapped classic assets (AQUA-shaped); `full_wrap`
  (operator-attested via `[supply].fully_wrapped_sacs`, none
  configured yet) keeps the original equality compare. Both cross-
  check metrics gained a `wrap_class` label. The real subset compare
  (Algorithm 2's `SACWrapped` component vs Algorithm 3's total) is
  documented as a follow-up — it needs new plumbing (a persisted
  `SACWrapped` snapshot field) not yet built.
- 2026-04-28 — initial draft alongside the cross-check landing PR
  (L2.12 PR 5).
- 2026-04-30 — Related section now cross-links the supply-pipeline
  architecture overview and the four sibling supply alerts
  (refresh-stalled / refresh-error-dominant / snapshot-stale /
  aggregator-silent) so an operator triaging a divergence has the
  surrounding-system map in one click.
- 2026-05-02 — Cross-check gauge emission shipped: the
  aggregator's supply-refresh loop now runs
  `supply.CrossCheckRefresher` per cadence and emits both
  `stellarindex_supply_cross_check_divergence_stroops` and
  `stellarindex_supply_cross_check_total{outcome=…}`. Status flipped
  from `draft` to `living`; removed the manual-cron caveat.

## stellarindex_supply_cross_check_unevaluable

_Source page `supply.md#stellarindex_supply_cross_check_unevaluable`: status draft, severity P3, last verified 2026-09-23._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_cross_check_unevaluable` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml`; promtool-tested in `deploy/monitoring/rule-tests/supply_test.yml` |
| Typical MTTR | 30 min – 2 hours |
| Impact | None served directly. The classic ↔ SAC conservation check for at least one pair is not running, so [`stellarindex_supply_cross_check_divergence`](supply.md#stellarindex_supply_cross_check_divergence) cannot fire for it. A real supply mis-sum on that asset would go unnoticed while this alert is open. |

### Why this exists

The aggregator's cross-check refresher reads the latest classic and SAC
snapshot for each configured pair every `aggregator_refresh_cadence`.
When it cannot evaluate a pair, it increments
`stellarindex_supply_cross_check_total{outcome}` and **deletes** that
pair's `stellarindex_supply_cross_check_divergence_stroops` series.
Before that, the gauge kept its last value, and Prometheus re-exported
it on every scrape. A pair whose classic refresher had stalled therefore
kept reading as a healthy `0` indefinitely, with no rule on the counter.
Now an absent series is expected for an unevaluable pair, and this alert
reports the gap.

### Symptoms

- `increase(stellarindex_supply_cross_check_total{outcome=~"missing_snapshot|read_error|misaligned"}[30m]) > 0`
  for more than an hour, per `outcome` × `wrap_class`.
- One or more `classic_key` series are missing from
  `stellarindex_supply_cross_check_divergence_stroops`.

| `outcome` | Means |
| --------- | ----- |
| `missing_snapshot` | One side of the pair has no `asset_supply_history` row. |
| `read_error` | Reading a snapshot or running the comparison keeps failing. |
| `misaligned` | Both snapshots exist but are more than 1000 ledgers (`supply.CrossCheckLedgerTolerance`) apart, so one side's refresher has stalled. |

### Quick diagnosis (≤ 10 min)

The counter has no `classic_key` label, so the pair comes from the
aggregator log. Every unevaluable outcome logs its pair: `read_error`
and `misaligned` log at WARN, `missing_snapshot` at DEBUG.

```sh
# 1) Which pair, and why. For missing_snapshot, compare the pairs
#    registered at startup with the series the gauge still exports.
journalctl -u stellarindex-aggregator --since='2 hours ago' --no-pager \
  | grep -E 'cross-check: (classic read failed|sac read failed|snapshots misaligned|compare failed)|cross-check pairs registered'
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_cross_check_divergence_stroops'

# 2) How old each side's latest snapshot is (substitute the pair's keys).
psql -d stellarindex -c \
  "SELECT asset_key, max(time) AS latest, max(ledger_sequence) AS ledger
     FROM asset_supply_history
    WHERE asset_key IN ('USDC:GA5Z...', 'CCW6...')
    GROUP BY asset_key;"
```

### Mitigation

- **`misaligned` or `missing_snapshot` on one side.** That side's supply
  refresher is not producing snapshots. Follow
  [`supply-refresh-stalled`](supply.md#stellarindex_aggregator_supply_refresh_stalled) or
  [`supply-refresh-error-dominant`](supply.md#stellarindex_aggregator_supply_refresh_error_dominant)
  for the stale asset. The cross-check recovers on the first tick after
  both sides are within 1000 ledgers of each other.
- **`missing_snapshot` on a newly added pair.** Wait one refresh cadence
  for both sides. If it persists, check that the classic asset is in
  `[supply].watched_classic_assets` and the SAC is in
  `watched_sep41_contracts`. The aggregator logs a WARN at startup for
  every `sac_wrappers` entry that is not cross-checked.
- **`read_error`.** Check storage first:
  [`pg-conns-saturated`](postgres.md#stellarindex_timescale_connections_saturated) and
  [`timescale-primary-down`](postgres.md#stellarindex_timescale_primary_down). A `compare
  failed` line instead means a snapshot has a nil `total_supply`.
  Re-seed that asset's snapshot and do not change the tolerance.

When the cause is fixed, the next tick restores the divergence series.
If it then reads over 1, follow
[`supply-cross-check-divergence`](supply.md#stellarindex_supply_cross_check_divergence).

## stellarindex_supply_divergence_high

_Source page `supply.md#stellarindex_supply_divergence_high`: status living, severity P3, last verified 2026-07-07._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_divergence_high` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml` |
| Typical MTTR | 1 – 3 hours (config-update-driven; not user-impacting on its own) |
| Impact | `/v1/assets/native` (or the affected asset) reports a `circulating_supply` — and the `market_cap_usd` derived from it — that differs from the market's authoritative figure by more than 1%. Customer-visible on the affected asset's detail page; aggregate price endpoints are unaffected. |

### What this alert is (and is NOT)

This is the automated version of the manual "is our circulating supply
right?" investigation documented in
[`docs/methodology/xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md).
It compares **OUR served `circulating_supply`** against an **external
authoritative reference**:

- **Stellar Network Dashboard** (`dashboard.stellar.org/api/v3/lumens`)
  — the market-standard XLM circulating figure. Free, no auth,
  authoritative for XLM. This is the primary (and, today, only-on)
  reference.
- **CoinGecko** (`/coins/{id}` → `market_data.circulating_supply`) —
  off by default; the free tier has been 429-throttled since
  2026-06-19. Enable with a Pro key.

It is **distinct** from
[`stellarindex_supply_cross_check_divergence`](supply.md#stellarindex_supply_cross_check_divergence),
which compares two of OUR OWN numbers (a classic asset's Algorithm 2
ledger-entry sum vs its SAC-wrapped Algorithm 3 event sum). This alert
is our number vs the world's.

### Symptoms

- `stellarindex_supply_divergence_ratio{asset="native",reference="stellar-dashboard"} > 0.01`
  for ≥ 1 h.
- `stellarindex_supply_divergence_total{outcome="divergent"}` rate
  non-zero.
- The `asset` + `reference` labels identify which figure and which
  reference disagree.

### Background — why 1%, not 0.03%

Our served XLM circulating (`total − Σ SDF-mandate − upgrade-reserve`)
tracks the Dashboard's (`total − mandate − upgrade-reserve − fee-pool`)
to within **~0.03%** — the entire residual is the Fee Pool (~10.1 M
XLM), which we deliberately do not subtract (a sub-basis-point
correction). See the methodology doc's live reconciliation table. The
1% alert threshold sits **two-plus orders of magnitude above** that
noise floor, so it fires only on a genuine drift, never on the
structural Fee-Pool delta.

### The two real causes (in likelihood order)

1. **A stale SDF-reserve exclusion account list (most likely).** SDF
   distributed lumens from — or added — an account that
   `stellarindex_sdf_reserve_accounts` no longer tracks accurately. Our
   circulating is then too LOW (we're still excluding distributed
   lumens) or too HIGH (SDF added an undistributed account we don't
   exclude). **This is the drift this alert exists to catch.**
2. **The reference changed methodology.** The Dashboard (or CoinGecko)
   redefined what it counts as circulating. Rare, but possible.

### Quick diagnosis (≤ 15 min)

```sh
# 1) Confirm the divergence + which reference.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_divergence_ratio'

# 2) Our served figure + basis.
curl -fs http://localhost:3000/v1/assets/native \
  | jq '{circulating_supply, supply_basis, total_supply}'

# 3) The reference's current figure (Dashboard).
curl -fs https://dashboard.stellar.org/api/v3/lumens \
  | jq '{totalSupply, sdfMandate, upgradeReserve, feePool}'
```

Compute the Dashboard's circulating as
`totalSupply − sdfMandate − upgradeReserve − feePool` and compare to
ours. If ours is materially LOWER, we are over-excluding (a reserve
account was distributed from); if HIGHER, we are under-excluding (a new
reserve account exists we don't track).

Cross-check the component-level agreement against the methodology doc's
reconciliation table — a single account whose balance moved is usually
obvious in the `sdfMandate` delta.

### Mitigation (≤ 60 min) — do NOT blindly change our number

> **The methodology doc is authoritative.** Do NOT edit our
> circulating figure to make the alert stop. Fix the exclusion set (if
> stale) or confirm the reference changed (and record it). Eager
> "make it match" edits hide a real depeg/mint event class.

If the cause is a **stale SDF exclusion set** (the common case):

- [ ] Reconcile `stellarindex_sdf_reserve_accounts` +
      `stellarindex_reserve_balances_stroops` against SDF's current
      published non-circulating set (the Dashboard's `lumens.js` source
      is the reference set). Both live in
      `configs/ansible/.../roles/archival-node/defaults/main.yml`.
- [ ] Update BOTH lists in one PR (secrets via
      `ansible-vault edit` if any balance is sensitive; the account
      list is public network truth). r1 config is ansible-managed —
      the change lands in `configs/ansible/` in the same PR
      (docs/operations/maintainer-workflow.md, "r1 configuration is
      ansible-managed").
- [ ] Apply: `ansible-playbook -i inventory/r1.yml
      playbooks/archival-node.yml --tags supply --check --diff` then
      without `--check`.
- [ ] Trigger a supply refresh so
      `asset_supply_history` gets a fresh row (the
      `supply.Refresher` goroutine on cadence, or the
      `stellarindex-ops supply snapshot` timer). The
      `supply.Refresher` reads the live balances from
      `account_observations`; a config-only account-set change takes
      effect on the next refresh with no lake re-derive.
- [ ] Verify `stellarindex_supply_divergence_ratio` drops below 0.01
      within a couple of worker cycles (default cadence 15 min).

If the cause is a **reference methodology change**:

- [ ] Confirm against the reference's own changelog / docs.
- [ ] Update `docs/methodology/xlm-circulating-supply.md`'s
      reconciliation table with the new basis + the new residual.
- [ ] If the new residual is legitimately > 1%, widen
      `[divergence.supply].threshold_pct` (and the alert's `0.01`
      literal in BOTH rule trees) with a comment citing the change.

### Known non-firing / graceful-degrade behaviour

- **Reference dark (CoinGecko 429, Dashboard outage).** The worker
  records `outcome="no_reference"` and does NOT update the ratio gauge
  — the gauge holds its last (healthy) value and the alert does not
  fire on a dead reference. This is deliberate: a missing reference is
  not a supply drift. Watch
  `stellarindex_supply_divergence_total{outcome="no_reference"}` if you
  want the "checker running blind" signal; it is not paged.
- **Bootstrap (no served snapshot yet).** Before the supply refresher
  has produced its first `asset_supply_history` row, the worker records
  `outcome="refresh_error"` and emits no ratio. Resolves once the first
  snapshot lands.
- **Worker disabled.** `[divergence.supply].enabled = false` (the
  default) means no series exist and the alert cannot fire. Enable on
  r1 via ansible to arm the check.

### Root cause analysis

Capture for the postmortem:

- Which account's balance moved (the `sdfMandate` component delta).
- The reserve-list PR diff + the `ratio`-after value.
- Whether the drift was a real SDF distribution (update the list) or a
  reference methodology change (update the methodology doc).

### Changelog

- 2026-07-07 — initial version, shipped alongside the supply-divergence
  cross-check worker (Stellar Dashboard + CoinGecko references +
  `stellarindex_supply_divergence_ratio` / `_total` / `_duration_seconds`).

## Shared context: `supply.md` alerts

_Source page `supply.md`: status living, severity P2, last verified 2026-10-06._

Six alerts, one rule file (`configs/prometheus/rules.r1/supply-refresh.yml`, group `stellarindex.supply_refresh`, `component: supply`; the multi-host twin is `deploy/monitoring/rules/supply-refresh.yml`). Only `_stalled` is `severity: page` (P2); the rest are `severity: ticket` (P3).

The aggregator-resident supply refresher (`runSupplyRefresh`, `cmd/stellarindex-aggregator`) ticks every `aggregator_refresh_cadence` (default 5 min) per watched asset and increments `stellarindex_aggregator_supply_refresh_total{asset_key, outcome}`. It feeds the F2 fields on `/v1/assets/{id}` (`circulating_supply`, `total_supply`, `max_supply`, `market_cap_usd`, `fdv_usd`) via `asset_supply_history`; on any failure the previous snapshot stays, so consumers see correct-but-old data. This is the goroutine path (`[supply] aggregator_refresh_enabled`); the code default is false but the ansible template renders `true` on r1. The systemd-timer path has its own alerts (`supply-snapshot-*`).

Shared commands (aggregator metrics on r1 are on `localhost:9465`; `ssh root@136.243.90.96`):

```sh
systemctl status stellarindex-aggregator
curl -s http://localhost:9465/metrics | awk '/^stellarindex_aggregator_supply_refresh_total\{/' | sort
sudo journalctl -u stellarindex-aggregator --since "1 hour ago" -n 200 | grep -E "supply refresh|supply-refresh"
grep -A 10 "^\[supply" /etc/stellarindex.toml
```

The metric is keyed by `(asset_key, outcome)`. Equivalent PromQL: `sum by (asset_key, outcome) (rate(stellarindex_aggregator_supply_refresh_total[15m]))`. Every asset stopped = fleet-wide; one `asset_key` failing while others tick = per-asset.

Log gotcha: the `supply refresh ok` line is Debug-level and invisible at the default log level, so its absence proves nothing. The visible signals are the Warn/Error lines (`supply refresh: no ledger` / `compute failed` / `insert failed`, and `supply refresh: <outcome>`).

### At a glance

- [`stellarindex_aggregator_supply_refresh_stalled`](#stellarindex_aggregator_supply_refresh_stalled): no `ok` tick fleet-wide for 30 min (page)
- [`stellarindex_aggregator_supply_refresh_never_initialized`](#stellarindex_aggregator_supply_refresh_never_initialized): `ok` series absent for 36 h
- [`stellarindex_aggregator_supply_refresh_error_dominant`](#stellarindex_aggregator_supply_refresh_error_dominant): one asset > 50% non-ok ticks for 30 min
- [`stellarindex_aggregator_supply_refresh_dormant_fleet`](#stellarindex_aggregator_supply_refresh_dormant_fleet): 2+ assets `dormant` together for 30 min
- [`stellarindex_sep41_supply_rollup_no_cursor`](#stellarindex_sep41_supply_rollup_no_cursor): SEP-41 rollup pinned, projector cursor absent
- [`stellarindex_ch_supply_gapfill_failed`](#stellarindex_ch_supply_gapfill_failed): `ch-supply.service` failed

## stellarindex_aggregator_supply_refresh_stalled

Severity P2 (`page`), `for: 5m`. MTTR 15-30 min. Impact: F2 fields go increasingly stale across all watched assets; customer-visible a few minutes after the alert.

Expr: `sum(changes(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[30m])) == 0`. The fleet-wide `sum()` is deliberate: one healthy asset means the goroutine is alive and per-asset failure belongs to [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant). The older `time() - max(timestamp(...))` form never fired (`timestamp()` returns scrape time, about now) and must not be reintroduced.

Quick diagnosis (5 min):

1. `systemctl status stellarindex-aggregator`: process up?
2. `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_ticks_total`: is the orchestrator alive (counter incrementing)?
3. The shared per-`asset_key` outcome listing above: if only some keys stalled, `error_dominant` should also be firing.
4. Recent supply-refresh Warn/Error logs.

Root causes:

1. **Aggregator process down.** Investigate the crash in journald; restart.
2. **Orchestrator wedged.** Process runs but `stellarindex_aggregator_ticks_total` is also stalled. Restart the binary and file a P2 bug for the wedge; take a pprof goroutine dump first if available.
3. **Every tick failing.** Goroutine alive but every per-asset tick has a non-ok outcome. `error_dominant` should also be firing; go there.
4. **Refresher disabled cannot be what fired this.** With the `changes()` expr a disabled refresher leaves the series ABSENT, which is [never_initialized](#stellarindex_aggregator_supply_refresh_never_initialized). If `_stalled` fired, the refresher was recently alive.

Mitigation: check process health; if up but stalled and `ticks_total` is also stalled, restart; if the orchestrator ticks but supply does not, confirm `aggregator_refresh_enabled = true` and look for repeated outcome labels. Restart is the safe mitigation (take a pprof goroutine dump first if available), then investigate from journald. Verification: `outcome="ok"` increments resume within one cadence (5 min); the alert clears once any `ok` increment lands inside the trailing 30 min window.

False positives: the first minutes after an aggregator restart have no observations; `for: 5m` absorbs about one cadence, longer restarts still trip it. A disabled refresher does not fire here (see cause 4).

Also see [`aggregator.md#stellarindex_aggregator_silent`](aggregator.md#stellarindex_aggregator_silent) when the orchestrator's own tick counter is stalled.

## stellarindex_aggregator_supply_refresh_never_initialized

Severity P3 (`ticket`), `for: 5m`. MTTR 15-60 min. The rule lives in the supply-refresh rule file in both trees (not `aggregator.yml`). Its `runbook_url` points at this section. The timer-path sibling, [supply.md#stellarindex_supply_snapshot_never_initialized](supply.md#stellarindex_supply_snapshot_never_initialized), covers the daily writer; this section is self-sufficient for the aggregator alert.

Impact: the supply-refresh goroutine has never produced a successful tick; F2 fields on `/v1/assets/{id}` are NULL for every asset.

Expr: `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h]) == 1`. A never-incremented counter does not exist as a series: absent, not zero. The 36 h window means a fresh boot has over a day of grace and cannot false-positive (operators eyeballing the metric in the first 5 min may still misread "no data yet" as broken). Distinct from `_stalled`, which needs the series to have existed; together they cover "was working, stopped" and "never worked".

Symptoms:

- `outcome="ok"` series absent from the scrape.
- `/v1/assets/USDC-G...` returns the `AssetDetail` envelope with all `*_supply` and `*_cap_usd` fields null.
- No `supply refresh complete` info lines in the aggregator log.

Quick diagnosis (5 min):

```sh
journalctl -u stellarindex-aggregator -n 200 --no-pager | grep -iE 'supply.*refresh|watched_'
grep -E '\[supply\]|watched_classic_assets|watched_sep41_contracts|sdf_reserve_accounts' /etc/stellarindex.toml
sudo -u postgres psql -d stellarindex -c "SELECT * FROM asset_supply_history ORDER BY time DESC LIMIT 5;"
```

- Empty `[supply].watched_*`: the refresh path is gated on a non-empty asset set, so unset means the goroutine is intentionally silent. Most common cause (F-1266, audit-2026-05-12). The usual chain: a new asset launches, the operator does not add it to the watched list, its F2 fields show null, a support ticket lands.
- Non-empty config but `asset_supply_history` empty: goroutine wired but every asset failing; check the LCM reader / classic-supply observers.
- Also confirm `aggregator_refresh_enabled = true`: when false (the code default) the goroutine never ticks.
- Aggregator recently restarted: wait 5 min; the first refresh waits on the bootstrap window.

Mitigation (15 min):

1. Populate the watched list in `/etc/stellarindex.toml`. Keys are `watched_classic_assets` / `watched_sep41_contracts`; the short forms `watched_classic` / `watched_sep41` are NOT recognised and the TOML is rejected at load:
   ```toml
   [supply]
   watched_classic_assets = [
       "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
       "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2",
       # ... full list per docs/operations/supply.md
   ]
   watched_sep41_contracts = []
   sdf_reserve_accounts = ["GA..."]
   ```
2. `systemctl restart stellarindex-aggregator`.
3. Within 5 min `outcome="ok"` should increment; sample a watched asset's `/v1/assets/{id}`.
4. Verification: `circulating_supply` non-null on at least one watched asset; `market_cap_usd` non-null when it has a USD price.

Long-term fix (tracked separately): auto-populate the watched list from the verified-currency catalogue (`internal/currency/data/seed.yaml`).

## stellarindex_aggregator_supply_refresh_error_dominant

Severity P3 (`ticket`), `for: 30m`. MTTR 15-60 min. Impact: F2 fields stale or wrong for the affected asset; the previous snapshot stays in `asset_supply_history`.

Expr (aggregated per asset, F-1320): `sum by (asset_key) (rate(..._supply_refresh_total{outcome!~"ok|dormant|missing_baseline"}[5m])) / (sum by (asset_key) (rate(..._supply_refresh_total[5m])) > 0) > 0.5`. Per asset because a single asset failing 100% of its ticks could never push a fleet-wide fraction past 50% while healthy siblings diluted it. Counted outcomes: `no_ledger`, `no_observation`, `compute_error`, `write_error`, `stale_component`, `missing_freshness`, `static_reserve` (XLM published from the dated static reserve map because the live account observer could not answer). Excluded:

- `dormant`: an accepted snapshot (see [stale_component](#outcomestale_component) case 2).
- `missing_baseline`: the SEP-41 genesis baseline is unseeded (SAC-wrapper whose pre-Soroban opening balance is not seeded, incident 2026-07-06). Run `stellarindex-ops supply seed-sep41-genesis`; it is range-scoped, not corruption, and a genuine post-seed negative still surfaces as `compute_error`. Visible only in the per-asset series.

Symptoms: `> 50%` of one asset's ticks non-ok for 30 min; repeated `supply refresh: <outcome>` lines with the same label.

Quick diagnosis (5 min):

1. Which outcome dominates: `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total | sort -t' ' -k2 -rn | head`.
2. Per-asset breakdown (shared commands). If one `asset_key` dominates the non-ok rate while others are healthy, the fault is per-asset (config drift on `watched_classic_assets`, missing locked-set member, SAC wrapper map gap) rather than fleet-wide.
3. Count wrapped errors: `sudo journalctl -u stellarindex-aggregator --since "30 min ago" -n 200 | grep "supply refresh: " | sort | uniq -c | sort -rn | head`.
4. Sanity-check the `[supply]` config (shared commands).

`outcome="dormant"` is NOT an error: the component anchor did not move since the last tick, so the last observation was re-stamped as current (F-1320). It is also what a dead observer looks like, so it is not evidence the producer is alive; do not chase it provided `MinComponentLedger` last moved within about 24 h (17,280 ledgers, `DefaultMaxDormantComponentLedgers`, R-002 audit-2026-07-23). Past that horizon the gate fails closed to `stale_component`: publishing stops and this alert fires.

### Root causes by dominant outcome

#### `outcome="no_ledger"`

The aggregator cannot resolve a real chain position to stamp the snapshot at. It takes the `ledgerstream` cursor from `ingestion_cursors` and clamps it to the newest ClickHouse `stellar.ledgers` row in the 512 ledgers below that cursor (about 45 min; an older row is refused anyway). Two conditions reach this outcome:

1. No cursor at all: the indexer has not produced its first `ledgerstream` row, or `ingestion_cursors` is unreachable.
2. No `stellar.ledgers` row in that window: the lake is empty, wholly gapped below the chain position, or its tip stalled further back than the clamp accepts (a wedged CH sink, not the ordinary landing race).

The cursor routinely leads the lake by a few ledgers (Postgres is realtime, the CH sink lands seconds later); that lead is clamped away and does not reach this outcome, so a sustained `no_ledger` rate is one of the two conditions, not ordinary lag.

- Signal: `no_ledger` increments far exceed any other outcome; the wrapped error text in the log names which condition fired and which cursor supplied the bound.
- Mitigation: for (1) confirm the indexer runs and writes cursors; if storage is broken, route to `postgres.md#stellarindex_timescale_connections_saturated`. For (2) compare `max(ledger_seq)` in `stellar.ledgers` with the `ledgerstream` cursor (empty or gapped lake vs stalled lake) and inspect archivist / galexie stack lag.

#### `outcome="no_observation"`

The chain reader (live LCM) returned no observation for at least one watched asset AND the operator-static fallback was empty. Most common after a fresh deploy: the AccountEntry observer has not backfilled deep enough.

- Signal: `no_observation` dominates; per-asset logs identify the uncovered watched accounts.
- Mitigation: (a) wait for the observer backfill (hours to days for the configured watched set), or (b) populate the operator-static blocks (`[supply.reserve_balances_stroops]`, `[metadata.issuer_home_domains]`) as a bridge.

#### `outcome="compute_error"`

The supply algorithm failed: Algorithm 1 (XLM) the reserve reader or XLMComputer threw; Algorithm 2 (classic) one of the four component sums failed; Algorithm 3 (SEP-41) the kind-totals query failed.

- Signal: `compute_error` increments without `no_observation` / `no_ledger`.
- Mitigation: read the wrapped error in the per-asset logs; typically a code bug or config inconsistency (asset not parseable). Roll back the binary if it is a recent deploy.

#### `outcome="write_error"`

`Store.InsertSupply` failed: Postgres unreachable, NUMERIC overflow on a malformed amount, etc. Confirm Postgres is reachable; check recent `asset_supply_history` rows for CHECK-constraint violations.

#### `outcome="stale_component"`

The F-1236 freshness gate rejected the snapshot: a per-component observation lags the snapshot's chain-tip ledger by more than `[supply] stale_component_ledgers` (default `1000` ledgers, about 85 min). The previous snapshot stays served.

The gate compares the always-advancing chain tip against `MinComponentLedger`, sourced from the change-driven classic/SEP-41 observers. Two different causes, not distinguishable by the gap alone; look at whether `MinComponentLedger` is moving:

1. **Stalled producer (real staleness).** The observer filling the component tables (trustlines / claimable_balances / liquidity_pools / sac_balances for classic; sep41_supply for SEP-41) is wedged or far behind; `MinComponentLedger` advances but cannot keep up, or regressed. This is what the gate is meant to catch.
   - Signal: the indexer's per-source freshness for that observer hypertable also lags; the `gap=...` in the WARN log does not stabilise around a fixed `min_component_ledger`.
   - Mitigation: treat as an observer-ingest stall; confirm the indexer is healthy and the observer progressing; route to the ingest-pipeline runbooks. Do NOT relax the gate to mask a stalled producer.
2. **Dormant asset (not staleness, F-1320; bounded to about 24 h, R-002).** A low-activity asset (governance tokens like PHO, niche classic credits) had no balance change, so `MinComponentLedger` is frozen and its last observation IS the current supply. Under the pre-F-1320 gate every future tick was rejected and the row went permanently stale (observed on PHO: gap 1017 -> 1324 and climbing). The refresher now treats an unchanged `MinComponentLedger` as dormant and accepts the snapshot (`outcome="dormant"`, row inserted) while the gap since it last moved stays within `DefaultMaxDormantComponentLedgers` (17,280 ledgers, about 24 h at 5 s close). Expect a single `stale_component` on the first tick after an aggregator restart for a quiet asset (cold start: dormant vs stalled is indistinguishable until a second tick), then it flips to `dormant`.

   Past the 24 h horizon the gate fails closed: a frozen `MinComponentLedger` is what a dead observer also looks like, so the tick is rejected with `stale_component`, publishing STOPS for that asset, and this alert fires. A sustained `stale_component` stream for one asset is therefore case 1 or a dormant asset past the horizon. The `stalled observer, not a dormant asset` log line marks the post-horizon case, not a fresh case-1 rejection.
   - Signal: in the WARN log `min_component_ledger` is constant across ticks while `gap` climbs; `first_observation` is logged on the cold-start tick. Once `gap` exceeds the horizon the line becomes `supply refresh: rejecting snapshot — component ledger frozen past the dormancy horizon (stalled observer, not a dormant asset)` with a `dormancy_horizon` field instead of `first_observation`.
   - Discriminator (quiet asset vs dead observer): check the component observer itself. When did `min_component_ledger` last advance, and is the indexer's per-source freshness for the relevant observer hypertable otherwise healthy? Healthy and nothing to write = dormant; observer ingest stalled = case 1 even though the alert looks identical (route to ingest-pipeline runbooks). Do not widen the horizon to silence the alert without checking.
   - Mitigation: for the pre-horizon cold-start blip (or a binary predating F-1320), raise that asset's threshold (see [per-asset threshold override](#per-asset-threshold-override-stale_component-remedy)). For an asset confirmed legitimately dormant beyond 24 h (and monitored by other means), `[supply] max_dormant_component_ledgers = 0` restores legacy unbounded dormancy; it is global with no per-asset equivalent, so prefer raising that asset's `stale_component_ledgers` when only one asset needs it.

#### `outcome="missing_freshness"`

Strict-freshness mode (`[supply] strict_freshness_required = true`) rejected a snapshot with `MinComponentLedger == 0` (no freshness anchor: the static-XLM fallback, or a transiently failing freshness producer such as a Postgres/Redis blip).

- Signal: increments only when strict mode is enabled.
- Mitigation: confirm every freshness producer is wired and not failing. If the deployment legitimately runs the static fallback for some assets, leave strict mode off (the default) until the storage-backed readers cover the watched set.

#### Per-asset threshold override (`stale_component` remedy)

The global `[supply] stale_component_ledgers` is one number for all assets. Give a known low-activity asset a relaxed per-asset threshold rather than loosening the gate fleet-wide (which would let a stalled high-traffic asset like XLM or USDC through):

```toml
[supply.stale_component_ledgers_by_asset]
# asset_key (CODE:ISSUER for classic, bare contract id for SEP-41)
# = relaxed threshold in ledgers. ~5000 = 7 h.
"PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO" = 5000
```

Identify the asset from the `asset_key` label on `stellarindex_aggregator_supply_refresh_total`. The code option is `supply.WithStaleComponentLedgersFor(assetKey, maxLag)`, wired automatically from the TOML. Keys are canonicalised (`PHO-G...` and `native` resolve like `PHO:G...` and `XLM`). A key that does not parse, names an asset outside `watched_classic_assets` / `watched_sep41_contracts` (XLM is always watched), or duplicates another spelling of the same asset fails config validation at boot. A value of `0` disables the gate for that asset only. After F-1320 the override is no longer required to keep a dormant row fresh (the gate self-recovers via `dormant`); use it to silence the single cold-start blip or on a binary predating the fix.

### Mitigation

1. Identify the dominant outcome (Quick diagnosis 1).
2. Apply the matching fix above.
3. `no_observation` with the observer not yet backfilled is expected during bootstrap: wait or populate the operator-static fallback blocks.
4. Verification: `ok` rate exceeds the error-outcome rate; the alert clears within 30 min as the rolling window catches up.

False positives: bootstrap after a fresh deploy (`no_observation` dominates while the AccountEntry observer, Task #54, backfills; `for: 30m` usually absorbs it, longer bootstraps trip it and the alert may be silenced during deploy windows). A new entry in `watched_classic_assets` produces `no_observation` ticks until the observer has rows for its locked-set members; other assets keep ticking `ok`.

## stellarindex_aggregator_supply_refresh_dormant_fleet

Severity P3 (`ticket`), `for: 30m`. Same diagnostics as [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant); this alert exists because `dormant` is excluded from that one, which would stay silent until the 24 h horizon flips these assets to `stale_component`.

Expr: `count(sum by (asset_key) (rate(..._supply_refresh_total{outcome="dormant"}[15m])) > 0) > 1`, i.e. two or more assets report `dormant` together.

`MinComponentLedger` is not a per-asset age: classic assets share the slowest of the four component observers' `MAX(ledger)`, SEP-41 contracts share `MAX(ledger)` of `sep41_supply_events`. A quiet asset cannot freeze that watermark alone, so shared dormancy means the producer stopped advancing, and every tick re-stamps a frozen observation as current until the 24 h horizon (served supply goes stale silently).

- Signal: the dormant `asset_key`s are all classic (or all SEP-41) and their WARN/DEBUG logs carry the same `min_component_ledger`.
- Mitigation: check each component observer advances: `MAX(ledger)` of `trustline_observations`, `claimable_observations`, `lp_reserve_observations` and `sac_balance_observations` (or `sep41_supply_events`) against the chain tip. The lowest is the stalled producer; route to the ingest-pipeline runbooks as in [stale_component](#outcomestale_component) case 1. Do not raise per-asset thresholds to silence it.

## stellarindex_sep41_supply_rollup_no_cursor

Severity P3 (`ticket`), `for: 30m`, per `contract_id`. MTTR 15 min once the projector commits `sep41_supply` cycles. Impact: served SEP-41 supply stays exact, but every supply read for the contract re-sums its whole `sep41_supply_events` history, the Postgres load behind the 2026-07-06 incident (migration 0085 exists to prevent it).

Why: the aggregator's rollup worker folds only rows the projector has durably committed, reading the bound from `ingestion_cursors` row `(source='projector', sub_source='sep41_supply')`. When absent it fails closed: folds nothing and leaves `sep41_supply_rollup.last_ledger` where it is (often 0). Correctness holds but the checkpoint + delta fast path does not. Those passes are labelled `outcome="no_cursor"` on `stellarindex_sep41_supply_rollup_advances_total` so they are not mistaken for a dormant token's `noop`. Expr: `sum by (contract_id) (increase(stellarindex_sep41_supply_rollup_advances_total{outcome="no_cursor"}[15m])) > 0`.

Symptoms:

- `no_cursor` climbs for the `contract_id` with no `ok` increments.
- One WARN per contract on entering the state: `sep41 supply rollup pinned: projector cursor for sep41_supply is absent`.
- Postgres IO and `stellarindex_aggregator_supply_refresh_duration_seconds` p99 may climb while nothing else looks wrong.

Quick diagnosis (5 min):

```sh
psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'projector' ORDER BY sub_source;"
psql -U stellarindex -d stellarindex -c \
  "SELECT contract_id, last_ledger, updated_at FROM sep41_supply_rollup ORDER BY contract_id;"
sudo journalctl -u stellarindex-indexer --since "30 min ago" | grep -i 'projector' | grep -i 'sep41_supply' | tail -20
```

If the first query shows a `sep41_supply` row the alert should clear on the next rollup pass; if not, check that the aggregator and projector point at the same database.

Mitigation (15 min):

- Get the projector committing `sep41_supply` cycles. It registers `sep41_supply` only when the indexer's config has a non-empty `[supply] watched_sep41_contracts`; an aggregator watching contracts the indexer does not causes this alert.
- If the projector runs but does not advance: [projector-wedged](projector.md#stellarindex_projector_wedged) or [projector-row-quarantined](projector.md#stellarindex_projector_row_quarantined).
- If `ingestion_cursors` was lost in a restore, do NOT insert a cursor row by hand: it claims settlement the projector never evidenced and the fold would permanently exclude any row still missing below it. Re-run the projection from a known ledger with `stellarindex-ops projector-replay -source sep41_supply` ([projector-replay](projector.md#stellarindex_projector_replay_stalled)).
- Verification: `ok` (or `noop`) increments resume on the next rollup pass; the alert resolves within 15 min.

Code: `AdvanceSEP41SupplyRollup` in `internal/storage/timescale/sep41_supply_events.go`; `runSEP41SupplyRollup` in `cmd/stellarindex-aggregator/main.go`. Rollup reset and re-fold: [sep41-mint-recovery](../sep41-mint-recovery.md).

## stellarindex_ch_supply_gapfill_failed

Severity P3 (`ticket`), `for: 10m`. MTTR 15-30 min. Detected via `node_systemd_unit_state` (node_exporter `--collector.systemd`): `node_systemd_unit_state{name="ch-supply.service",state="failed"} == 1`. Impact: the daily defensive forward-gap-fill of `stellar.supply_flows` (backs `/v1/assets` SEP-41 supply) failed. Served supply is likely still correct (live decode-at-ingest keeps it current), but the backstop is down and a real live-writer gap would go unhealed.

Why: `ch-supply.service` failed silently for weeks in 2026-07. The 2026-07-03 non-root hardening set `User=stellarindex` but the script still appended to a root-owned `/var/log/ch-supply-refresh.log` (`Permission denied` every run) and nothing alerted. Logging moved to journald (the script writes nothing to disk); this alert makes future failures loud.

Symptoms: the unit is `failed` for 10 min or more; the daily timer (`ch-supply.timer`, about 08:00) shows a failed last run.

Quick diagnosis (5 min):

```sh
ssh root@r1 'systemctl status ch-supply.service --no-pager'
ssh root@r1 'journalctl -u ch-supply.service -n 60 --no-pager'
```

Classify:

- `seed [X,Y] FAILED` (stderr): a `stellarindex-ops ch-supply` chunk errored; usually ClickHouse pressure (Phase-0 / heavy re-derive) or a transient CH error.
- `ch-supply: tip unresolved`: the Postgres `ingestion_cursors` tip query returned empty/0. Check Postgres and the `ledgerstream` cursor.
- `ch-supply: supply_flows watermark unresolved (got '...')`: the ClickHouse `max(ledger_seq)` probe failed (curl error on stderr above it) or returned non-digits. Check ClickHouse on `:8123` and that `stellar.supply_flows` exists; nothing was seeded.
- Any `Permission denied` / disk write: regression of the original bug; the script must not write to disk (see `run-ch-supply.sh`).

Mitigation (15 min):

- ClickHouse pressure: re-run off-peak with `systemctl start ch-supply.service`; the memory guard (`CHSUPPLY_MEMGUARD`) throttles it. Idempotent (ReplacingMergeTree key), safe to re-run.
- Tip unresolved: confirm the indexer is advancing (`ledgerstream` cursor in `ingestion_cursors`); fix upstream, then re-run.
- Verification: a clean run flips the metric to 0 and the alert clears within about 1 min of the next scrape. Confirm the watermark advanced toward tip: `ssh root@r1 'clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.supply_flows"'`.

False positive: a single failure during an intense re-derive that the next daily run heals. `for: 10m` rides out the run window but not a persistent failed state; a sustained failure is real.

Script: `configs/ansible/roles/archival-node/files/run-ch-supply.sh`; unit: `templates/systemd/ch-supply.service.j2`. Sibling failed-unit alert: [`verify-archive.md#stellarindex_verify_archive_unit_failed`](verify-archive.md#stellarindex_verify_archive_unit_failed). Architecture: `docs/architecture/storage-considerations.md#supply-flows-in-the-lake`.

## Shared context: `supply.md` alerts

_Source page `supply.md`: status living, severity P3, last verified 2026-10-06._

Rule file: `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `component: supply`; the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`). Severities: `circulating_zero` and `critical_stale` are `severity: page` (P2); the other three are `severity: ticket` (P3).

All five alerts watch the **systemd-timer path**: `supply-snapshot.timer` -> `supply-snapshot.service` -> `stellarindex-ops supply snapshot` (native XLM, daily). There is no wrapper script. The binary writes `/var/lib/node_exporter/textfile_collector/supply_snapshot.prom` itself (`internal/supply/textfile.go`, gated on `-textfile-output` / the unit's `TEXTFILE_OUTPUT` env; metric names and labels are defined there). On r1 `/etc/default/supply-snapshot` is ansible-managed with `TEXTFILE_OUTPUT` set (`10-observability.yml`); finding it unset is config drift to codify and fix.

The second producer of `asset_supply_history` is the aggregator-resident goroutine (`runSupplyRefresh` in `cmd/stellarindex-aggregator`, gated by `[supply] aggregator_refresh_enabled = true`, emits `stellarindex_aggregator_supply_refresh_total{outcome=...}`). It is tracked by [supply.md#stellarindex_aggregator_supply_refresh_stalled](supply.md#stellarindex_aggregator_supply_refresh_stalled) / `supply.md#stellarindex_aggregator_supply_refresh_error_dominant`, not by these alerts. A goroutine-only deployment cannot trip `unit_failed` or `circulating_zero` (no textfile, series absent); its equivalent of `unit_failed` is [`_error_dominant`](supply.md#stellarindex_aggregator_supply_refresh_error_dominant) (at least 50 % of refresher ticks with a non-`ok` outcome). On r1 BOTH paths are live (timer for native XLM; goroutine at 5 min cadence for the watched classic assets, `stellarindex.toml.j2` `[supply]` block). They are complementary: do NOT silence an alert here on the theory that the goroutine path covers it. Overview: [supply-pipeline.md](../../architecture/supply-pipeline.md).

Shared impact: `/v1/assets/{id}` F2 fields (total / circulating / max / market_cap_usd / fdv_usd) go stale or wrong.

Shared commands (r1 shapes):

```sh
ssh root@136.243.90.96
journalctl -u supply-snapshot.service -n 100 --output=cat
systemctl status supply-snapshot.timer; systemctl list-timers supply-snapshot.timer
systemctl start supply-snapshot.service        # force a one-off run
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

The writer also dials ClickHouse for the ledger's close time (`-ch-addr`, default `127.0.0.1:9300`); a down ClickHouse fails the run (`unit_failed` territory).

### At a glance

- [`stellarindex_supply_snapshot_unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert) - last run exited non-zero (P3, 30 m)
- [`stellarindex_supply_snapshot_stale`](#stellarindex_supply_snapshot_stale) - no success for > 36 h (P3)
- [`stellarindex_supply_snapshot_critical_stale`](#stellarindex_supply_snapshot_critical_stale) - no success for > 72 h (P2)
- [`stellarindex_supply_snapshot_never_initialized`](#stellarindex_supply_snapshot_never_initialized) - gauge absent for 36 h (P3)
- [`stellarindex_supply_snapshot_circulating_zero`](#stellarindex_supply_snapshot_circulating_zero) - XLM circulating <= 0 (P2)

## stellarindex_supply_snapshot_unit_failed_alert

`stellarindex_supply_snapshot_unit_failed > 0`, `for: 30m`, `severity: ticket`. Typical MTTR 15-30 min. Impact bounded: F2 fields keep serving the previous good value but go stale until the writer recovers.

The gauge is emitted by the subcommand's own `supplySnapshotMaybeEmitFailure` (`internal/ops/supply/supply.go` + `internal/supply/textfile.go`) on failure.

Symptoms:

- `stellarindex_supply_snapshot_unit_failed{asset_key=...} > 0` for 30 min or more.
- Latest `supply-snapshot.service` run in journald exited non-zero.
- `last_success_timestamp` for the asset is older than the 24 h daily cadence.

Diagnosis (5 min): shared commands above (journal, dry-run), then validate config:

```sh
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops docs-config | head   # parses cleanly?
grep -E "sdf_reserve_accounts|reserve_balances_stroops" /etc/stellarindex.toml
```

Root causes, roughly by frequency:

1. **Missing entry in `reserve_balances_stroops`, only when the fallback path is consulted.** `SupplyConfig.Validate` deliberately does not require a balance per `sdf_reserve_accounts` account (`internal/config/config.go`; the live AccountEntry observer may cover it and Validate has no DB access). The error fires at READ time, when the chained reader falls back to the static map for an observer-uncovered account (`internal/supply/config_reader.go`).
   - Signal: `supply: ConfigReserveBalanceReader: no balance configured for account G...`.
   - Fix: add the balance entry (bring-up fallback) or backfill the AccountEntry observer so the live path covers the account; re-run.
2. **Postgres unavailable.** `timescale.Open` or `InsertSupply` failed. Same flow as `postgres.md#stellarindex_timescale_connections_saturated`: confirm reachability and pool depth. A down ClickHouse fails the run the same way.
3. **No ingestion cursors yet.** Fresh box: `resolveSnapshotLedger` errors "no ingestion cursors yet - pass -ledger explicitly until the indexer has produced a cursor."
   - Fix: wait for the indexer's first cursor, or set `EXTRA_FLAGS="-ledger <known-good>"` in `/etc/default/supply-snapshot`.
4. **Operator config edit broke parsing** (trailing comma, mistyped key). Signal: `config:` prefix in the error. Fix the TOML, reload.

   > **Blind spot: a config parse error never trips THIS alert.** Config-load and flag errors return BEFORE the first failure-gauge emit (`supplySnapshotMaybeEmitFailure` is reachable only after `config.LoadWithEnv` + `cfg.Supply.Validate` succeed), so the unit exits non-zero without setting `unit_failed=1`. Nothing watches `node_systemd_unit_state` for this unit, so it surfaces only ~36 h later via [`_stale`](#stellarindex_supply_snapshot_stale) (or [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized) on a box that never succeeded). If you arrive from one of those, check journald for a `config:`-prefixed error first.

Mitigation: reproduce via diagnosis, apply the matching fix, `systemctl start supply-snapshot.service`. Verify `unit_failed` returns to 0 and `last_success_timestamp` updates.

False positive: first run after a fresh deploy, before the first daily fire; the 30 m `for` usually absorbs it.

## stellarindex_supply_snapshot_stale

`(time() - stellarindex_supply_snapshot_last_success_timestamp{asset_key=...}) > 36*3600`, `for: 5m`, `severity: ticket`, `alert_family: supply_snapshot_stale`. 36 h = 24 h cadence + 12 h cushion. Typical MTTR 15 min. Impact: F2 fields visibly old; `observed_at` more than a day behind chain state. This alert tracks only the timer-path gauge; see the shared intro for why it is real on r1.

Diagnosis (5 min):

```sh
systemctl status supply-snapshot.timer ; systemctl list-timers supply-snapshot.timer   # scheduled?
journalctl -u supply-snapshot.service --since "3 days ago" -n 50                       # last run?
ls -la /var/lib/node_exporter/textfile_collector/supply_snapshot.prom                  # textfile written?
systemctl start supply-snapshot.service                                                # force a run
```

Root causes:

1. **Timer disabled** (stopped for maintenance, not re-enabled). Fix: `systemctl enable --now supply-snapshot.timer`.
2. **Service unit failing every run.** Fires alongside [`unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert); follow that section.
3. **`TEXTFILE_OUTPUT` unset means this alert CANNOT be what fired.** With no textfile the series is absent and `time() - <missing>` is no data; that state belongs to [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized). On r1 an unset value is ansible drift to fix.
4. **Clock skew.** A backwards host clock jump makes a recent run look ancient. Signal: `node_time_seconds` deviates from real time. Fix ntp, then run the writer once with a fresh clock.

Mitigation: identify the silent stage (timer / unit / textfile), apply the fix, force a run. Verify `last_success_timestamp` updates within 60 s after a successful run reaches node_exporter.

False positive: none from a never-initialized gauge. An absent series evaluates to no data, so if `_stale` fired the gauge existed and a run HAS succeeded at some point. r1 sat in the absent state silently for 24+ days before the 2026-05-08 audit, which is why `_never_initialized` exists.

## stellarindex_supply_snapshot_critical_stale

Same expression and diagnosis as [`_stale`](#stellarindex_supply_snapshot_stale) with threshold `> 72*3600`, `for: 5m`, `severity: page` (P2), same `alert_family`. Three days without a snapshot makes F2 fields visibly old to customers; escalate per the SEV-3 playbook. It firing means `_stale` has been firing for 36 h unanswered: follow the `_stale` section (and `unit_failed_alert` if runs are failing).

## stellarindex_supply_snapshot_never_initialized

`absent_over_time(stellarindex_supply_snapshot_last_success_timestamp[36h]) == 1`, `for: 5m`, `severity: ticket`. Typical MTTR 10 min (one-shot operator action). Impact: F2 fields (circulating / total / max / market_cap_usd / fdv_usd) render as `null` for every asset.

Why separate from `_stale`: `time() - <missing>` is no data, not infinity, so a deployment that never wrote a snapshot is invisible to `_stale`. The 36 h window matches `_stale`'s cushion so a fresh install awaiting its first daily fire does not false-positive. The aggregator alert `stellarindex_aggregator_supply_refresh_never_initialized` (`rules.r1/supply-refresh.yml`) is the goroutine-path sibling; its section lives in [supply.md#stellarindex_aggregator_supply_refresh_never_initialized](supply.md#stellarindex_aggregator_supply_refresh_never_initialized).

Symptoms:

- Annotation: "supply snapshot has never published - pipeline uninitialized."
- `/v1/assets/native` and `/v1/assets/xlm` omit `circulating_supply` / `market_cap_usd` (omitted when null per the wire-shape contract).
- `psql ... -c "SELECT count(*) FROM asset_supply_history"` returns 0.

Diagnosis (5 min):

```sh
systemctl is-enabled supply-snapshot.timer     # "not-found" = never installed; "enabled" = installed
systemctl status supply-snapshot.timer --no-pager
systemctl status supply-snapshot.service --no-pager   # most recent run
grep -A 1 'aggregator_refresh_enabled' /etc/stellarindex.toml   # absent/false = goroutine path off
journalctl -u stellarindex-aggregator --since '2 days ago' | grep -E 'supply-refresh|supply.*ok|supply.*err'
```

Resolution. Only the ops-CLI textfile writer (Path A) emits `last_success_timestamp`, so only Path A clears this alert. Both paths populate `asset_supply_history`; r1 runs both (see shared intro), so they are not mutually exclusive.

Path A, systemd timer (daily):

```sh
sudo cp deploy/systemd/supply-snapshot.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload

# CRITICAL: wire the textfile output BEFORE starting. The shipped unit defaults
# `Environment=TEXTFILE_OUTPUT=` (EMPTY, no metrics emitted). Started as-is the
# snapshot writes to Postgres but never publishes last_success_timestamp, so this
# alert never clears and the verify grep below fails on a working writer.
cat <<'EOT' | sudo tee /etc/default/supply-snapshot
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/supply_snapshot.prom
EOT

sudo systemctl enable --now supply-snapshot.timer
sudo systemctl start supply-snapshot.service
sudo journalctl -u supply-snapshot.service --no-pager -n 50
# Expect: `Wrote snapshot for asset_key=XLM ledger=<N> basis=<...>` and zero exit.
curl -s http://localhost:9100/metrics | grep stellarindex_supply_snapshot_last_success_timestamp
# Expect one line with a recent unix timestamp (after node_exporter rescrapes the .prom).
```

The alert clears within 5 min of a successful first run.

Path B, aggregator goroutine (sub-minute cadence capable):

```sh
# /etc/stellarindex.toml
[supply]
aggregator_refresh_enabled = true
# optional: aggregator_refresh_cadence = "5m" (default)

sudo systemctl restart stellarindex-aggregator
sudo journalctl -u stellarindex-aggregator -f | grep -E 'supply-refresh'     # tick within one cadence
curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total
# Expect at least one outcome="ok" line per watched asset_key.
```

Path B does NOT silence this alert. It clears the sibling aggregator `_never_initialized` alert and populates the goroutine-path metrics tracked by [supply.md#stellarindex_aggregator_supply_refresh_stalled](supply.md#stellarindex_aggregator_supply_refresh_stalled). If a deployment intentionally runs Path B exclusively, silence this textfile-path alert; `_stale` cannot fire there (its series is absent), so no silence is needed for it.

Why neither path is the default: the supply pipeline ships dormant by design. The operator-managed `reserve_balances_stroops` config is the source of truth for SDF reserves (subtracted from total to get circulating); without it the writer would emit nonsense, so the gate forces operator review before first publish. Wiring guide: [docs/operations/supply.md](../supply-snapshot.md).

Verify (within `max(36 h, aggregator_refresh_cadence)`):

```sh
sudo -u postgres psql -d stellarindex -c \
  "SELECT asset_key, count(*) AS rows, max(time) AS latest
   FROM asset_supply_history GROUP BY asset_key ORDER BY asset_key"
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data.circulating_supply'   # numeric string, not null
```

`internal/supply/refresher.go`: the `OutcomeKindMissingFreshness` outcome surfaces when the opt-in `[supply].strict_freshness_required` is enabled.

## stellarindex_supply_snapshot_circulating_zero

`stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"} <= 0`, `for: 5m`, `severity: page` (P2). Typical MTTR 15-60 min. Impact: `/v1/assets/native` reports `circulating_supply: 0`, a customer-visible data-quality incident. Timer-path-only (live on r1): the gauge comes from `internal/supply/textfile.go`; an absent series is owned by [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized). This alert presumes the writer ran successfully but produced a wrong value; writer failures are [`unit_failed`](#stellarindex_supply_snapshot_unit_failed_alert).

Per ADR-0011 native XLM circulating = total - sum(SDF reserves). Non-positive means either the reserve-balance sum (live observer OR static fallback) equals or exceeds the frozen total, or the XLMComputer math is broken (regression).

Diagnosis (5 min). The writer reads reserve balances from the live LCM AccountEntry observer FIRST (chained-fallback reader, L2.12a, PRs #411-#413), so check the database before the TOML:

```sh
# 1. Latest snapshot (flags before the positional; Go's flag package stops at the first positional)
stellarindex-ops supply audit -config /etc/stellarindex.toml native

# 2. Which reserve source did the writer use? The reader consults the
#    account_observations hypertable first; the TOML map is used only when at
#    least one watched account has NO observation at-or-before the snapshot
#    ledger (the whole call then drops to the static map, no mixing).
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT account_id, max(ledger) AS latest_obs,
          (SELECT balance_stroops FROM account_observations b
            WHERE b.account_id = a.account_id
            ORDER BY ledger DESC LIMIT 1) AS latest_balance
     FROM account_observations a
    WHERE account_id IN (SELECT unnest(ARRAY['G...','G...']))  -- paste sdf_reserve_accounts
    GROUP BY account_id;"

# 3. ONLY if step 2 shows an uncovered account (fallback in play): sum the TOML
#    balances vs the frozen total. A wrong TOML value is harmless while the
#    observer covers every account.
grep -A 100 "^\[supply" /etc/stellarindex.toml
python3 -c "
import re, sys
content = open('/etc/stellarindex.toml').read()
balances = re.findall(r'^\\s*\"?([A-Z0-9]+)\"?\\s*=\\s*\"?(\\d+)\"?', content, re.M)
total = sum(int(b) for _, b in balances if len(_) == 56)
print(f'sum of reserve balances: {total} stroops = {total/1e7:.2f} XLM')
print(f'frozen total:           500018068120000000 stroops = 50,001,806,812.00 XLM')
print(f'difference:             {500018068120000000 - total} stroops')
"

# 4. Dry-run to confirm reproduction
stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

Root causes:

1. **Corrupt or inflated observer balances.** The live `account_observations` rows are summed first (`internal/supply/config_reader.go`, chained reader in `internal/ops/supply/supply.go`). A decode/backfill bug inflating a reserve's `balance_stroops` drives circulating <= 0 with a correct TOML.
   - Signal: step 2 `latest_balance` implausibly large vs stellar.expert.
   - Fix: file a P2 against the AccountEntry observer; as a stopgap correct the rows from chain state and re-run the writer.
2. **Reserve balance overstated in the TOML fallback** (only with an uncovered account). Operator copied an SDF value with the wrong scale (USD or XLM instead of stroops, inflating by 10^7). Signal: step 3 reserve total ~ 10^7 x frozen total. Fix: divide the entry by 10^7, re-run.
3. **All-reserve config.** `sdf_reserve_accounts` includes the issuer or a payment account; poisons BOTH paths since the observer sums whatever the list names. Signal: extra G-strkeys vs SDF's announcement. Fix: remove them.
4. **XLMComputer bug** (should not happen; algorithm is trivial). Signal: `supply snapshot -dry-run` gives the same wrong value with verified-correct inputs on both reserve paths. Fix: roll back the writer binary; file a P2 bug.

Mitigation: identify cause (observer coverage FIRST, TOML only if fallback is in play); observer-data error: correct/re-derive the affected `account_observations` rows and file the observer bug; config error: fix TOML and force a run; algorithm bug: roll back. In every case verify `circulating_supply > 0` on the next snapshot; the alert clears within 5 min of a corrected snapshot.

False positive: none realistic. A zero would be correct only if every XLM were burned; ADR-0011's zero-is-a-valid-answer note does not apply to native XLM (hard-capped, indestructible by design).

## stellarindex_supply_verify_rollup_stale

**Runbook — `stellarindex_supply_verify_rollup_stale` / `_never_initialized`**

_Source page `supply.md#stellarindex_supply_verify_rollup_stale`: status draft, severity P3, last verified 2026-09-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_verify_rollup_stale`, `stellarindex_supply_verify_rollup_never_initialized` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-verify-rollup.yml`; multi-host twin in `deploy/monitoring/rules/supply-verify-rollup.yml`. |
| Typical MTTR | 15 min |
| Impact | No customer impact by itself: the SEP-41 supply rollup is simply no longer certified to reconcile with its source events. |

### Symptoms

- `_stale`: `stellarindex_supply_verify_rollup_last_success_timestamp` is
  older than 36 h (daily cadence + 12 h). Every run since has failed, or the
  timer stopped.
- `_never_initialized`: the series has not existed for 36 h. The timer was
  never installed, node_exporter is not reading the textfile directory, or no
  run has ever passed clean — a failed run writes no
  `last_success_timestamp`.

### Quick diagnosis (≤ 5 min)

```sh
systemctl list-timers supply-verify-rollup.timer
systemctl status supply-verify-rollup.service
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
```

- Timer absent: the archival-node role has not been applied since the unit
  landed. Apply with `--tags ops-jobs`.
- File present with `unit_failed 1`: follow
  [supply-verify-rollup-unit-failed](supply.md#stellarindex_supply_verify_rollup_unit_failed_alert).
- File absent after a run: check `TEXTFILE_OUTPUT` in
  `/etc/default/supply-verify-rollup` and the unit's `ReadWritePaths`.

### Mitigation

- [ ] Get one clean run: `systemctl start supply-verify-rollup.service`.
- [ ] Verification: `last_success_timestamp` advances and both alerts clear
      within one scrape plus `for: 5m`.

## stellarindex_supply_verify_rollup_unit_failed_alert

_Source page `supply.md#stellarindex_supply_verify_rollup_unit_failed_alert`: status draft, severity P3, last verified 2026-09-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_verify_rollup_unit_failed_alert` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-verify-rollup.yml` (group `stellarindex.supply_verify_rollup`, `for: 30m`); multi-host twin in `deploy/monitoring/rules/supply-verify-rollup.yml`. |
| Typical MTTR | 30–60 min (a fold rebuild runs one contract at a time) |
| Impact | Served SEP-41 supply (`/v1/assets/{id}` and `/supply`) for a drifted contract is read from a fold that no longer matches its source events, so it can be wrong until the fold is rebuilt. |

### Why this exists

`stellarindex-ops supply verify-rollup` re-sums `sep41_supply_events` for
each watched contract at its `sep41_supply_rollup` checkpoint and diffs that
against the fold. It runs daily from `supply-verify-rollup.timer` and writes
`/var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom`, so
"the rollup reconciles" is a scraped fact rather than a pasted transcript.

### Symptoms

`stellarindex_supply_verify_rollup_unit_failed == 1`. The run failed for one
of four reasons, told apart by the gauges it wrote:

| `drift_total` | `missing_total` | `checked_total` | Cause |
| --- | --- | --- | --- |
| > 0 | any | > 0 | A checkpoint diverges from the re-sum. |
| 0 | > 0 | any | A watched contract has no `sep41_supply_rollup` row — unexamined, not clean. |
| 0 | 0 | 0 | Nothing was checked (empty rollup table, or `-contracts` matched nothing). |
| absent | absent | absent | The run errored before counting (config, Postgres, timeout). |

### Quick diagnosis (≤ 5 min)

```sh
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
journalctl -u supply-verify-rollup.service -n 100 --output=cat
```

The journal names every drifted `(contract, kind)` and every missing contract.

### Mitigation

- **Drift:** rebuild the named contracts' fold. `stellarindex-ops supply
  seed-sep41-genesis -config /etc/stellarindex.toml -write` rebuilds the fold
  under the genesis baseline in one transaction per contract (the W5.4
  procedure in [v1-launch-plan.md](../v1-launch-plan.md)). Then re-run
  `systemctl start supply-verify-rollup.service` and confirm
  `unit_failed 0`.
- **Missing:** the contract is in `[supply] watched_sep41_contracts` but has
  never been folded. Check the rollup worker first —
  [sep41-supply-rollup-no-cursor](supply.md#stellarindex_sep41_supply_rollup_no_cursor) — then
  seed it as above.
- **Nothing checked:** the role installs this job only where
  `stellarindex_watched_sep41_contracts` is non-empty
  (`supply_verify_rollup_enabled`), so the watched set has no rollup rows at
  all. Treat as missing.
- **Error:** fix the cause in the journal; a timeout means raising
  `RUN_TIMEOUT` in `/etc/default/supply-verify-rollup` (and
  `TimeoutStartSec` in the unit with it).

### Known false-positive patterns

None known. A failed run is never promoted to clean.

## Related

**stellarindex_supply_cross_check_divergence**

- ADR-0011 §"SAC-wrapped classics — both algorithms must agree" —
  the ORIGINAL policy this runbook implemented. Superseded in
  practice (not in ADR text — ADRs are immutable) by the 2026-07-08
  `WrapClass` fix above; ADR-0011 itself is not amended because the
  original equality invariant is still correct for a `full_wrap` pair.
- [`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
  — overview of the three-algorithm split, the six observers,
  and where the cross-check fits.
- [`docs/reference/metrics/README.md`](../../reference/metrics/README.md)
  — `wrap_class` label semantics on both cross-check metrics.
- [`supply-cross-check-unevaluable`](supply.md#stellarindex_supply_cross_check_unevaluable)
  — sibling alert for pairs the refresher cannot evaluate
  (`missing_snapshot` / `read_error` / `misaligned`); their gauge
  series is deleted, so this alert is silent for them.
- `aggregator.md#stellarindex_aggregator_silent` — if the aggregator is stalled, the
  cross-check gauge is also stale; investigate that first.
- `supply.md#stellarindex_aggregator_supply_refresh_stalled` / `supply.md#stellarindex_aggregator_supply_refresh_error_dominant`
  — when the refresher itself isn't producing snapshots; both
  algorithm readings would be stale rather than divergent.
- `supply.md#stellarindex_supply_snapshot_stale` — sibling alert on the systemd-timer
  path; if it's also firing, the alternative-path producer is
  down too.
- `internal/supply/crosscheck.go` — the comparison code (`CrossCheck`,
  `CrossCheckSubsetBound`, `CrossCheckForClass`, `WrapClass`); any
  tolerance or invariant change must update this runbook.

**stellarindex_supply_cross_check_unevaluable**

- [`supply-cross-check-divergence`](supply.md#stellarindex_supply_cross_check_divergence) —
  the alert this one keeps honest.
- [`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
  — the outcome table for the cross-check counter.
- `internal/supply/crosscheck_refresher.go` — `Tick` and the outcome
  kinds. `cmd/stellarindex-aggregator/main.go::crossCheckPairs` builds
  the pair set. It rejects a wrapper that is not the classic asset's
  derived SAC at startup.

**stellarindex_supply_divergence_high**

- [`docs/methodology/xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md)
  — the authoritative methodology + live reconciliation. Read this
  FIRST; it is the source of truth this alert defends.
- [ADR-0011](../../adr/0011-supply-algorithm.md) — the three-algorithm
  supply spec (XLM is Algorithm 1).
- [`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
  — observer → reader → snapshot → API data flow; where the reserve
  balances are read from.
- [`supply.md#stellarindex_supply_cross_check_divergence`](supply.md#stellarindex_supply_cross_check_divergence)
  — the INTERNAL consistency sibling (classic vs SAC), not this
  external cross-check.
- [`entry-walk-renumbering.md`](entry-walk-renumbering.md) — the
  deploy-time procedure after a `dispatcher.EntryWalkVersion` bump. A
  skipped repair leaves stale balance observations that this alert will
  eventually surface as a widening divergence, and a plain re-derive will
  NOT fix them (the `intra_ledger_seq` guard silently drops the
  correction).
- `internal/divergence/supply.go` — the worker; the reference clients
  + threshold live here.

**`supply.md` alerts**

- [`supply.md#stellarindex_supply_snapshot_never_initialized`](supply.md#stellarindex_supply_snapshot_never_initialized): the timer-path never-initialized alert (the sibling of `_never_initialized` here).
- `supply.md#stellarindex_supply_snapshot_stale`: systemd-timer-path equivalent (different metric, different expectation).
- ADR-0011 (three-domain supply algorithm), ADR-0021, ADR-0022, ADR-0023: algorithms and observer designs the refresher consumes.

**`supply.md` alerts**

- `supply.md#stellarindex_supply_cross_check_divergence` - when the value itself looks wrong (classic vs SAC divergence).
- `postgres.md#stellarindex_timescale_connections_saturated` - Postgres reachability.
- [supply.md#stellarindex_aggregator_supply_refresh_stalled](supply.md#stellarindex_aggregator_supply_refresh_stalled), [supply.md#stellarindex_aggregator_supply_refresh_error_dominant](supply.md#stellarindex_aggregator_supply_refresh_error_dominant) - goroutine-path counterparts.
- `archive.md#stellarindex_archive_completeness_stale` - same shape on the archive side.
- [supply-pipeline.md](../../architecture/supply-pipeline.md) (incl. "The chained-fallback reader pattern"), [ADR-0011](../../adr/0011-supply-algorithm.md) (Algorithm 1, native XLM), ADR-0021 (chained-fallback reserve reader).

**stellarindex_supply_verify_rollup_stale**

- Companion: [supply-verify-rollup-unit-failed](supply.md#stellarindex_supply_verify_rollup_unit_failed_alert).
- Sibling pattern: [supply-snapshot-stale](supply.md#stellarindex_supply_snapshot_stale),
  [supply-snapshot-never-initialized](supply.md#stellarindex_supply_snapshot_never_initialized).

**stellarindex_supply_verify_rollup_unit_failed_alert**

- Implementation: `internal/ops/supply/supply_verify_rollup.go`,
  `internal/supply/textfile.go`.
- Unit: `configs/ansible/roles/archival-node/templates/systemd/supply-verify-rollup.{service,timer}.j2`.
- Companion: [supply-verify-rollup-stale](supply.md#stellarindex_supply_verify_rollup_stale) —
  no clean run for 36 h, or never.
- [sep41-supply-rollup-no-cursor](supply.md#stellarindex_sep41_supply_rollup_no_cursor) — the
  fold is not advancing at all.
