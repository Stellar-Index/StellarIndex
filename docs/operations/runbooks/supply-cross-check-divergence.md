---
title: Runbook — supply-cross-check-divergence
last_verified: 2026-07-08
status: living
severity: P3
---

# Runbook — `stellarindex_supply_cross_check_divergence`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_cross_check_divergence` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml` |
| Typical MTTR | 1 – 4 hours (RCA-driven; not user-impacting on its own) |
| Impact | The asset's `total_supply` / `circulating_supply` / `market_cap_usd` / `fdv_usd` on `/v1/assets/{id}` will be wrong by the divergence amount until reconciled. Customer-visible only on the affected asset's detail page; aggregate price endpoints are unaffected. |

## 2026-07-08 category-error fix (BACKLOG #59) — read this first

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
  per ADR-0011/0022's own math IS a true equality (both measure the
  same wrapped amount via independent data paths: a ledger-entry
  snapshot sum vs an event-flow sum) — is **not available at this
  compare site today**. `ClassicComputer.Compute` folds `SACWrapped`
  into the classic `TotalSupply` before returning a `Supply`, and
  only that folded total is persisted to `asset_supply_history` (the
  table `CrossCheckRefresher` reads). Wiring the real subset compare
  needs either a `sac_wrapped_stroops` column + `Supply` field +
  `ClassicComputer.Compute` populating it, or the refresher querying
  `ClassicSupplyStore.SumSACBalancesAtOrBefore` directly instead of
  the persisted snapshot. Real schema/plumbing work — tracked as a
  follow-up, not implemented as an approximation.
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

**Known limitation:** the subset-bound check assumes Algorithm 2's
classic total isn't itself an undercount. PHO/BLND are a documented,
*separate* case where Algorithm 2 undercounts pool-internal SAC
balances held in non-standard `contract_data` keys (not SEP-41
`Balance` entries) — see `docs/architecture/supply-pipeline.md`. If
such an undercounted pair is ever added to `sac_wrappers`, the
subset-bound check could false-positive in the `sac_total >
classic_total` direction. This is NOT a regression from this fix
(the old equality check was already broken for such a pair too), but
if you see this alert with `wrap_class="partial_wrap"` on a pair with
known pool-internal SAC balances, check that structural gap before
assuming fresh corruption.

## Symptoms

- `stellarindex_supply_cross_check_divergence_stroops{classic_key="...",wrap_class="..."} > 1` for ≥ 5 min.
- The labelled `classic_key` identifies the affected asset (format `CODE:ISSUER`); `wrap_class` (`partial_wrap` | `full_wrap`) identifies which invariant fired — see above.
- `stellarindex_supply_cross_check_total{outcome="over",wrap_class="..."}` rate non-zero.

## Background — what each algorithm measures, and what "agree" means now

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

## Quick diagnosis (≤ 15 min)

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

> **Periodic gauge emission is wired into the aggregator's supply-refresh loop** when `[supply].aggregator_refresh_enabled = true`. Every `aggregator_refresh_cadence` tick, `supply.CrossCheckRefresher` (built in `cmd/stellarindex-aggregator/main.go::buildCrossCheckRefresher`) loads the latest classic + SAC snapshots for every classic asset that's both in `watched_classic_assets` AND has its SAC contract id declared in `sac_wrappers` AND that contract id is also in `watched_sep41_contracts`. The intersection is the cross-check pair set — outside it the supply package can't compare, so the refresher silently skips it. The CLI `stellarindex-ops supply audit <asset> -cross-check <counterpart>` path remains available for ad-hoc operator inspection but is no longer the gauge-emission path.

Decision tree (2026-07-08: read `wrap_class` off the firing series first — it changes which row applies):

| wrap_class | Direction | Likely cause | Mitigation |
| ---------- | --------- | ------------ | ---------- |
| `partial_wrap` (the common case) | SAC > Classic (the ONLY direction that can fire for this class) | Algorithm 2 undercounted classic supply (missed a trustline / claimable / LP / SAC-balance entry — including the known PHO/BLND-shaped pool-internal-SAC-balance gap, see above) OR Algorithm 3 double-counted a mint | Replay the affected ledger range through the trustline-delta indexer (Algorithm 2 undercounted, the more common case); if that doesn't close the gap, check Algorithm 3's mint-event replay for a duplicate |
| `partial_wrap` | Classic > SAC | **Not possible — this alert cannot fire in this direction for `partial_wrap`.** If you somehow see this, the metric computation itself regressed; check `internal/supply/crosscheck.go`'s `CrossCheckSubsetBound` first, not the data |
| `full_wrap` (operator-attested; none configured as of 2026-07-08) | Classic > SAC | Algorithm 3 missed mint events (rare — events are durable) | Replay the SAC contract's event range from Galexie; rerun Algorithm 3 |
| `full_wrap` | Classic < SAC | Algorithm 2 missed a trustline / claimable / LP entry change | Replay the affected ledger range through the trustline-delta indexer; rerun Algorithm 2 |
| either | Both readings stale | Aggregator orchestrator stalled; cross-check is comparing old data | Check `stellarindex_aggregator_silent` runbook first |

## Mitigation (≤ 60 min)

- [ ] **Identify which side is wrong** with the audit subcommand
      (#233). Pass the classic asset; supply the SAC counterpart
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
      `|| operator-escalate` if scripting.

- [ ] **Replay the affected range.** Per-algorithm replay
      subcommands aren't shipped yet — the operator path today is
      restarting the indexer with a config override that re-reads
      the ledger window. See `cmd/stellarindex-indexer` flags.

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

## Root cause analysis

Capture for the postmortem:

- The first ledger at which the two readings diverged. (Walk
  `asset_supply_history` backward from the alert-firing time.)
- The replay-range commands you ran + the divergence-after value.
- Which indexer was at fault (Algorithm 2's trustline-delta vs.
  Algorithm 3's event-sum).
- If the corruption was caused by a recent code change: the PR diff
  + the audit log.

## Known false-positive patterns

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
- **Clock skew between processes**: if the cross-checker's
  Algorithm-2 read happens at ledger N and Algorithm-3 read at
  ledger N+1, a fresh mint between them looks like divergence. For
  `wrap_class="partial_wrap"` this only matters in the SAC-ahead
  direction (Algorithm 3 observing a mint Algorithm 2 hasn't caught
  up to yet); the aggregator orchestrator pins both reads to the same
  ledger boundary, so a regression that breaks that pinning would
  surface as a chronic 1-2-stroop noise floor.

## Related

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
- `aggregator-silent.md` — if the aggregator is stalled, the
  cross-check gauge is also stale; investigate that first.
- `supply-refresh-stalled.md` / `supply-refresh-error-dominant.md`
  — when the refresher itself isn't producing snapshots; both
  algorithm readings would be stale rather than divergent.
- `supply-snapshot-stale.md` — sibling alert on the systemd-timer
  path; if it's also firing, the alternative-path producer is
  down too.
- `internal/supply/crosscheck.go` — the comparison code (`CrossCheck`,
  `CrossCheckSubsetBound`, `CrossCheckForClass`, `WrapClass`); any
  tolerance or invariant change must update this runbook.

## Changelog

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
