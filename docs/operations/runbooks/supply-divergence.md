---
title: Runbook — supply-divergence
last_verified: 2026-07-07
status: living
severity: P3
---

# Runbook — `stellarindex_supply_divergence_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_divergence_high` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml` |
| Typical MTTR | 1 – 3 hours (config-update-driven; not user-impacting on its own) |
| Impact | `/v1/assets/native` (or the affected asset) reports a `circulating_supply` — and the `market_cap_usd` derived from it — that differs from the market's authoritative figure by more than 1%. Customer-visible on the affected asset's detail page; aggregate price endpoints are unaffected. |

## What this alert is (and is NOT)

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
[`stellarindex_supply_cross_check_divergence`](supply-cross-check-divergence.md),
which compares two of OUR OWN numbers (a classic asset's Algorithm 2
ledger-entry sum vs its SAC-wrapped Algorithm 3 event sum). This alert
is our number vs the world's.

## Symptoms

- `stellarindex_supply_divergence_ratio{asset="native",reference="stellar-dashboard"} > 0.01`
  for ≥ 1 h.
- `stellarindex_supply_divergence_total{outcome="divergent"}` rate
  non-zero.
- The `asset` + `reference` labels identify which figure and which
  reference disagree.

## Background — why 1%, not 0.03%

Our served XLM circulating (`total − Σ SDF-mandate − upgrade-reserve`)
tracks the Dashboard's (`total − mandate − upgrade-reserve − fee-pool`)
to within **~0.03%** — the entire residual is the Fee Pool (~10.1 M
XLM), which we deliberately do not subtract (a sub-basis-point
correction). See the methodology doc's live reconciliation table. The
1% alert threshold sits **two-plus orders of magnitude above** that
noise floor, so it fires only on a genuine drift, never on the
structural Fee-Pool delta.

## The two real causes (in likelihood order)

1. **A stale SDF-reserve exclusion account list (most likely).** SDF
   distributed lumens from — or added — an account that
   `stellarindex_sdf_reserve_accounts` no longer tracks accurately. Our
   circulating is then too LOW (we're still excluding distributed
   lumens) or too HIGH (SDF added an undistributed account we don't
   exclude). **This is the drift this alert exists to catch.**
2. **The reference changed methodology.** The Dashboard (or CoinGecko)
   redefined what it counts as circulating. Rare, but possible.

## Quick diagnosis (≤ 15 min)

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

## Mitigation (≤ 60 min) — do NOT blindly change our number

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
      the change lands in `configs/ansible/` in the same PR (CLAUDE.md
      "r1 configuration is ansible-managed").
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

## Known non-firing / graceful-degrade behaviour

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

## Root cause analysis

Capture for the postmortem:

- Which account's balance moved (the `sdfMandate` component delta).
- The reserve-list PR diff + the `ratio`-after value.
- Whether the drift was a real SDF distribution (update the list) or a
  reference methodology change (update the methodology doc).

## Related

- [`docs/methodology/xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md)
  — the authoritative methodology + live reconciliation. Read this
  FIRST; it is the source of truth this alert defends.
- [ADR-0011](../../adr/0011-supply-algorithm.md) — the three-algorithm
  supply spec (XLM is Algorithm 1).
- [`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
  — observer → reader → snapshot → API data flow; where the reserve
  balances are read from.
- [`supply-cross-check-divergence.md`](supply-cross-check-divergence.md)
  — the INTERNAL consistency sibling (classic vs SAC), not this
  external cross-check.
- `internal/divergence/supply.go` — the worker; the reference clients
  + threshold live here.

## Changelog

- 2026-07-07 — initial version, shipped alongside the supply-divergence
  cross-check worker (Stellar Dashboard + CoinGecko references +
  `stellarindex_supply_divergence_ratio` / `_total` / `_duration_seconds`).
